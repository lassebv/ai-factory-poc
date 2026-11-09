package storage

import (
	"bytes"
	"container/list"
	"context"
	"crypto/sha1"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"torrentd/internal/bt/metainfo"
)

// verifyChunk is the read size VerifyPiece uses, so that hashing a large
// piece does not need a piece-sized buffer (MEM-01).
const verifyChunk = MaxHashBuffer

// file is the on-disk state of one entry of info.Files.
type file struct {
	path   string // final path, filepath.Join(dir, Path...)
	offset int64
	length int64
	pad    bool
	// final is true once the file lives under its final name.
	final bool
	// unverified counts the pieces overlapping the file that are not yet
	// verified.
	unverified int
}

func (f *file) diskPath() string {
	if f.final {
		return f.path
	}
	return f.path + PartSuffix
}

// storage implements Storage.
type storage struct {
	info  *metainfo.Info
	files []file
	// ends[i] = files[i].offset + files[i].length, for binary search.
	ends []int64

	pools   *Pools
	private bool // pools are owned (and closed) by this storage
	// ctx is cancelled by Close so that queued pool work is abandoned.
	ctx    context.Context
	cancel context.CancelFunc

	mu       sync.Mutex
	cond     *sync.Cond
	closed   bool
	active   int // public calls in progress
	verified []bool
	cache    map[int]*handle
	lru      *list.List
	open     int
	maxOpen  int
	// dirty is the set of files written since the last Sync.
	dirty map[int]bool
}

func open(dir string, info *metainfo.Info, opts Options) (Storage, error) {
	if info == nil {
		return nil, errors.New("storage: nil info")
	}
	if !filepath.IsAbs(dir) {
		return nil, fmt.Errorf("storage: download dir %q is not absolute", dir)
	}
	if info.PieceLength <= 0 {
		return nil, fmt.Errorf("storage: invalid piece length %d", info.PieceLength)
	}
	dir = filepath.Clean(dir)
	for _, f := range info.Files {
		rel := filepath.Join(f.Path...)
		if len(f.Path) == 0 || !filepath.IsLocal(rel) {
			return nil, fmt.Errorf("%w: %q", ErrUnsafePath, f.Path)
		}
	}
	if opts.MaxOpenFiles <= 0 {
		opts.MaxOpenFiles = DefaultMaxOpenFiles
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("storage: create download dir: %w", err)
	}

	s := &storage{
		info:     info,
		files:    make([]file, len(info.Files)),
		ends:     make([]int64, len(info.Files)),
		pools:    opts.Pools,
		dirty:    map[int]bool{},
		verified: make([]bool, info.NumPieces()),
		cache:    make(map[int]*handle),
		lru:      list.New(),
		maxOpen:  opts.MaxOpenFiles,
	}
	s.cond = sync.NewCond(&s.mu)
	s.ctx, s.cancel = context.WithCancel(context.Background())
	for i, mf := range info.Files {
		f := &s.files[i]
		*f = file{
			path:   filepath.Join(append([]string{dir}, mf.Path...)...),
			offset: mf.Offset,
			length: mf.Length,
			pad:    mf.Pad,
		}
		s.ends[i] = mf.Offset + mf.Length
		if f.length > 0 {
			first := f.offset / info.PieceLength
			last := (f.offset + f.length - 1) / info.PieceLength
			f.unverified = int(last - first + 1)
		}
		if f.pad {
			continue
		}
		if err := prepare(f); err != nil {
			s.cancel()
			return nil, err
		}
	}
	if s.pools == nil {
		s.pools = NewPools(opts.DiskWorkers, opts.HashWorkers)
		s.private = true
	}
	return s, nil
}

