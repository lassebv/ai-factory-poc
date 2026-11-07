package storage

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestOpenInvalidInfo(t *testing.T) {
	dir := t.TempDir()
	if _, err := Open(dir, nil, Options{}); err == nil {
		t.Error("nil info accepted")
	}
	info, _ := buildInfo("z.bin", 16384, tfile{path: []string{"z.bin"}, size: 10})
	info.PieceLength = 0
	if _, err := Open(dir, info, Options{}); err == nil {
		t.Error("zero piece length accepted")
	}
	info, _ = buildInfo("z.bin", 16384, tfile{path: []string{"z.bin"}, size: 10})
	info.Files[0].Path = nil
	if _, err := Open(dir, info, Options{}); !errors.Is(err, ErrUnsafePath) {
		t.Errorf("empty path = %v, want ErrUnsafePath", err)
	}
	info.Files[0].Path = []string{"/abs"}
	if _, err := Open(dir, info, Options{}); !errors.Is(err, ErrUnsafePath) {
		t.Errorf("absolute path = %v, want ErrUnsafePath", err)
	}
}

func TestOpenDirIsFile(t *testing.T) {
	f := filepath.Join(t.TempDir(), "file")
	os.WriteFile(f, nil, 0o644)
	info, _ := buildInfo("a", 16384, tfile{path: []string{"a"}, size: 10})
	if _, err := Open(f, info, Options{}); err == nil {
		t.Error("dir that is a regular file accepted")
	}
	if _, err := Open(filepath.Join(f, "sub"), info, Options{}); err == nil {
		t.Error("dir below a regular file accepted")
	}
	// A directory sitting where the file should go cannot be prepared.
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "a"+PartSuffix), 0o755)
	if _, err := Open(dir, info, Options{}); err == nil {
		t.Error("directory in place of .part accepted")
	}
	// A regular file where a parent directory should be.
	multi, _ := buildInfo("m", 16384, tfile{path: []string{"sub", "x"}, size: 10}, tfile{path: []string{"y"}, size: 1})
	dir = t.TempDir()
	os.MkdirAll(filepath.Join(dir, "m"), 0o755)
	os.WriteFile(filepath.Join(dir, "m", "sub"), nil, 0o644)
	if _, err := Open(dir, multi, Options{}); err == nil {
		t.Error("parent that is a file accepted")
	}
}

func TestExistingFinalFileUsedInPlace(t *testing.T) {
	info, flat := buildInfo("done.bin", 16384, tfile{path: []string{"done.bin"}, size: 20000})
	dir := t.TempDir()
	final := filepath.Join(dir, "done.bin")
	if err := os.WriteFile(final, flat, 0o644); err != nil {
		t.Fatal(err)
	}
	s := mustOpen(t, dir, info, Options{})
	if exists(final + PartSuffix) {
		t.Fatal(".part created although the final file exists")
	}
	for i := 0; i < info.NumPieces(); i++ {
		if ok, err := s.VerifyPiece(i); err != nil || !ok {
			t.Fatalf("VerifyPiece(%d) = %v, %v", i, ok, err)
		}
		if err := s.MarkVerified(i); err != nil {
			t.Fatal(err)
		}
	}
	if got, _ := os.ReadFile(final); !bytes.Equal(got, flat) {
		t.Fatal("existing data changed")
	}
}

func TestExistingLongerPartNotShortened(t *testing.T) {
	info, _ := buildInfo("long.bin", 16384, tfile{path: []string{"long.bin"}, size: 100})
	dir := t.TempDir()
	part := filepath.Join(dir, "long.bin"+PartSuffix)
	os.WriteFile(part, make([]byte, 500), 0o644)
	mustOpen(t, dir, info, Options{})
	if st, _ := os.Stat(part); st.Size() != 500 {
		t.Fatalf("size %d, want 500", st.Size())
	}
}

