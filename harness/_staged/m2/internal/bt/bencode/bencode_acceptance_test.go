package bencode

// Acceptance tests for the pinned bencode API (spec/contracts/go-api.md).
// Owned by the verifier role: implementers must not edit this file.

import (
	"bytes"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

type accFile struct {
	Length int64    `bencode:"length"`
	Path   []string `bencode:"path"`
}

type accInfo struct {
	Name        string    `bencode:"name"`
	Length      int64     `bencode:"length,omitempty"`
	PieceLength int64     `bencode:"piece length"`
	Pieces      []byte    `bencode:"pieces"`
	Private     int       `bencode:"private,omitempty"`
	Files       []accFile `bencode:"files,omitempty"`
	Ignored     string    `bencode:"-"`
}

func TestRoundTrip(t *testing.T) {
	marshal := []struct {
		in   any
		want string
	}{
		{int64(0), "i0e"},
		{-42, "i-42e"},
		{uint32(7), "i7e"},
		{"spam", "4:spam"},
		{[]byte{}, "0:"},
		{[]byte("\x00\xff"), "2:\x00\xff"},
		{[]any{"a", int64(1)}, "l1:ai1ee"},
		{[]string{}, "le"},
		{map[string]any{"b": 1, "a": "x"}, "d1:a1:x1:bi1ee"},
		{map[string]int{}, "de"},
		{accInfo{Name: "x", Length: 5, PieceLength: 16384, Pieces: []byte("abc"), Ignored: "no"},
			"d6:lengthi5e4:name1:x12:piece lengthi16384e6:pieces3:abce"},
		{accInfo{Name: "m", PieceLength: 1, Pieces: []byte{}, Private: 1,
			Files: []accFile{{Length: 1, Path: []string{"a", "b"}}}},
			"d5:filesld6:lengthi1e4:pathl1:a1:beee4:name1:m12:piece lengthi1e6:pieces0:7:privatei1ee"},
	}
	for _, c := range marshal {
		got, err := Marshal(c.in)
		if err != nil {
			t.Errorf("Marshal(%#v): %v", c.in, err)
			continue
		}
		if string(got) != c.want {
			t.Errorf("Marshal(%#v) = %q, want %q", c.in, got, c.want)
		}
	}

	// Struct round trip.
	orig := accInfo{Name: "multi", PieceLength: 32768, Pieces: bytes.Repeat([]byte{1}, 20), Private: 1,
		Files: []accFile{{Length: 10, Path: []string{"dir", "a.txt"}}, {Length: 0, Path: []string{"b"}}}}
	enc, err := Marshal(orig)
	if err != nil {
		t.Fatal(err)
	}
	var back accInfo
	if err := UnmarshalStrict(enc, &back); err != nil {
		t.Fatalf("UnmarshalStrict(Marshal(x)): %v", err)
	}
	if !reflect.DeepEqual(orig, back) {
		t.Fatalf("round trip changed the value:\n got %#v\nwant %#v", back, orig)
	}

	// Decoding into any.
	var v any
	if err := Unmarshal([]byte("d1:ai1e1:bl1:xi-3eee"), &v); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"a": int64(1), "b": []any{"x", int64(-3)}}
	if !reflect.DeepEqual(v, want) {
		t.Fatalf("decode into any = %#v, want %#v", v, want)
	}

	// A bencode string decodes into both string and []byte fields.
	var s struct {
		A string `bencode:"a"`
		B []byte `bencode:"b"`
	}
	if err := Unmarshal([]byte("d1:a2:hi1:b2:yoe"), &s); err != nil || s.A != "hi" || string(s.B) != "yo" {
		t.Fatalf("string/[]byte decode = %+v, %v", s, err)
	}
}

var strictBad = []string{
	"i03e", "i-0e", "i-e", "ie", "i01e", "i-01e", "i1.5e",
	"03:abc",
	"d1:bi1e1:ai2ee",      // unsorted keys
	"d1:ai1e1:ai2ee",      // duplicate key
	"d1:ad1:zi1e1:yi2eee", // unsorted keys in a nested dict
	"i1ex", "d1:ai1ee ",   // trailing data
}