// prepare creates the parent directories and the (sparse) file for f, or
// adopts an existing <path> or <path>.part. It holds at most one descriptor
// and closes it before returning.
func prepare(f *file) error {
	if err := os.MkdirAll(filepath.Dir(f.path), 0o755); err != nil {
		return fmt.Errorf("storage: create directory: %w", err)
	}
	if f.length == 0 {
		f.final = true
	} else if _, err := os.Stat(f.path); err == nil {
		f.final = true // an existing complete-named file is used in place
	}
	p := f.diskPath()
	h, err := os.OpenFile(p, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return fmt.Errorf("storage: create %s: %w", p, err)
	}
	st, err := h.Stat()
	if err == nil && st.Size() < f.length {
		err = h.Truncate(f.length) // extends sparsely; never shortens
	}
	if cerr := h.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("storage: preallocate %s: %w", p, err)
	}
	return nil
}

// enter registers a public call; it fails with ErrClosed after Close.
func (s *storage) enter() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	s.active++
	return nil
}

func (s *storage) leave() {
	s.mu.Lock()
	s.active--
	s.cond.Broadcast()
	s.mu.Unlock()
}

// PieceSize implements Storage.
func (s *storage) PieceSize(index int) int64 {
	if index < 0 || index >= len(s.verified) {
		return 0
	}
	off := int64(index) * s.info.PieceLength
	return min(s.info.PieceLength, s.info.TotalLength-off)
}

// WantedBytes implements Storage.
func (s *storage) WantedBytes(index int) int64 {
	size := s.PieceSize(index)
	if size == 0 {
		return 0
	}
	off := int64(index) * s.info.PieceLength
	var n int64
	s.each(off, size, func(fi int, _ int64, lo, hi int64) {
		if !s.files[fi].pad {
			n += hi - lo
		}
	})
	return n
}

// each calls fn for every non-empty file overlapping the flat range
// [off, off+n), with fileOff the offset within the file and [lo, hi) the
// sub-range relative to off.
func (s *storage) each(off, n int64, fn func(fi int, fileOff, lo, hi int64)) {
	end := off + n
	for fi := sort.Search(len(s.ends), func(i int) bool { return s.ends[i] > off }); fi < len(s.files); fi++ {
		f := &s.files[fi]
		if f.offset >= end {
			return
		}
		if f.length == 0 {
			continue
		}
		a, b := max(off, f.offset), min(end, f.offset+f.length)
		fn(fi, a-f.offset, a-off, b-off)
	}
}

// checkRange validates a block request and returns its flat offset.
func (s *storage) checkRange(index, begin, n int) (int64, error) {
	size := s.PieceSize(index)
	if size == 0 || begin < 0 || int64(begin)+int64(n) > size {
		return 0, fmt.Errorf("%w: piece %d begin %d length %d", ErrOutOfRange, index, begin, n)
	}
	return int64(index)*s.info.PieceLength + int64(begin), nil
}

// WriteBlock implements Storage.
func (s *storage) WriteBlock(index int, begin int, data []byte) error {
	if err := s.enter(); err != nil {
		return err
	}
	defer s.leave()
	off, err := s.checkRange(index, begin, len(data))
	if err != nil {
		return err
	}
	return s.onDisk(func() error {
		if err := s.io(off, data, true); err != nil {
			return err
		}
		s.markDirty(off, int64(len(data)))
		return nil
	})
}

// ReadBlock implements Storage.
func (s *storage) ReadBlock(index int, begin int, buf []byte) error {
	if err := s.enter(); err != nil {
		return err
	}
	defer s.leave()
	off, err := s.checkRange(index, begin, len(buf))
	if err != nil {
		return err
	}
	return s.onDisk(func() error { return s.io(off, buf, false) })
}

