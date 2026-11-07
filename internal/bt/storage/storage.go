// Package storage maps a torrent's pieces onto its files (§4.1 Storage):
// pieces may span file boundaries, files are sparsely preallocated as
// <file>.part and renamed once all of their pieces verify (G21), BEP 47 pad
// files never touch the disk, and open handles are bounded by an LRU (D9).
package storage

import (
	"crypto/sha1"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"torrentd/internal/bt/metainfo"
)

// PartSuffix marks a file whose pieces are not all verified yet.
const PartSuffix = ".part"

// DefaultMaxHandles is the default open-file limit (D9).
const DefaultMaxHandles = 128

// ErrPieceRange is returned for a piece index or length that doesn't fit
// the torrent.
var ErrPieceRange = errors.New("storage: piece out of range")

type file struct {
	final     string // absolute final path
	length    int64
	offset    int64
	pad       bool
	remaining int // pieces not yet verified
	done      bool
}

func (f *file) path() string {
	if f.done {
		return f.final
	}
	return f.final + PartSuffix
}

// Storage reads and writes the data of one torrent. Its methods are safe
// for concurrent use by the disk workers.
type Storage struct {
	dir       string
	pieceLen  int64
	total     int64
	numPieces int
	hashes    []byte
	files     []*file // in offset order, pad files included
	cache     *handleCache

	mu       sync.Mutex // guards file.remaining, file.done, verified and closed
	verified []bool
	closed   bool
}

// ErrClosed is returned for I/O after Close.
var ErrClosed = errors.New("storage: closed")