func TestShortFileReadsZeros(t *testing.T) {
	info, _ := buildInfo("s.bin", 16384, tfile{path: []string{"s.bin"}, size: 1000})
	dir := t.TempDir()
	s := mustOpen(t, dir, info, Options{})
	part := filepath.Join(dir, "s.bin"+PartSuffix)
	os.WriteFile(part, []byte{1, 2, 3}, 0o644) // truncated behind our back
	buf := bytes.Repeat([]byte{9}, 1000)
	if err := s.ReadBlock(0, 0, buf); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(buf[:3], []byte{1, 2, 3}) || !bytes.Equal(buf[3:], make([]byte, 997)) {
		t.Fatal("short file should read as zeros past its end")
	}
}

func TestMissingFileErrors(t *testing.T) {
	info, _ := buildInfo("gone.bin", 16384, tfile{path: []string{"gone.bin"}, size: 1000})
	dir := t.TempDir()
	s := mustOpen(t, dir, info, Options{})
	os.Remove(filepath.Join(dir, "gone.bin"+PartSuffix))
	if err := s.WriteBlock(0, 0, []byte{1}); err == nil {
		t.Error("write to a deleted file succeeded")
	}
	if _, err := s.VerifyPiece(0); err == nil {
		t.Error("verify of a deleted file succeeded")
	}
	if err := s.MarkVerified(0); err == nil {
		t.Error("rename of a deleted file succeeded")
	}
}

func TestReadOnlyFileWriteError(t *testing.T) {
	info, _ := buildInfo("ro.bin", 16384, tfile{path: []string{"ro.bin"}, size: 1000})
	dir := t.TempDir()
	s := mustOpen(t, dir, info, Options{}).(*storage)
	// Swap in a read-only descriptor to force a write error.
	h, err := s.acquire(0)
	if err != nil {
		t.Fatal(err)
	}
	h.f.Close()
	h.f, _ = os.Open(filepath.Join(dir, "ro.bin"+PartSuffix))
	s.release(h)
	if err := s.WriteBlock(0, 0, []byte{1}); err == nil {
		t.Error("write through a read-only handle succeeded")
	}
}

func TestOutOfRangeAndClosed(t *testing.T) {
	info, _ := buildInfo("r.bin", 16384, tfile{path: []string{"r.bin"}, size: 20000})
	s := mustOpen(t, t.TempDir(), info, Options{})
	if err := s.WriteBlock(0, -1, []byte{1}); !errors.Is(err, ErrOutOfRange) {
		t.Errorf("negative begin = %v", err)
	}
	if err := s.MarkVerified(5); !errors.Is(err, ErrOutOfRange) {
		t.Errorf("MarkVerified(5) = %v", err)
	}
	if s.WantedBytes(-1) != 0 || s.WantedBytes(0) != 16384 || s.WantedBytes(1) != 20000-16384 {
		t.Error("WantedBytes wrong")
	}
	s.Close()
	if err := s.ReadBlock(0, 0, make([]byte, 1)); !errors.Is(err, ErrClosed) {
		t.Errorf("ReadBlock after Close = %v", err)
	}
	if _, err := s.VerifyPiece(0); !errors.Is(err, ErrClosed) {
		t.Errorf("VerifyPiece after Close = %v", err)
	}
	if err := s.MarkVerified(0); !errors.Is(err, ErrClosed) {
		t.Errorf("MarkVerified after Close = %v", err)
	}
	if _, err := s.(*storage).acquire(0); !errors.Is(err, ErrClosed) {
		t.Errorf("acquire after Close = %v", err)
	}
}

func TestStaleHandleClosedByLastUser(t *testing.T) {
	info, flat := buildInfo("st.bin", 16384, tfile{path: []string{"st.bin"}, size: 1000})
	dir := t.TempDir()
	s := mustOpen(t, dir, info, Options{}).(*storage)
	if err := s.WriteBlock(0, 0, flat); err != nil {
		t.Fatal(err)
	}
	h, err := s.acquire(0) // simulate a reader in the middle of an op
	if err != nil {
		t.Fatal(err)
	}
	if err := s.MarkVerified(0); err != nil {
		t.Fatal(err)
	}
	if !h.stale || s.open != 1 {
		t.Fatalf("in-use handle should be stale and still counted: stale=%v open=%d", h.stale, s.open)
	}
	s.release(h)
	if s.open != 0 {
		t.Fatalf("open = %d after last release, want 0", s.open)
	}
	buf := make([]byte, 1000)
	if err := s.ReadBlock(0, 0, buf); err != nil || !bytes.Equal(buf, flat) {
		t.Fatalf("read after rename: %v", err)
	}
	if !exists(filepath.Join(dir, "st.bin")) {
		t.Fatal("not renamed")
	}
}