// io reads or writes the flat range [off, off+len(buf)). Pad ranges read as
// zeros and are not written. The caller holds a disk slot. At most one
// handle is held at a time, so concurrent callers cannot deadlock on the
// handle limit.
func (s *storage) io(off int64, buf []byte, write bool) error {
	var firstErr error
	s.each(off, int64(len(buf)), func(fi int, fileOff, lo, hi int64) {
		if firstErr != nil {
			return
		}
		part := buf[lo:hi]
		if s.files[fi].pad {
			if !write {
				clear(part)
			}
			return
		}
		h, err := s.acquire(fi)
		if err != nil {
			firstErr = err
			return
		}
		defer s.release(h)
		if write {
			_, err = h.f.WriteAt(part, fileOff)
		} else {
			var n int
			n, err = h.f.ReadAt(part, fileOff)
			if errors.Is(err, io.EOF) { // a file shorter than expected reads as zeros
				clear(part[n:])
				err = nil
			}
		}
		if err != nil {
			firstErr = fmt.Errorf("storage: %s: %w", s.files[fi].path, err)
		}
	})
	return firstErr
}

// VerifyPiece implements Storage.
func (s *storage) VerifyPiece(index int) (bool, error) {
	if err := s.enter(); err != nil {
		return false, err
	}
	defer s.leave()
	size := s.PieceSize(index)
	if size == 0 {
		return false, fmt.Errorf("%w: piece %d", ErrOutOfRange, index)
	}
	var ok bool
	var herr error
	if err := s.run(s.pools.Hash, func() error {
		ok, herr = s.hashPiece(index, size)
		return herr
	}); err != nil {
		return false, err
	}
	return ok, nil
}

// hashPiece streams piece index through SHA-1 with a bounded buffer. It
// runs on a hash worker and reads the files itself (not through Disk).
func (s *storage) hashPiece(index int, size int64) (bool, error) {
	off := int64(index) * s.info.PieceLength
	buf := make([]byte, min(size, verifyChunk))
	h := sha1.New()
	for done := int64(0); done < size; {
		chunk := buf[:min(int64(len(buf)), size-done)]
		if err := s.io(off+done, chunk, false); err != nil {
			return false, err
		}
		h.Write(chunk)
		done += int64(len(chunk))
	}
	want := s.info.PieceHash(index)
	return bytes.Equal(h.Sum(nil), want[:]), nil
}

// MarkVerified implements Storage.
func (s *storage) MarkVerified(index int) error {
	if err := s.enter(); err != nil {
		return err
	}
	defer s.leave()
	size := s.PieceSize(index)
	if size == 0 {
		return fmt.Errorf("%w: piece %d", ErrOutOfRange, index)
	}
	off := int64(index) * s.info.PieceLength

	s.mu.Lock()
	if s.verified[index] {
		s.mu.Unlock()
		return nil
	}
	s.verified[index] = true
	var complete []int
	s.each(off, size, func(fi int, _, _, _ int64) {
		f := &s.files[fi]
		f.unverified--
		if f.unverified == 0 && !f.pad && !f.final {
			complete = append(complete, fi)
		}
	})
	s.mu.Unlock()

	var firstErr error
	for _, fi := range complete {
		if err := s.finish(fi); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// finish renames file fi from <path>.part to <path>. The cached handle is
// dropped first (closed now, or by its last user), so later opens use the
// new name.
func (s *storage) finish(fi int) error {
	return s.onDisk(func() error { return s.rename(fi) })
}

func (s *storage) rename(fi int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	f := &s.files[fi]
	if h := s.cache[fi]; h != nil {
		s.drop(h)
		s.cond.Broadcast()
	}
	if err := os.Rename(f.path+PartSuffix, f.path); err != nil {
		return fmt.Errorf("storage: rename %s: %w", f.path+PartSuffix, err)
	}
	f.final = true
	return nil
}

// Close implements Storage. It waits for calls in progress, then closes
// every handle.
func (s *storage) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	s.cancel()
	s.cond.Broadcast() // wake acquirers so they observe closed
	for s.active > 0 {
		s.cond.Wait()
	}
	var firstErr error
	for _, h := range s.cache {
		if err := s.drop(h); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if s.private {
		s.mu.Unlock()
		s.pools.Close()
		s.mu.Lock()
	}
	return firstErr
}
