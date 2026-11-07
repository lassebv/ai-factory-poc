package bencode

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

type inner struct {
	N int `bencode:"n"`
}

type kitchen struct {
	B     bool              `bencode:"b"`
	U8    uint8             `bencode:"u8"`
	I8    int8              `bencode:"i8"`
	Arr   [2]int            `bencode:"arr"`
	Hash  [4]byte           `bencode:"hash"`
	Ptr   *inner            `bencode:"ptr"`
	Map   map[string]string `bencode:"map,omitempty"`
	Any   any               `bencode:"any"`
	Raw   RawMessage        `bencode:"raw"`
	Plain string
	Dup   string `bencode:"Plain"`
	priv  int
}

func TestKitchenSinkRoundTrip(t *testing.T) {
	in := kitchen{B: true, U8: 255, I8: -128, Arr: [2]int{1, 2}, Hash: [4]byte{'a', 'b', 'c', 'd'},
		Ptr: &inner{N: 3}, Map: map[string]string{"z": "1", "a": "2"}, Any: []any{"x"},
		Raw: RawMessage("i9e"), Plain: "p"}
	enc, err := Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	want := "d5:Plain1:p3:anyl1:xe3:arrli1ei2ee1:bi1e4:hash4:abcd2:i8i-128e3:mapd1:a1:21:z1:1e3:ptrd1:ni3ee3:rawi9e2:u8i255ee"
	if string(enc) != want {
		t.Fatalf("Marshal = %q\nwant      %q", enc, want)
	}
	var out kitchen
	if err := UnmarshalStrict(enc, &out); err != nil {
		t.Fatal(err)
	}
	in.priv = 0
	if !reflect.DeepEqual(in, out) {
		t.Fatalf("got %#v\nwant %#v", out, in)
	}
	// Nil pointers, nil interfaces and empty RawMessages are omitted.
	enc, err = Marshal(kitchen{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(enc), "ptr") || strings.Contains(string(enc), "raw") || strings.Contains(string(enc), "any") {
		t.Fatalf("nil fields encoded: %q", enc)
	}
}

func TestMarshalErrors(t *testing.T) {
	var nilPtr *inner
	for _, v := range []any{nil, nilPtr, 1.5, map[int]int{1: 1}, []any{nil}, RawMessage{}, make(chan int),
		map[string]any{"a": 1.5}, struct{ F float64 }{}, []any{[]any{func() {}}}} {
		if _, err := Marshal(v); err == nil {
			t.Errorf("Marshal(%#v) succeeded", v)
		}
	}
	var te *TypeError
	if _, err := Marshal(1.5); !errors.As(err, &te) || te.Error() == "" {
		t.Errorf("want a *TypeError, got %v", err)
	}
	// Bool false and omitempty of every kind.
	type oe struct {
		S string  `bencode:"s,omitempty"`
		I int     `bencode:"i,omitempty"`
		U uint    `bencode:"u,omitempty"`
		B bool    `bencode:"b,omitempty"`
		P *int    `bencode:"p,omitempty"`
		F float64 `bencode:"f,omitempty"`
		X bool    `bencode:"x"`
	}
	if _, err := Marshal(oe{}); err == nil {
		t.Error("float field must fail even with omitempty")
	}
	type oe2 struct {
		S string `bencode:"s,omitempty"`
		I int    `bencode:"i,omitempty"`
		U uint   `bencode:"u,omitempty"`
		B bool   `bencode:"b,omitempty"`
		X bool   `bencode:"x"`
	}
	if b, err := Marshal(oe2{}); err != nil || string(b) != "d1:xi0ee" {
		t.Errorf("Marshal(oe2{}) = %q, %v", b, err)
	}
}

func TestUnmarshalTypeErrors(t *testing.T) {
	var (
		s   string
		n   int8
		u   uint8
		b   bool
		arr [1]int
		h   [2]byte
		sl  []int
		bs  []byte
		m   map[string]int
		st  inner
		fn  error
	)
	cases := []struct {
		in string
		v  any
	}{
		{"i1e", &s}, {"i300e", &n}, {"i-1e", &u}, {"i300e", &u}, {"i99999999999999999999e", &b},
		{"li1ei2ee", &arr}, {"3:abc", &h}, {"3:abc", &sl}, {"l1:ae", &bs}, {"d1:ai1ee", &sl},
		{"le", &m}, {"d1:a1:xe", &m}, {"le", &st}, {"1:a", &fn}, {"i99999999999999999999e", new(any)},
		{"i99999999999999999999e", &n}, {"x", new(any)}, {"lxe", new(any)}, {"d1:ax", new(any)},
	}
	for _, c := range cases {
		if err := Unmarshal([]byte(c.in), c.v); err == nil {
			t.Errorf("Unmarshal(%q, %T) succeeded", c.in, c.v)
		}
	}
	if err := Unmarshal([]byte("i1e"), nil); !errors.Is(err, ErrNilTarget) {
		t.Errorf("nil target: %v", err)
	}
	if err := Unmarshal([]byte("i1e"), s); !errors.Is(err, ErrNilTarget) {
		t.Errorf("non-pointer target: %v", err)
	}
	var se *SyntaxError
	if err := Unmarshal([]byte("i1"), new(any)); !errors.As(err, &se) || se.Error() == "" {
		t.Errorf("want a *SyntaxError, got %v", err)
	}
}

func TestUnmarshalAssorted(t *testing.T) {
	var u uint64
	if err := Unmarshal([]byte("i18446744073709551615e"), &u); err != nil || u != 1<<64-1 {
		t.Fatalf("uint64 max: %d %v", u, err)
	}
	if err := Unmarshal([]byte("i-0e"), &u); err != nil || u != 0 {
		t.Fatalf("lenient -0: %d %v", u, err)
	}
	var b bool
	if err := Unmarshal([]byte("i1e"), &b); err != nil || !b {
		t.Fatalf("bool: %v %v", b, err)
	}
	var pp **int
	if err := Unmarshal([]byte("i7e"), &pp); err != nil || **pp != 7 {
		t.Fatalf("pointer chain: %v", err)
	}
	// Lenient: duplicate keys (last wins), non-canonical numbers and lengths.
	var m map[string]int
	if err := Unmarshal([]byte("d1:ai1e1:ai02e01:bi3ee"), &m); err != nil || m["a"] != 2 || m["b"] != 3 {
		t.Fatalf("lenient map: %v %v", m, err)
	}
	if err := UnmarshalStrict([]byte("d01:ai1ee"), &m); err == nil {
		t.Fatal("strict must reject a non-canonical key length")
	}
	// Unknown struct keys are skipped but still validated.
	var st inner
	if err := UnmarshalStrict([]byte("d1:nli1ee1:xd1:yleee"), &st); err == nil {
		t.Fatal("list into int field must fail")
	}
	if err := UnmarshalStrict([]byte("d1:ni5e1:xd1:yleee"), &st); err != nil || st.N != 5 {
		t.Fatalf("skip unknown: %+v %v", st, err)
	}
	if err := UnmarshalStrict([]byte("d1:ni5e1:xd1:yi01eee"), &st); err == nil {
		t.Fatal("strict must validate skipped values")
	}
	var arr [3]int
	if err := Unmarshal([]byte("li1ei2ee"), &arr); err != nil || arr != [3]int{1, 2, 0} {
		t.Fatalf("short array: %v %v", arr, err)
	}
	var l []any
	if err := Unmarshal([]byte("le"), &l); err != nil || l == nil || len(l) != 0 {
		t.Fatalf("empty list: %#v %v", l, err)
	}
	for _, in := range []string{"i1e", "le", "d1:ai1ee"} {
		if !Valid([]byte(in), true) {
			t.Errorf("Valid(%q) = false", in)
		}
	}
	if Valid([]byte("i01e"), true) || !Valid([]byte("i01e"), false) {
		t.Error("Valid strictness")
	}
	if err := Unmarshal([]byte("d"+strings.Repeat("1:a", 0)+strings.Repeat("d1:a", 65)+strings.Repeat("e", 66)), new(any)); err == nil {
		t.Error("deep dicts must be rejected")
	}
	if err := Unmarshal([]byte(strings.Repeat("d1:a", 65)+"i1e"+strings.Repeat("e", 65)), &st); err == nil {
		t.Error("deep dicts into a struct must be rejected")
	}
}
