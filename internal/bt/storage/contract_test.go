package storage

// FROZEN CONTRACT TESTS (ADR-0011, ADR-0017, ADR-0019). Owned by the architect. Every correct
// implementation of contract.go must pass these.

import (
	"bytes"
	"context"
	"crypto/sha1"
	"errors"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"torrentd/internal/bt/btest"
	"torrentd/internal/bt/metainfo"
	"torrentd/internal/bt/piece"
)

type tfile struct {
	path []string
	size int64
	pad  bool
}

// buildInfo returns an Info for files plus the flat content (pad ranges are
// zeros), with correct piece hashes.
func buildInfo(name string, pieceLen int64, files ...tfile) (*metainfo.Info, []byte) {
	info := &metainfo.Info{Name: name, PieceLength: pieceLen, MultiFile: len(files) > 1}
	rng := rand.New(rand.NewPCG(uint64(len(name)), uint64(pieceLen)))
	var flat []byte
	for _, f := range files {
		p := f.path
		if info.MultiFile {
			p = append([]string{name}, f.path...)
		}
		info.Files = append(info.Files, metainfo.File{Path: p, Length: f.size, Offset: int64(len(flat)), Pad: f.pad})
		chunk := make([]byte, f.size)
		if !f.pad {
			for i := range chunk {
				chunk[i] = byte(rng.Uint32())
			}
		}
		flat = append(flat, chunk...)
	}
	info.TotalLength = int64(len(flat))
	for off := int64(0); off < info.TotalLength; off += pieceLen {
		end := min(off+pieceLen, info.TotalLength)
		h := sha1.Sum(flat[off:end])
		info.Pieces = append(info.Pieces, h[:]...)
	}
	return info, flat
}

func pieceData(info *metainfo.Info, flat []byte, i int) []byte {
	off := int64(i) * info.PieceLength
	return flat[off:min(off+info.PieceLength, info.TotalLength)]
}

