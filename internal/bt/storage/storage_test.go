package storage

import (
	"bytes"
	"crypto/sha1"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"torrentd/internal/bt/metainfo"
)

type tfile struct {
	path []string
	size int64
	pad  bool
}

// build returns an Info for files with piece length pl and the content.
func build(pl int64, files ...tfile) (*metainfo.Info, []byte) {
	info := &metainfo.Info{Name: "t", PieceLength: pl, MultiFile: true}
	var data []byte
	for i, f := range files {
		info.Files = append(info.Files, metainfo.File{Path: f.path, Length: f.size, Offset: int64(len(data)), Pad: f.pad})
		for j := int64(0); j < f.size; j++ {
			b := byte(i*31 + int(j)%251 + 1)
			if f.pad {
				b = 0
			}
			data = append(data, b)
		}
	}
	info.TotalLength = int64(len(data))
	for off := int64(0); off < info.TotalLength; off += pl {
		sum := sha1.Sum(data[off:min(off+pl, info.TotalLength)])
		info.Pieces = append(info.Pieces, sum[:]...)
	}
	return info, data
}

func piece(data []byte, pl int64, i int) []byte {
	return data[int64(i)*pl : min(int64(i+1)*pl, int64(len(data)))]
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

func TestSpanningPiecesPartFilesAndPads(t *testing.T) {
	dir := t.TempDir()
	info, data := build(100,
		tfile{path: []string{"t", "a.bin"}, size: 150},
		tfile{path: []string{"t", "empty"}, size: 0},
		tfile{path: []string{"t", ".pad", "50"}, size: 50, pad: true},
		tfile{path: []string{"t", "sub", "b.bin"}, size: 30},
		tfile{path: []string{"t", "c.bin"}, size: 125},
	) // 355 bytes, 4 pieces
	s, err := Open(dir, info, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	root := filepath.Join(dir, "t")
	if !exists(filepath.Join(root, "empty")) || !exists(filepath.Join(root, "a.bin.part")) ||
		!exists(filepath.Join(root, "sub", "b.bin.part")) || exists(filepath.Join(root, ".pad")) {
		t.Fatal("Open did not lay out the files as expected")
	}
	if st, _ := os.Stat(filepath.Join(root, "c.bin.part")); st.Size() != 125 {
		t.Fatalf("c.bin.part size %d", st.Size())
	}
	if s.NumPieces() != 4 || s.PieceSize(3) != 55 || s.PieceSize(0) != 100 {
		t.Fatal("geometry")
	}
	if s.VisibleBytes(1) != 50 || s.VisibleBytes(2) != 100 {
		t.Fatalf("VisibleBytes = %d, %d", s.VisibleBytes(1), s.VisibleBytes(2))
	}
	// Pieces 0 and 1 cover a.bin (and the pad); a.bin completes with piece 1.
	for i := 0; i < 2; i++ {
		p := piece(data, 100, i)
		if !s.Verify(i, p) {
			t.Fatalf("Verify(%d)", i)
		}
		if err := s.WritePiece(i, p); err != nil {
			t.Fatal(err)
		}
		renamed, err := s.MarkVerified(i)
		if err != nil {
			t.Fatal(err)
		}
		if (i == 1) != (len(renamed) == 1) {
			t.Fatalf("piece %d renamed %v", i, renamed)
		}
	}
	if !exists(filepath.Join(root, "a.bin")) || exists(filepath.Join(root, "a.bin.part")) {
		t.Fatal("a.bin was not renamed")
	}
	if r, _ := s.MarkVerified(1); r != nil {
		t.Fatal("MarkVerified twice renamed again")
	}
	// Reading back works through the final name, and pads read as zeros.
	got := make([]byte, 100)
	if err := s.ReadPiece(1, got); err != nil || !bytes.Equal(got, piece(data, 100, 1)) {
		t.Fatalf("ReadPiece(1): %v", err)
	}
	for i := 2; i < 4; i++ {
		s.WritePiece(i, piece(data, 100, i))
		if _, err := s.MarkVerified(i); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{"a.bin", "sub/b.bin", "c.bin"} {
		if exists(filepath.Join(root, f+PartSuffix)) || !exists(filepath.Join(root, f)) {
			t.Errorf("%s not final", f)
		}
	}
	b, _ := os.ReadFile(filepath.Join(root, "c.bin"))
	if !bytes.Equal(b, data[230:]) {
		t.Error("c.bin content")
	}
	if s.cache.open() > 2 {
		t.Errorf("%d handles open, limit 2", s.cache.open())
	}
	if s.Verify(0, []byte("bad")) || s.Verify(9, nil) {
		t.Error("Verify accepted bad data")
	}
}

func TestRangeErrorsAndClose(t *testing.T) {
	info, data := build(64, tfile{path: []string{"x"}, size: 100})
	s, err := Open(t.TempDir(), info, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.WritePiece(2, nil); err != ErrPieceRange {
		t.Errorf("WritePiece(2): %v", err)
	}
	if err := s.WritePiece(0, data[:10]); err != ErrPieceRange {
		t.Errorf("short WritePiece: %v", err)
	}
	if err := s.ReadPiece(-1, nil); err != ErrPieceRange {
		t.Errorf("ReadPiece(-1): %v", err)
	}
	if err := s.ReadPiece(0, nil); err != ErrPieceRange {
		t.Errorf("short ReadPiece: %v", err)
	}
	if _, err := s.MarkVerified(5); err != ErrPieceRange {
		t.Errorf("MarkVerified(5): %v", err)
	}
	if err := s.WritePiece(1, data[64:]); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.WritePiece(1, data[64:]); !errors.Is(err, ErrClosed) {
		t.Errorf("after Close: %v", err)
	}
	if _, err := s.cache.acquire("x"); err != ErrClosed {
		t.Errorf("acquire after close: %v", err)
	}
}

func TestOpenErrors(t *testing.T) {
	info, _ := build(64, tfile{path: []string{"..", "x"}, size: 10})
	if _, err := Open(t.TempDir(), info, 0); err == nil {
		t.Error("unsafe path accepted")
	}
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "blocker"), nil, 0o644)
	info, _ = build(64, tfile{path: []string{"blocker", "x"}, size: 10})
	if _, err := Open(dir, info, 0); err == nil {
		t.Error("file in place of a directory accepted")
	}
	info, _ = build(64, tfile{path: []string{"d"}, size: 0})
	os.Mkdir(filepath.Join(dir, "d"), 0o755)
	if _, err := Open(dir, info, 0); err == nil {
		t.Error("directory in place of an empty file accepted")
	}
	info, _ = build(64, tfile{path: []string{"e"}, size: 10})
	os.Mkdir(filepath.Join(dir, "e.part"), 0o755)
	if _, err := Open(dir, info, 0); err == nil {
		t.Error("directory in place of a part file accepted")
	}
}

func TestHandleCache(t *testing.T) {
	dir := t.TempDir()
	c := newHandleCache(2)
	p := func(n string) string { return filepath.Join(dir, n) }
	a, _ := c.acquire(p("a"))
	b, _ := c.acquire(p("b"))
	// Both busy: a third file goes over the limit rather than closing them.
	x, err := c.acquire(p("c"))
	if err != nil || c.open() != 3 {
		t.Fatalf("busy overflow: %v, %d open", err, c.open())
	}
	c.release(a)
	c.release(b)
	c.release(x)
	if _, err := c.acquire(p("d")); err != nil || c.open() > 2 {
		t.Fatalf("after release: %v, %d open", err, c.open())
	}
	again, _ := c.acquire(p("d"))
	c.release(again)
	// Evicting a busy handle closes it on its last release.
	busy, _ := c.acquire(p("e"))
	if err := c.evict(p("e")); err != nil {
		t.Fatal(err)
	}
	c.release(busy)
	if _, err := busy.f.Stat(); err == nil {
		t.Error("evicted busy handle still open after release")
	}
	if err := c.evict(p("nope")); err != nil {
		t.Fatal(err)
	}
	if _, err := c.acquire(filepath.Join(dir, "missing", "f")); err == nil {
		t.Error("open in a missing dir succeeded")
	}
	if err := c.closeAll(); err != nil {
		t.Fatal(err)
	}
}

func TestPool(t *testing.T) {
	p := NewPool(3)
	var n atomic.Int32
	var wg sync.WaitGroup
	for range 100 {
		wg.Add(1)
		if !p.Submit(func() { n.Add(1); wg.Done() }) {
			t.Fatal("Submit failed")
		}
	}
	wg.Wait()
	p.Close()
	if n.Load() != 100 || p.Submit(func() {}) {
		t.Fatalf("ran %d jobs; Submit after Close accepted", n.Load())
	}
	if DefaultHashWorkers() < 1 {
		t.Fatal("DefaultHashWorkers")
	}
}