func TestStrictRejects(t *testing.T) {
	for _, in := range strictBad {
		var v any
		if err := UnmarshalStrict([]byte(in), &v); err == nil {
			t.Errorf("UnmarshalStrict(%q) succeeded, want an error", in)
		}
	}
	for _, in := range []string{"i0e", "i-1e", "i42e", "0:", "le", "de", "d1:ai1e1:bi2ee", "d2:aai1e1:bi2ee"} {
		var v any
		if err := UnmarshalStrict([]byte(in), &v); err != nil {
			t.Errorf("UnmarshalStrict(%q) = %v, want success", in, err)
		}
	}
}

func TestLenient(t *testing.T) {
	var v any
	if err := Unmarshal([]byte("d1:bi1e1:ai2ee"), &v); err != nil {
		t.Fatalf("Unmarshal must accept unsorted keys: %v", err)
	}
	if want := map[string]any{"a": int64(2), "b": int64(1)}; !reflect.DeepEqual(v, want) {
		t.Fatalf("got %#v, want %#v", v, want)
	}
	for _, in := range []string{"", "i1", "l", "d", "d1:a", "x", "5:abc", "di1ei2ee", "i1ex", "le1"} {
		var v any
		if err := Unmarshal([]byte(in), &v); err == nil {
			t.Errorf("Unmarshal(%q) succeeded, want an error", in)
		}
	}
}

func TestRawMessagePreserved(t *testing.T) {
	type meta struct {
		Announce string     `bencode:"announce"`
		Info     RawMessage `bencode:"info"`
	}
	// The info dict contains a key the struct does not know and, in the
	// lenient case, unsorted keys: the raw bytes must survive untouched.
	for _, in := range []string{
		"d8:announce3:url4:infod6:lengthi1e4:name1:x7:x-extra3:yesee",
		"d8:announce3:url4:infod4:name1:x6:lengthi1eee",
	} {
		var m meta
		if err := Unmarshal([]byte(in), &m); err != nil {
			t.Fatalf("Unmarshal(%q): %v", in, err)
		}
		start := strings.Index(in, "4:info") + len("4:info")
		wantInfo := in[start : len(in)-1]
		if string(m.Info) != wantInfo {
			t.Errorf("RawMessage = %q, want %q", m.Info, wantInfo)
		}
		out, err := Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		if string(out) != in {
			t.Errorf("Marshal = %q, want the original %q", out, in)
		}
	}
}

func TestLimits(t *testing.T) {
	ok := strings.Repeat("l", 64) + strings.Repeat("e", 64)
	var v any
	if err := Unmarshal([]byte(ok), &v); err != nil {
		t.Errorf("nesting depth 64 must be accepted: %v", err)
	}
	deep := strings.Repeat("l", 65) + strings.Repeat("e", 65)
	if err := Unmarshal([]byte(deep), &v); err == nil {
		t.Error("nesting depth 65 must be rejected")
	}
	veryDeep := strings.Repeat("l", 1_000_000)
	if err := Unmarshal([]byte(veryDeep), &v); err == nil {
		t.Error("a million nested lists must be rejected (and must not overflow the stack)")
	}

	for _, in := range []string{"1073741824:abc", "99999999999999999999:x", "d3:keyl1073741824:abcee"} {
		var before, after runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&before)
		err := Unmarshal([]byte(in), &v)
		runtime.ReadMemStats(&after)
		if err == nil {
			t.Errorf("Unmarshal(%q) succeeded, want an error", in)
		}
		if grew := after.TotalAlloc - before.TotalAlloc; grew > 64<<20 {
			t.Errorf("Unmarshal(%q) allocated %d MiB before failing; lengths must be checked first", in, grew>>20)
		}
	}
}

func FuzzDecode(f *testing.F) {
	for _, s := range append(strictBad,
		"i0e", "4:spam", "l4:spami42ee", "d3:bar4:spam3:fooi42ee", "d4:infod6:lengthi1e4:name1:xee",
		"d1:bi1e1:ai2ee", strings.Repeat("l", 70)) {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		var v any
		if err := Unmarshal(data, &v); err != nil {
			var s any
			_ = UnmarshalStrict(data, &s)
			return
		}
		out, err := Marshal(v)
		if err != nil {
			t.Fatalf("Marshal of a decoded value failed: %v", err)
		}
		var v2 any
		if err := UnmarshalStrict(out, &v2); err != nil {
			t.Fatalf("Marshal output is not canonical (%v): %q", err, out)
		}
		out2, err := Marshal(v2)
		if err != nil || !bytes.Equal(out, out2) {
			t.Fatalf("decode→encode is not stable: %q vs %q (%v)", out, out2, err)
		}
	})
}
