package storage

import (
	"container/list"
	"fmt"
	"os"
)

// handle is a cached open file. refs counts the goroutines currently using
// it; a handle with refs > 0 is never closed. A stale handle (its file was
// renamed while in use) is closed by its last user.
type handle struct {
	file  int // index into storage.files
	f     *os.File
	refs  int
	stale bool
	elem  *list.Element // position in storage.lru while cached
}

// acquire returns an open handle for file fi with its reference count
// incremented. It blocks while MaxOpenFiles handles are open and all of
// them are in use. The caller must call release.
func (s *storage) acquire(fi int) (*handle, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for {
		if s.closed {
			return nil, ErrClosed
		}
		if h := s.cache[fi]; h != nil {
			h.refs++
			s.lru.MoveToFront(h.elem)
			return h, nil
		}
		if s.open < s.maxOpen {
			break
		}
		if !s.evictOne() {
			s.cond.Wait()
		}
	}
	// Opening under the lock keeps the open count exact and avoids opening
	// the same file twice. Opens are cheap compared to the I/O itself.
	f, err := os.OpenFile(s.files[fi].diskPath(), os.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("storage: open %s: %w", s.files[fi].diskPath(), err)
	}
	s.open++
	h := &handle{file: fi, f: f, refs: 1}
	h.elem = s.lru.PushFront(h)
	s.cache[fi] = h
	return h, nil
}

// release drops one reference to h, closing it if it is stale and unused.
func (s *storage) release(h *handle) {
	s.mu.Lock()
	defer s.mu.Unlock()
	h.refs--
	if h.refs == 0 && h.stale {
		h.f.Close()
		s.open--
	}
	s.cond.Broadcast()
}

// evictOne closes the least recently used idle handle. It reports false if
// every open handle is in use. s.mu must be held.
func (s *storage) evictOne() bool {
	for e := s.lru.Back(); e != nil; e = e.Prev() {
		h := e.Value.(*handle)
		if h.refs == 0 {
			s.drop(h)
			return true
		}
	}
	return false
}

// drop removes h from the cache, closing it now if idle or marking it stale
// so that its last user closes it. s.mu must be held.
func (s *storage) drop(h *handle) error {
	s.lru.Remove(h.elem)
	delete(s.cache, h.file)
	if h.refs > 0 {
		h.stale = true
		return nil
	}
	s.open--
	return h.f.Close()
}
