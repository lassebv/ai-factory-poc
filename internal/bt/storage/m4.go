package storage

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"torrentd/internal/bt/metainfo"
	"torrentd/internal/bt/piece"
)

// run executes fn on pool p and waits. Work abandoned because the storage
// or the pool was closed before it started reports ErrClosed.
func (s *storage) run(p Pool, fn func() error) error {
	var ferr error
	err := p.Run(s.ctx, func() { ferr = fn() })
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return ErrClosed
		}
		return err
	}
	return ferr
}

// onDisk runs fn on the disk pool.
func (s *storage) onDisk(fn func() error) error { return s.run(s.pools.Disk, fn) }

// markDirty records the files overlapping [off, off+n) as needing a Sync.
func (s *storage) markDirty(off, n int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.each(off, n, func(fi int, _, _, _ int64) {
		if !s.files[fi].pad {
			s.dirty[fi] = true
		}
	})
}

// VerifyPieceAsync implements Storage.
func (s *storage) VerifyPieceAsync(index int, done func(ok bool, err error)) error {
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if closed {
		return ErrClosed
	}
	size := s.PieceSize(index)
	if size == 0 {
		return fmt.Errorf("%w: piece %d", ErrOutOfRange, index)
	}
	return s.pools.Hash.Submit(func() {
		if err := s.enter(); err != nil {
			done(false, err)
			return
		}
		ok, err := s.hashPiece(index, size)
		s.leave()
		done(ok, err)
	})
}

// Sync implements Storage: it fsyncs every file written since the last
// Sync, reopening files whose handle was evicted.
func (s *storage) Sync() error {
	if err := s.enter(); err != nil {
		return err
	}
	defer s.leave()
	return s.onDisk(func() error {
		s.mu.Lock()
		var files []int
		for fi := range s.dirty {
			files = append(files, fi)
		}
		clear(s.dirty)
		s.mu.Unlock()
		var firstErr error
		for _, fi := range files {
			err := s.syncFile(fi)
			if err != nil {
				s.mu.Lock()
				s.dirty[fi] = true
				s.mu.Unlock()
				if firstErr == nil {
					firstErr = err
				}
			}
		}
		return firstErr
	})
}

func (s *storage) syncFile(fi int) error {
	h, err := s.acquire(fi)
	if err != nil {
		return err
	}
	defer s.release(h)
	if err := h.f.Sync(); err != nil {
		return fmt.Errorf("storage: sync %s: %w", s.files[fi].path, err)
	}
	return nil
}

func stat(dir string, info *metainfo.Info) (Fingerprint, error) {
	if info == nil {
		return nil, errors.New("storage: nil info")
	}
	var fp Fingerprint
	for _, f := range info.Files {
		if f.Pad {
			continue
		}
		rel := filepath.Join(f.Path...)
		if len(f.Path) == 0 || !filepath.IsLocal(rel) {
			return nil, fmt.Errorf("%w: %q", ErrUnsafePath, f.Path)
		}
		fs := FileStat{Path: strings.Join(f.Path, "/")}
		p := filepath.Join(dir, rel)
		for _, cand := range []string{p, p + PartSuffix} {
			st, err := os.Stat(cand)
			if err != nil {
				if errors.Is(err, os.ErrNotExist) {
					continue
				}
				return nil, fmt.Errorf("storage: stat %s: %w", cand, err)
			}
			if !st.Mode().IsRegular() {
				continue
			}
			fs.Exists, fs.Size, fs.ModTime = true, st.Size(), st.ModTime().UnixNano()
			break
		}
		fp = append(fp, fs)
	}
	return fp, nil
}

// fileSpan is a non-pad file's piece range for planning.
type fileSpan struct {
	first, last int // pieces overlapped; last < first for empty files
	length      int64
}

func planResume(in ResumeInput) ResumePlan {
	info := in.Info
	n := info.NumPieces()
	var spans []fileSpan
	for _, f := range info.Files {
		if f.Pad {
			continue
		}
		sp := fileSpan{first: 0, last: -1, length: f.Length}
		if f.Length > 0 && info.PieceLength > 0 {
			sp.first = int(f.Offset / info.PieceLength)
			sp.last = int((f.Offset + f.Length - 1) / info.PieceLength)
		}
		spans = append(spans, sp)
	}
	cur := func(k int) FileStat {
		if k < len(in.Current) {
			return in.Current[k]
		}
		return FileStat{}
	}
	trusted := piece.NewBitfield(n)
	plan := ResumePlan{Trusted: trusted}

	if in.Saved == nil {
		on := make([]bool, n)
		for k, sp := range spans {
			if c := cur(k); c.Exists && c.Size > 0 {
				for i := sp.first; i <= sp.last; i++ {
					on[i] = true
				}
			}
		}
		for i, b := range on {
			if b {
				plan.Recheck = append(plan.Recheck, i)
			}
		}
		return plan
	}

	if in.Saved.Count() > 0 {
		all, some := true, false
		for k, sp := range spans {
			if sp.length == 0 {
				continue
			}
			some = true
			if cur(k).Exists {
				all = false
			}
		}
		if some && all {
			return ResumePlan{FilesMissing: true, Trusted: trusted}
		}
	}

	fpOK := len(in.SavedFingerprint) == len(spans)
	missing := make([]bool, n)
	suspect := make([]bool, n)
	for k, sp := range spans {
		c := cur(k)
		var sus, miss bool
		switch {
		case !c.Exists:
			sus, miss = sp.length > 0, sp.length > 0
		case in.Clean:
			sus = !fpOK || c.Size != in.SavedFingerprint[k].Size || c.ModTime != in.SavedFingerprint[k].ModTime
		case fpOK:
			sus = c.Size != in.SavedFingerprint[k].Size
		default:
			sus = c.Size != sp.length
		}
		for i := sp.first; i <= sp.last; i++ {
			suspect[i] = suspect[i] || sus
			missing[i] = missing[i] || miss
		}
	}
	for i := 0; i < n; i++ {
		if !in.Saved.Has(i) {
			continue
		}
		switch {
		case !suspect[i]:
			trusted.Set(i)
		case !missing[i]:
			plan.Recheck = append(plan.Recheck, i)
		}
	}
	return plan
}
