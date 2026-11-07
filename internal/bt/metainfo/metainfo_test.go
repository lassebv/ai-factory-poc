package metainfo

import (
	"bytes"
	"crypto/sha1"
	"errors"
	"strings"
	"testing"

	"torrentd/internal/bt/bencode"
)

func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	b, err := bencode.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func hashes(n int) string { return strings.Repeat("x", 20*n) }

func single(name string, length int64) map[string]any {
	return map[string]any{"name": name, "length": length, "piece length": 16384,
		"pieces": hashes(int((length + 16383) / 16384))}
}

func torrent(t *testing.T, info any, extra map[string]any) []byte {
	t.Helper()
	m := map[string]any{"info": info}
	for k, v := range extra {
		m[k] = v
	}
	return mustMarshal(t, m)
}

func TestParseSingleFile(t *testing.T) {
	info := single("a.bin", 20000)
	info["private"] = 1
	data := torrent(t, info, map[string]any{"announce": "http://t/a"})
	m, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	raw := mustMarshal(t, info)
	if m.InfoHash != sha1.Sum(raw) || !bytes.Equal(m.RawInfo, raw) {
		t.Fatal("info-hash is not the SHA-1 of the info bytes")
	}
	if len(m.HexHash()) != 40 {
		t.Fatal(m.HexHash())
	}
	i := m.Info
	if i.Name != "a.bin" || !i.Private || i.MultiFile || i.NumPieces() != 2 || i.TotalLength != 20000 {
		t.Fatalf("%+v", i)
	}
	if i.PieceHash(1) != [20]byte(bytes.Repeat([]byte("x"), 20)) {
		t.Fatal("PieceHash")
	}
	if !i.PowerOfTwoPieces() {
		t.Fatal("16 KiB is a power of two")
	}
	if len(m.Trackers) != 1 || m.Trackers[0] != (Tracker{URL: "http://t/a"}) {
		t.Fatalf("trackers %+v", m.Trackers)
	}
	if vf := i.VisibleFiles(); len(vf) != 1 || vf[0].PathString() != "a.bin" {
		t.Fatalf("%+v", vf)
	}
}

func TestParseMultiFileWithPadsAndUTF8(t *testing.T) {
	info := map[string]any{
		"name": "bad", "name.utf-8": "dir", "piece length": 16384, "pieces": hashes(2),
		"files": []any{
			map[string]any{"length": 16000, "path": []any{"a"}},
			map[string]any{"length": 384, "path": []any{".pad", "384"}, "attr": "p"},
			map[string]any{"length": 100, "path": []any{"x"}, "path.utf-8": []any{"sub", "b"}},
			map[string]any{"length": 0, "path": []any{"empty"}},
		},
	}
	m, err := Parse(torrent(t, info, map[string]any{
		"announce":      "http://ignored",
		"announce-list": []any{[]any{"http://a", "http://b"}, []any{}, []any{"http://a", " "}, []any{"udp://c"}},
	}))
	if err != nil {
		t.Fatal(err)
	}
	i := m.Info
	if !i.MultiFile || i.Name != "dir" || i.TotalLength != 16484 || i.VisibleLength() != 16100 {
		t.Fatalf("%+v", i)
	}
	vf := i.VisibleFiles()
	if len(vf) != 3 || vf[0].PathString() != "dir/a" || vf[1].PathString() != "dir/sub/b" || vf[1].Offset != 16384 {
		t.Fatalf("%+v", vf)
	}
	want := []Tracker{{"http://a", 0}, {"http://b", 0}, {"udp://c", 1}}
	if len(m.Trackers) != len(want) {
		t.Fatalf("trackers %+v", m.Trackers)
	}
	for k := range want {
		if m.Trackers[k] != want[k] {
			t.Fatalf("trackers %+v", m.Trackers)
		}
	}
}