// writePiece writes piece i in 16 KiB blocks.
func writePiece(t *testing.T, s Storage, info *metainfo.Info, flat []byte, i int) {
	t.Helper()
	data := pieceData(info, flat, i)
	for b := 0; b < len(data); b += 16384 {
		if err := s.WriteBlock(i, b, data[b:min(b+16384, len(data))]); err != nil {
			t.Fatalf("WriteBlock(%d, %d): %v", i, b, err)
		}
	}
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

func mustOpen(t *testing.T, dir string, info *metainfo.Info, opts Options) Storage {
	t.Helper()
	s, err := Open(dir, info, opts)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestContractSpanningFilesAndPartRename(t *testing.T) {
	info, flat := buildInfo("spanning", 32768,
		tfile{path: []string{"first.bin"}, size: 70000},
		tfile{path: []string{"empty.txt"}, size: 0},
		tfile{path: []string{"dir", "small.bin"}, size: 5000},
		tfile{path: []string{"dir", "deeper", "big.bin"}, size: 131079},
		tfile{path: []string{"last.bin"}, size: 33333})
	dir := filepath.Join(t.TempDir(), "not", "yet")
	s := mustOpen(t, dir, info, Options{})
	root := filepath.Join(dir, "spanning")

	for _, f := range info.Files {
		p := filepath.Join(append([]string{dir}, f.Path...)...)
		if f.Length == 0 {
			if !exists(p) {
				t.Errorf("zero-length file %s should exist under its final name", p)
			}
			continue
		}
		st, err := os.Stat(p + PartSuffix)
		if err != nil {
			t.Fatalf("Open should preallocate %s.part: %v", p, err)
		}
		if st.Size() != f.Length {
			t.Errorf("%s.part size %d, want %d", p, st.Size(), f.Length)
		}
		if exists(p) {
			t.Errorf("%s exists before its pieces are verified", p)
		}
	}
	if s.PieceSize(0) != 32768 || s.PieceSize(info.NumPieces()-1) != info.TotalLength%32768 || s.PieceSize(99) != 0 {
		t.Errorf("PieceSize wrong")
	}

	// Pieces 0-2 cover first.bin (0..70000) fully: piece 2 ends at 98304.
	for i := 0; i < 3; i++ {
		writePiece(t, s, info, flat, i)
		ok, err := s.VerifyPiece(i)
		if err != nil || !ok {
			t.Fatalf("VerifyPiece(%d) = %v, %v", i, ok, err)
		}
		if err := s.MarkVerified(i); err != nil {
			t.Fatal(err)
		}
	}
	if !exists(filepath.Join(root, "first.bin")) || exists(filepath.Join(root, "first.bin.part")) {
		t.Fatal("first.bin should be renamed once pieces 0-2 are verified")
	}
	if !exists(filepath.Join(root, "dir", "small.bin")) {
		t.Fatal("dir/small.bin (70000..75000) lies inside piece 2 and should be renamed")
	}
	if !exists(filepath.Join(root, "dir", "deeper", "big.bin.part")) {
		t.Fatal("big.bin is not complete and should still be .part")
	}
	if err := s.MarkVerified(2); err != nil { // idempotent
		t.Fatal(err)
	}
	for i := 3; i < info.NumPieces(); i++ {
		writePiece(t, s, info, flat, i)
		if ok, err := s.VerifyPiece(i); err != nil || !ok {
			t.Fatalf("VerifyPiece(%d) = %v, %v", i, ok, err)
		}
		if err := s.MarkVerified(i); err != nil {
			t.Fatal(err)
		}
	}
	// Data stays readable after the renames, across file boundaries.
	buf := make([]byte, 16384)
	if err := s.ReadBlock(2, 0, buf); err != nil || !bytes.Equal(buf, flat[65536:65536+16384]) {
		t.Fatalf("ReadBlock across first.bin/small.bin after rename: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	for _, f := range info.Files {
		p := filepath.Join(append([]string{dir}, f.Path...)...)
		got, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		if !bytes.Equal(got, flat[f.Offset:f.Offset+f.Length]) {
			t.Errorf("%s content differs", p)
		}
		if exists(p + PartSuffix) {
			t.Errorf("%s.part remains", p)
		}
	}
}

func TestContractVerifyDetectsCorruption(t *testing.T) {
	info, flat := buildInfo("one.bin", 32768, tfile{path: []string{"one.bin"}, size: 80000})
	dir := t.TempDir()
	s := mustOpen(t, dir, info, Options{})
	bad := bytes.Clone(pieceData(info, flat, 1))
	bad[100] ^= 0xff
	if err := s.WriteBlock(1, 0, bad); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.VerifyPiece(1); err != nil || ok {
		t.Fatalf("VerifyPiece of corrupted data = %v, %v; want false, nil", ok, err)
	}
	writePiece(t, s, info, flat, 1)
	if ok, err := s.VerifyPiece(1); err != nil || !ok {
		t.Fatalf("VerifyPiece after rewrite = %v, %v", ok, err)
	}
	if ok, _ := s.VerifyPiece(0); ok {
		t.Fatal("an unwritten (sparse zero) piece must not verify")
	}
	// Single-file torrents live at dir/<name>.
	if !exists(filepath.Join(dir, "one.bin.part")) {
		t.Fatal("single-file torrent should be at <dir>/one.bin.part")
	}
}

func TestContractPadFiles(t *testing.T) {
	info, flat := buildInfo("hybrid", 32768,
		tfile{path: []string{"a.bin"}, size: 1000},
		tfile{path: []string{".pad", "31768"}, size: 31768, pad: true},
		tfile{path: []string{"b.bin"}, size: 40000})
	dir := t.TempDir()
	s := mustOpen(t, dir, info, Options{})
	if s.WantedBytes(0) != 1000 || s.WantedBytes(1) != 32768 || s.WantedBytes(2) != 40000-32768 || s.WantedBytes(3) != 0 {
		t.Fatalf("WantedBytes = %d %d %d", s.WantedBytes(0), s.WantedBytes(1), s.WantedBytes(2))
	}
	// Write piece 0 with garbage in the pad range: it must be discarded.
	p0 := bytes.Clone(pieceData(info, flat, 0))
	for i := 1000; i < len(p0); i++ {
		p0[i] = 0xee
	}
	if err := s.WriteBlock(0, 0, p0[:16384]); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteBlock(0, 16384, p0[16384:]); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 32768)
	if err := s.ReadBlock(0, 0, buf); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(buf, pieceData(info, flat, 0)) {
		t.Fatal("pad ranges must read as zeros")
	}
	for i := 0; i < info.NumPieces(); i++ {
		if i > 0 {
			writePiece(t, s, info, flat, i)
		}
		if ok, err := s.VerifyPiece(i); err != nil || !ok {
			t.Fatalf("VerifyPiece(%d) = %v, %v", i, ok, err)
		}
		if err := s.MarkVerified(i); err != nil {
			t.Fatal(err)
		}
	}
	s.Close()
	var names []string
	filepath.Walk(dir, func(p string, fi os.FileInfo, err error) error {
		if err == nil && !fi.IsDir() {
			rel, _ := filepath.Rel(dir, p)
			names = append(names, rel)
		}
		return nil
	})
	want := map[string]bool{filepath.Join("hybrid", "a.bin"): true, filepath.Join("hybrid", "b.bin"): true}
	if len(names) != 2 || !want[names[0]] || !want[names[1]] {
		t.Fatalf("files on disk = %v, want only a.bin and b.bin", names)
	}
	if exists(filepath.Join(dir, "hybrid", ".pad")) {
		t.Fatal("a pad directory was created")
	}
}

func TestContractOpenRejects(t *testing.T) {
	info, _ := buildInfo("x.bin", 16384, tfile{path: []string{"x.bin"}, size: 100})
	if _, err := Open("relative/dir", info, Options{}); err == nil {
		t.Error("Open with a relative dir must fail")
	}
	evil, _ := buildInfo("evil", 16384, tfile{path: []string{"ok.bin"}, size: 10}, tfile{path: []string{"x"}, size: 10})
	evil.Files[1].Path = []string{"evil", "..", "..", "escape.bin"}
	dir := filepath.Join(t.TempDir(), "d")
	if _, err := Open(dir, evil, Options{}); !errors.Is(err, ErrUnsafePath) {
		t.Errorf("Open with an escaping path = %v, want ErrUnsafePath", err)
	}
	if exists(filepath.Join(filepath.Dir(dir), "escape.bin")) {
		t.Error("a file was created outside dir")
	}
}

func TestContractRangeAndClose(t *testing.T) {
	info, _ := buildInfo("r.bin", 16384, tfile{path: []string{"r.bin"}, size: 20000})
	s := mustOpen(t, t.TempDir(), info, Options{})
	if err := s.WriteBlock(1, 0, make([]byte, 3617)); !errors.Is(err, ErrOutOfRange) {
		t.Errorf("write past the last piece end = %v, want ErrOutOfRange", err)
	}
	if err := s.WriteBlock(2, 0, make([]byte, 1)); !errors.Is(err, ErrOutOfRange) {
		t.Errorf("write to piece 2 of 2 = %v, want ErrOutOfRange", err)
	}
	if err := s.ReadBlock(0, 16000, make([]byte, 1000)); !errors.Is(err, ErrOutOfRange) {
		t.Errorf("read past the piece end = %v, want ErrOutOfRange", err)
	}
	if _, err := s.VerifyPiece(-1); !errors.Is(err, ErrOutOfRange) {
		t.Errorf("VerifyPiece(-1) = %v, want ErrOutOfRange", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Errorf("second Close = %v, want nil", err)
	}
	if err := s.WriteBlock(0, 0, []byte{1}); !errors.Is(err, ErrClosed) {
		t.Errorf("WriteBlock after Close = %v, want ErrClosed", err)
	}
}

func TestContractExistingDataKept(t *testing.T) {
	info, flat := buildInfo("keep.bin", 16384, tfile{path: []string{"keep.bin"}, size: 30000})
	dir := t.TempDir()
	part := filepath.Join(dir, "keep.bin"+PartSuffix)
	if err := os.WriteFile(part, flat[:16384], 0o644); err != nil {
		t.Fatal(err)
	}
	s := mustOpen(t, dir, info, Options{})
	if ok, err := s.VerifyPiece(0); err != nil || !ok {
		t.Fatalf("existing .part data must be kept: VerifyPiece(0) = %v, %v", ok, err)
	}
	if st, _ := os.Stat(part); st == nil || st.Size() != 30000 {
		t.Fatalf("a shorter existing .part must be extended to the file length")
	}
}

// openFDs counts the process's open file descriptors, or -1 if unknown.
func openFDs() int {
	for _, d := range []string{"/dev/fd", "/proc/self/fd"} {
		if es, err := os.ReadDir(d); err == nil {
			return len(es)
		}
	}
	return -1
}

func TestContractOpenFileLimit(t *testing.T) {
	if runtime.GOOS == "windows" || openFDs() < 0 {
		t.Skip("no fd listing on this platform")
	}
	var files []tfile
	for i := 0; i < 300; i++ {
		files = append(files, tfile{path: []string{"many", "f" + string(rune('a'+i%26)) + string(rune('a'+i/26))}, size: 3000})
	}
	info, flat := buildInfo("many", 16384, files...)
	base := openFDs()
	var peak atomic.Int64
	stop := make(chan struct{})
	var sampler sync.WaitGroup
	sampler.Add(1)
	go func() {
		defer sampler.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			if n := int64(openFDs() - base); n > peak.Load() {
				peak.Store(n)
			}
			runtime.Gosched()
		}
	}()
	s := mustOpen(t, t.TempDir(), info, Options{MaxOpenFiles: 8, DiskWorkers: 8})
	var wg sync.WaitGroup
	sem := make(chan struct{}, 16)
	for i := 0; i < info.NumPieces(); i++ {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			writePiece(t, s, info, flat, i)
			if ok, err := s.VerifyPiece(i); err != nil || !ok {
				t.Errorf("VerifyPiece(%d) = %v, %v", i, ok, err)
				return
			}
			if err := s.MarkVerified(i); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	close(stop)
	sampler.Wait()
	// Allow a little slack for the sampler's own directory handle.
	if p := peak.Load(); p > 8+3 {
		t.Errorf("up to %d extra file descriptors were open, want <= MaxOpenFiles (8)", p)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if n := openFDs() - base; n > 1 {
		t.Errorf("%d file descriptors still open after Close", n)
	}
}

// ---- M4 (ADR-0017, ADR-0019) ----

func TestContractPoolBoundsAndClose(t *testing.T) {
	btest.CheckGoroutines(t)
	p := NewPool(3)
	if p.Workers() != 3 {
		t.Fatalf("Workers = %d", p.Workers())
	}
	var running, peak atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 30; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p.Run(context.Background(), func() {
				n := running.Add(1)
				for {
					old := peak.Load()
					if n <= old || peak.CompareAndSwap(old, n) {
						break
					}
				}
				time.Sleep(2 * time.Millisecond)
				running.Add(-1)
			})
		}()
	}
	wg.Wait()
	if peak.Load() > 3 || peak.Load() < 2 {
		t.Fatalf("peak concurrency %d with 3 workers", peak.Load())
	}
	// Run with a done ctx while the pool is busy never runs fn.
	gate := make(chan struct{})
	for i := 0; i < 3; i++ {
		if err := p.Submit(func() { <-gate }); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	ran := false
	if err := p.Run(ctx, func() { ran = true }); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Run on a busy pool with an expiring ctx = %v", err)
	}
	var dropped atomic.Bool
	p.Submit(func() { dropped.Store(true) }) // queued behind the gate
	go func() { time.Sleep(50 * time.Millisecond); close(gate) }()
	p.Close() // waits for the 3 running fns, drops the queued one
	p.Close()
	if ran || dropped.Load() {
		t.Fatal("a cancelled Run or a queued Submit ran after Close")
	}
	if err := p.Submit(func() {}); !errors.Is(err, ErrClosed) {
		t.Fatalf("Submit after Close = %v", err)
	}
	if err := p.Run(context.Background(), func() {}); !errors.Is(err, ErrClosed) {
		t.Fatalf("Run after Close = %v", err)
	}
}

// Storage work runs on the shared pools: with the single disk worker
// blocked, WriteBlock cannot complete; with the hash worker blocked,
// VerifyPiece cannot.
func TestContractStorageUsesSharedPools(t *testing.T) {
	btest.CheckGoroutines(t)
	info, flat := buildInfo("pools.bin", 32768, tfile{path: []string{"pools.bin"}, size: 65536})
	pools := NewPools(1, 1)
	defer pools.Close()
	dir := t.TempDir()
	s := mustOpen(t, dir, info, Options{Pools: pools})
	for name, tc := range map[string]struct {
		pool Pool
		op   func() error
	}{
		"disk": {pools.Disk, func() error { return s.WriteBlock(0, 0, flat[:16384]) }},
		"hash": {pools.Hash, func() error { _, err := s.VerifyPiece(1); return err }},
	} {
		gate := make(chan struct{})
		tc.pool.Submit(func() { <-gate })
		done := make(chan error, 1)
		go func() { done <- tc.op() }()
		select {
		case err := <-done:
			t.Fatalf("%s: operation finished (%v) while the only %s worker was busy", name, err, name)
		case <-time.After(100 * time.Millisecond):
		}
		close(gate)
		if err := <-done; err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	writePiece(t, s, info, flat, 0)
	res := make(chan bool, 1)
	if err := s.VerifyPieceAsync(0, func(ok bool, err error) {
		if err != nil {
			t.Error(err)
		}
		res <- ok
	}); err != nil {
		t.Fatal(err)
	}
	if !<-res {
		t.Fatal("VerifyPieceAsync: a correctly written piece failed")
	}
	if err := s.Sync(); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if err := s.Sync(); !errors.Is(err, ErrClosed) {
		t.Fatalf("Sync after Close = %v", err)
	}
	if err := s.VerifyPieceAsync(0, func(bool, error) {}); !errors.Is(err, ErrClosed) {
		t.Fatalf("VerifyPieceAsync after Close = %v", err)
	}
	// The shared pools are not closed by a Storage.
	if err := pools.Disk.Run(context.Background(), func() {}); err != nil {
		t.Fatalf("Storage.Close closed the shared pools: %v", err)
	}
}

func TestContractPrivatePoolsClosed(t *testing.T) {
	btest.CheckGoroutines(t)
	info, _ := buildInfo("priv.bin", 32768, tfile{path: []string{"priv.bin"}, size: 40000})
	s, err := Open(t.TempDir(), info, Options{})
	if err != nil {
		t.Fatal(err)
	}
	s.Close() // must stop its private pools' workers
}

func TestContractStatFingerprint(t *testing.T) {
	info, flat := buildInfo("st", 32768,
		tfile{path: []string{"a.bin"}, size: 40000},
		tfile{path: []string{"pad"}, size: 25536, pad: true},
		tfile{path: []string{"b.bin"}, size: 30000})
	dir := filepath.Join(t.TempDir(), "never-created")
	fp, err := Stat(dir, info)
	if err != nil {
		t.Fatal(err)
	}
	if len(fp) != 2 || fp[0].Exists || fp[1].Exists || fp[0].Path != "st/a.bin" || fp[1].Path != "st/b.bin" {
		t.Fatalf("Stat of nothing = %+v", fp)
	}
	if exists(dir) {
		t.Fatal("Stat created the download dir")
	}
	s := mustOpen(t, dir, info, Options{})
	writePiece(t, s, info, flat, 0)
	fp, err = Stat(dir, info)
	if err != nil {
		t.Fatal(err)
	}
	if !fp[0].Exists || fp[0].Size != 40000 || fp[0].ModTime == 0 || !fp[1].Exists || fp[1].Size != 30000 {
		t.Fatalf("Stat after Open = %+v", fp)
	}
	later := time.Now().Add(time.Hour)
	os.Chtimes(filepath.Join(dir, "st", "b.bin"+PartSuffix), later, later)
	fp2, _ := Stat(dir, info)
	if fp2[1].ModTime != later.UnixNano() && fp2[1].ModTime/1e9 != later.Unix() {
		t.Fatalf("mtime %d, want %d", fp2[1].ModTime, later.UnixNano())
	}
}

func TestContractPlanResume(t *testing.T) {
	// Pieces of 32 KiB: file a = pieces 0-1 (and half of 2), b = 2-3.
	info, _ := buildInfo("pr", 32768,
		tfile{path: []string{"a.bin"}, size: 80000},
		tfile{path: []string{"b.bin"}, size: 51072})
	if info.NumPieces() != 4 {
		t.Fatalf("setup: %d pieces", info.NumPieces())
	}
	bf := func(idx ...int) *piece.Bitfield {
		b := piece.NewBitfield(4)
		for _, i := range idx {
			b.Set(i)
		}
		return b
	}
	saved := Fingerprint{{Path: "pr/a.bin", Exists: true, Size: 80000, ModTime: 100}, {Path: "pr/b.bin", Exists: true, Size: 51072, ModTime: 200}}
	mod := func(f func(fp Fingerprint)) Fingerprint {
		c := append(Fingerprint(nil), saved...)
		f(c)
		return c
	}
	all := bf(0, 1, 2, 3)
	for _, tc := range []struct {
		name    string
		in      ResumeInput
		missing bool
		trusted []int
		recheck []int
	}{
		{"clean, unchanged", ResumeInput{Saved: all, SavedFingerprint: saved, Current: saved, Clean: true}, false, []int{0, 1, 2, 3}, nil},
		{"clean, b mtime changed", ResumeInput{Saved: all, SavedFingerprint: saved, Current: mod(func(f Fingerprint) { f[1].ModTime = 999 }), Clean: true}, false, []int{0, 1}, []int{2, 3}},
		{"clean, no saved fingerprint", ResumeInput{Saved: bf(1), Current: saved, Clean: true}, false, nil, []int{1}},
		{"unclean, mtimes ignored", ResumeInput{Saved: bf(0, 3), SavedFingerprint: saved, Current: mod(func(f Fingerprint) { f[0].ModTime, f[1].ModTime = 1, 2 }), Clean: false}, false, []int{0, 3}, nil},
		{"unclean, a resized", ResumeInput{Saved: all, SavedFingerprint: saved, Current: mod(func(f Fingerprint) { f[0].Size = 10 }), Clean: false}, false, []int{3}, []int{0, 1, 2}},
		{"b missing", ResumeInput{Saved: all, SavedFingerprint: saved, Current: mod(func(f Fingerprint) { f[1] = FileStat{Path: "pr/b.bin"} }), Clean: true}, false, []int{0, 1}, nil},
		{"all missing", ResumeInput{Saved: bf(2), SavedFingerprint: saved, Current: Fingerprint{{Path: "pr/a.bin"}, {Path: "pr/b.bin"}}, Clean: true}, true, nil, nil},
		{"fresh add, nothing on disk", ResumeInput{Current: Fingerprint{{Path: "pr/a.bin"}, {Path: "pr/b.bin"}}, Clean: true}, false, nil, nil},
		{"fresh add over existing data", ResumeInput{Current: mod(func(f Fingerprint) { f[1] = FileStat{Path: "pr/b.bin"} }), Clean: true}, false, nil, []int{0, 1, 2}},
	} {
		tc.in.Info = info
		got := PlanResume(tc.in)
		if got.FilesMissing != tc.missing {
			t.Errorf("%s: FilesMissing = %v", tc.name, got.FilesMissing)
			continue
		}
		var trusted []int
		if got.Trusted != nil {
			for i := 0; i < 4; i++ {
				if got.Trusted.Has(i) {
					trusted = append(trusted, i)
				}
			}
		}
		if !tc.missing && (!slices.Equal(trusted, tc.trusted) || !slices.Equal(got.Recheck, tc.recheck)) {
			t.Errorf("%s: trusted %v recheck %v, want %v %v", tc.name, trusted, got.Recheck, tc.trusted, tc.recheck)
		}
	}
}