// Open prepares the files of info under dir: missing directories are
// created and every non-empty file is preallocated sparsely as
// <file>.part. Empty files are created under their final name right away,
// since they have no pieces to wait for. maxHandles <= 0 means
// DefaultMaxHandles.
func Open(dir string, info *metainfo.Info, maxHandles int) (*Storage, error) {
	if maxHandles <= 0 {
		maxHandles = DefaultMaxHandles
	}
	s := &Storage{
		dir:       dir,
		pieceLen:  info.PieceLength,
		total:     info.TotalLength,
		numPieces: info.NumPieces(),
		hashes:    info.Pieces,
		cache:     newHandleCache(maxHandles),
		verified:  make([]bool, info.NumPieces()),
	}
	for _, mf := range info.Files {
		rel := filepath.Join(mf.Path...)
		if !filepath.IsLocal(rel) {
			return nil, fmt.Errorf("storage: unsafe path %q", mf.PathString())
		}
		f := &file{final: filepath.Join(dir, rel), length: mf.Length, offset: mf.Offset, pad: mf.Pad}
		if f.length > 0 {
			first, last := s.pieceSpan(f)
			f.remaining = last - first + 1
		}
		s.files = append(s.files, f)
		if f.pad {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(f.final), 0o755); err != nil {
			return nil, err
		}
		if f.length == 0 {
			f.done = true
			h, err := os.OpenFile(f.final, os.O_RDWR|os.O_CREATE, 0o644)
			if err != nil {
				return nil, err
			}
			h.Close()
			continue
		}
		if err := preallocate(f.path(), f.length); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// preallocate creates path if needed and sets its size without writing
// data, so the file is sparse on filesystems that support it.
func preallocate(path string, size int64) error {
	h, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}
	st, err := h.Stat()
	if err == nil && st.Size() != size {
		err = h.Truncate(size)
	}
	if cerr := h.Close(); err == nil {
		err = cerr
	}
	return err
}

// pieceSpan returns the first and last piece a non-empty file overlaps.
func (s *Storage) pieceSpan(f *file) (int, int) {
	return int(f.offset / s.pieceLen), int((f.offset + f.length - 1) / s.pieceLen)
}

// NumPieces returns the number of pieces.
func (s *Storage) NumPieces() int { return s.numPieces }

// PieceSize returns the size of piece i.
func (s *Storage) PieceSize(i int) int64 {
	if i == s.numPieces-1 {
		return s.total - int64(i)*s.pieceLen
	}
	return s.pieceLen
}

// segment is the part of a piece that falls in one file.
type segment struct {
	f       *file
	fileOff int64 // offset within the file
	bufOff  int64 // offset within the piece
	n       int64
}

// segments returns the file ranges covered by piece i, in order.
func (s *Storage) segments(i int) ([]segment, error) {
	if i < 0 || i >= s.numPieces {
		return nil, ErrPieceRange
	}
	start := int64(i) * s.pieceLen
	end := start + s.PieceSize(i)
	k := sort.Search(len(s.files), func(j int) bool { return s.files[j].offset+s.files[j].length > start })
	var out []segment
	for ; k < len(s.files) && s.files[k].offset < end; k++ {
		f := s.files[k]
		if f.length == 0 {
			continue
		}
		lo, hi := max(start, f.offset), min(end, f.offset+f.length)
		out = append(out, segment{f: f, fileOff: lo - f.offset, bufOff: lo - start, n: hi - lo})
	}
	return out, nil
}

// VisibleBytes returns how many bytes of piece i belong to non-pad files.
func (s *Storage) VisibleBytes(i int) int64 {
	segs, _ := s.segments(i)
	var n int64
	for _, sg := range segs {
		if !sg.f.pad {
			n += sg.n
		}
	}
	return n
}

// WritePiece writes the data of piece i. Pad-file ranges are skipped.
func (s *Storage) WritePiece(i int, data []byte) error {
	segs, err := s.segments(i)
	if err != nil {
		return err
	}
	if int64(len(data)) != s.PieceSize(i) {
		return ErrPieceRange
	}
	for _, sg := range segs {
		if sg.f.pad {
			continue
		}
		if err := s.io(sg, data, true); err != nil {
			return err
		}
	}
	return nil
}

// ReadPiece reads piece i into buf, which must be PieceSize(i) long.
// Pad-file ranges read as zeros.
func (s *Storage) ReadPiece(i int, buf []byte) error {
	segs, err := s.segments(i)
	if err != nil {
		return err
	}
	if int64(len(buf)) != s.PieceSize(i) {
		return ErrPieceRange
	}
	for _, sg := range segs {
		if sg.f.pad {
			clear(buf[sg.bufOff : sg.bufOff+sg.n])
			continue
		}
		if err := s.io(sg, buf, false); err != nil {
			return err
		}
	}
	return nil
}

func (s *Storage) io(sg segment, buf []byte, write bool) error {
	s.mu.Lock()
	path, closed := sg.f.path(), s.closed
	s.mu.Unlock()
	if closed {
		return ErrClosed
	}
	h, err := s.cache.acquire(path)
	if err != nil {
		return err
	}
	defer s.cache.release(h)
	b := buf[sg.bufOff : sg.bufOff+sg.n]
	if write {
		_, err = h.f.WriteAt(b, sg.fileOff)
	} else {
		_, err = h.f.ReadAt(b, sg.fileOff)
	}
	return err
}

// Verify reports whether data matches the SHA-1 of piece i.
func (s *Storage) Verify(i int, data []byte) bool {
	if i < 0 || i >= s.numPieces {
		return false
	}
	sum := sha1.Sum(data)
	return string(sum[:]) == string(s.hashes[i*sha1.Size:(i+1)*sha1.Size])
}

// MarkVerified records that piece i is verified and on disk. Every file
// whose pieces are now all verified is closed and renamed from .part to its
// final name; their final paths are returned.
func (s *Storage) MarkVerified(i int) ([]string, error) {
	segs, err := s.segments(i)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.verified[i] {
		return nil, nil
	}
	s.verified[i] = true
	var renamed []string
	var errs []error
	for _, sg := range segs {
		f := sg.f
		f.remaining--
		if f.remaining > 0 || f.pad || f.done {
			continue
		}
		part := f.path()
		if err := s.cache.evict(part); err != nil {
			errs = append(errs, err)
		}
		if err := os.Rename(part, f.final); err != nil {
			errs = append(errs, err)
			continue
		}
		f.done = true
		renamed = append(renamed, f.final)
	}
	return renamed, errors.Join(errs...)
}

// Close closes every open handle. Later reads and writes fail with
// ErrClosed.
func (s *Storage) Close() error {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	return s.cache.closeAll()
}