func TestHandleLimitBlocksUntilRelease(t *testing.T) {
	info, _ := buildInfo("lim", 16384, tfile{path: []string{"a"}, size: 10}, tfile{path: []string{"b"}, size: 10})
	s := mustOpen(t, t.TempDir(), info, Options{MaxOpenFiles: 1}).(*storage)
	h, err := s.acquire(0)
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan *handle)
	go func() {
		h2, _ := s.acquire(1)
		got <- h2
	}()
	// The second acquire cannot proceed while handle 0 is in use.
	s.mu.Lock()
	for len(s.cache) != 1 {
		s.cond.Wait()
	}
	s.mu.Unlock()
	s.release(h)
	h2 := <-got
	if h2 == nil || h2.file != 1 || s.open != 1 {
		t.Fatalf("acquire after release: %+v open=%d", h2, s.open)
	}
	s.release(h2)
}

func TestCloseWaitsForBlockedAcquire(t *testing.T) {
	info, _ := buildInfo("cw", 16384, tfile{path: []string{"a"}, size: 10}, tfile{path: []string{"b"}, size: 10})
	s := mustOpen(t, t.TempDir(), info, Options{MaxOpenFiles: 1}).(*storage)
	h, _ := s.acquire(0)
	errc := make(chan error, 1)
	go func() { errc <- s.ReadBlock(0, 10, make([]byte, 10)) }() // needs file b
	closed := make(chan error, 1)
	go func() { closed <- s.Close() }()
	if err := <-errc; !errors.Is(err, ErrClosed) && err != nil {
		t.Fatalf("blocked read = %v", err)
	}
	s.release(h)
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
	if s.open != 0 {
		t.Fatalf("open = %d after Close", s.open)
	}
}

func TestConcurrentReadWriteDefaults(t *testing.T) {
	info, flat := buildInfo("cc", 16384,
		tfile{path: []string{"a"}, size: 50000},
		tfile{path: []string{".pad", "1"}, size: 15536, pad: true},
		tfile{path: []string{"b"}, size: 70000})
	s := mustOpen(t, t.TempDir(), info, Options{})
	var wg sync.WaitGroup
	for i := 0; i < info.NumPieces(); i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			writePiece(t, s, info, flat, i)
			got := make([]byte, s.PieceSize(i))
			if err := s.ReadBlock(i, 0, got); err != nil || !bytes.Equal(got, pieceData(info, flat, i)) {
				t.Errorf("piece %d readback: %v", i, err)
			}
			if ok, err := s.VerifyPiece(i); err != nil || !ok {
				t.Errorf("VerifyPiece(%d) = %v, %v", i, ok, err)
			}
			if err := s.MarkVerified(i); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	var total int64
	for i := 0; i < info.NumPieces(); i++ {
		total += s.WantedBytes(i)
	}
	if total != info.VisibleLength() {
		t.Fatalf("sum of WantedBytes = %d, want %d", total, info.VisibleLength())
	}
}

func TestVerifyLargePieceInChunks(t *testing.T) {
	info, flat := buildInfo("big.bin", 4<<20, tfile{path: []string{"big.bin"}, size: 3<<20 + 7})
	s := mustOpen(t, t.TempDir(), info, Options{})
	writePiece(t, s, info, flat, 0)
	if ok, err := s.VerifyPiece(0); err != nil || !ok {
		t.Fatalf("VerifyPiece = %v, %v", ok, err)
	}
}