func TestParseRejects(t *testing.T) {
	files := func(fs ...any) map[string]any {
		return map[string]any{"name": "d", "piece length": 16384, "pieces": hashes(1), "files": fs}
	}
	file := func(length int64, p ...any) map[string]any { return map[string]any{"length": length, "path": p} }
	mod := func(f func(m map[string]any)) map[string]any {
		m := single("a", 100)
		f(m)
		return m
	}
	cases := map[string][]byte{
		"not bencode":    []byte("hello"),
		"not a dict":     []byte("4:spam"),
		"no info":        []byte("d8:announce1:xe"),
		"unsorted info":  []byte("d4:infod4:name1:a6:lengthi1eee"),
		"no pieces":      torrent(t, mod(func(m map[string]any) { delete(m, "pieces") }), nil),
		"v2 only":        torrent(t, mod(func(m map[string]any) { delete(m, "pieces"); m["meta version"] = 2 }), nil),
		"no piece len":   torrent(t, mod(func(m map[string]any) { delete(m, "piece length") }), nil),
		"small piece":    torrent(t, mod(func(m map[string]any) { m["piece length"] = 8192 }), nil),
		"huge piece":     torrent(t, mod(func(m map[string]any) { m["piece length"] = 512 << 20 }), nil),
		"no name":        torrent(t, mod(func(m map[string]any) { delete(m, "name") }), nil),
		"bad name":       torrent(t, mod(func(m map[string]any) { m["name"] = ".." }), nil),
		"name type":      torrent(t, mod(func(m map[string]any) { m["name"] = 5 }), nil),
		"zero length":    torrent(t, mod(func(m map[string]any) { m["length"] = 0 }), nil),
		"no length":      torrent(t, mod(func(m map[string]any) { delete(m, "length") }), nil),
		"pieces %20":     torrent(t, mod(func(m map[string]any) { m["pieces"] = "abc" }), nil),
		"pieces count":   torrent(t, mod(func(m map[string]any) { m["pieces"] = hashes(2) }), nil),
		"too many":       torrent(t, mod(func(m map[string]any) { m["length"] = int64(16384) << 22; m["pieces"] = "" }), nil),
		"empty files":    torrent(t, files(), nil),
		"file no length": torrent(t, files(map[string]any{"path": []any{"a"}}), nil),
		"file negative":  torrent(t, files(file(-1, "a")), nil),
		"file no path":   torrent(t, files(file(1)), nil),
		"dotdot":         torrent(t, files(file(1, "..")), nil),
		"dot":            torrent(t, files(file(1, ".")), nil),
		"empty comp":     torrent(t, files(file(1, "a", "")), nil),
		"slash":          torrent(t, files(file(1, "/etc")), nil),
		"backslash":      torrent(t, files(file(1, `a\b`)), nil),
		"nul":            torrent(t, files(file(1, "a\x00")), nil),
		"duplicate":      torrent(t, files(file(1, "a"), file(1, "a")), nil),
		"dup case":       torrent(t, files(file(1, "Readme"), file(1, "README")), nil),
		"dup nfc":        torrent(t, files(file(1, "café"), file(1, "café")), nil),
		"dup sanitized":  torrent(t, files(file(1, "a?"), file(1, "a*")), nil),
		"all empty":      torrent(t, files(file(0, "a")), nil),
		"overflow":       torrent(t, files(file(1<<62, "a"), file(1<<62, "b"), file(1<<62, "c")), nil),
	}
	for name, data := range cases {
		if _, err := Parse(data); !errors.Is(err, ErrInvalidTorrent) {
			t.Errorf("%s: err = %v, want ErrInvalidTorrent", name, err)
		}
	}
}

func TestSanitize(t *testing.T) {
	for in, want := range map[string]string{
		"ok.txt":      "ok.txt",
		`a<b>c:d"e|f`: "a_b_c_d_e_f",
		"q?*":         "q__",
		"trail..":     "trail__",
		"space ":      "space_",
		"CON":         "CON_",
		"nul.txt":     "nul_.txt",
		"Lpt1.a.b":    "Lpt1_.a.b",
		"console":     "console",
		"tab\there":   "tab_here",
		"bad\xffutf8": "bad_utf8",
	} {
		if got := sanitize(in); got != want {
			t.Errorf("sanitize(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTrackersFallbacks(t *testing.T) {
	// A malformed announce-list falls back to announce.
	tr := trackers(bencode.RawMessage("3:url"), bencode.RawMessage("i1e"))
	if len(tr) != 1 || tr[0].URL != "url" {
		t.Fatalf("%+v", tr)
	}
	if tr := trackers(bencode.RawMessage("i1e"), nil); len(tr) != 0 {
		t.Fatalf("%+v", tr)
	}
	if tr := trackers(nil, nil); len(tr) != 0 {
		t.Fatalf("%+v", tr)
	}
}

func TestNonPowerOfTwo(t *testing.T) {
	info := single("a", 30000)
	info["piece length"] = 20000
	info["pieces"] = hashes(2)
	m, err := Parse(torrent(t, info, nil))
	if err != nil {
		t.Fatal(err)
	}
	if m.Info.PowerOfTwoPieces() {
		t.Fatal("20000 is not a power of two")
	}
}

func TestMagnetErrorsWrap(t *testing.T) {
	for _, uri := range []string{"magnet:?xt=urn:btih:%zz", "magnet:?xt=urn:btih:" + strings.Repeat("A", 32)[:31] + "%"} {
		if _, err := ParseMagnet(uri); !errors.Is(err, ErrInvalidMagnet) {
			t.Errorf("ParseMagnet(%q) = %v", uri, err)
		}
	}
	m, err := ParseMagnet("MAGNET:?&xt=urn:btih:" + sintelHex + "&xt=urn:btih:" + strings.Repeat("0", 40) + "&tr=&x.pe=")
	if err != nil || m.InfoHash != sintelHash(t) || len(m.Trackers) != 0 || len(m.Peers) != 0 {
		t.Fatalf("%+v %v", m, err)
	}
}
