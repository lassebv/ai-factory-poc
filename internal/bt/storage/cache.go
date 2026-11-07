package storage

import (
	"container/list"
	"errors"
	"os"
	"sync"
)

// handle is one cached open file.
type handle struct {
	path   string
	f      *os.File
	refs   int
	doomed bool // evicted while in use: close on the last release
	el     *list.Element
}

// handleCache keeps at most max files open. Handles in use are never
// closed; the least recently used idle handle is closed to make room.
type handleCache struct {
	mu  sync.Mutex
	max int
	m   map[string]*handle
	lru *list.List // idle and busy handles, most recently used at the front
	closed bool
}

func newHandleCache(max int) *handleCache {
	return &handleCache{max: max, m: map[string]*handle{}, lru: list.New()}
}

// acquire returns an open handle for path, opening (and creating) the file
// if needed.
func (c *handleCache) acquire(path string) (*handle, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, ErrClosed
	}
	if h := c.m[path]; h != nil {
		h.refs++
		c.lru.MoveToFront(h.el)
		return h, nil
	}
	for c.lru.Len() >= c.max {
		if !c.evictIdle() {
			break // every handle is busy; go over the limit briefly
		}
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, err
	}
	h := &handle{path: path, f: f, refs: 1}
	h.el = c.lru.PushFront(h)
	c.m[path] = h
	return h, nil
}

// evictIdle closes the least recently used idle handle.
func (c *handleCache) evictIdle() bool {
	for e := c.lru.Back(); e != nil; e = e.Prev() {
		if h := e.Value.(*handle); h.refs == 0 {
			c.remove(h)
			h.f.Close()
			return true
		}
	}
	return false
}

func (c *handleCache) remove(h *handle) {
	c.lru.Remove(h.el)
	delete(c.m, h.path)
}

func (c *handleCache) release(h *handle) {
	c.mu.Lock()
	defer c.mu.Unlock()
	h.refs--
	if h.refs == 0 && h.doomed {
		h.f.Close()
	}
}

// evict closes the handle for path, if open.
func (c *handleCache) evict(path string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	h := c.m[path]
	if h == nil {
		return nil
	}
	c.remove(h)
	if h.refs > 0 {
		h.doomed = true
		return nil
	}
	return h.f.Close()
}

// open returns the number of cached handles.
func (c *handleCache) open() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lru.Len()
}

func (c *handleCache) closeAll() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	var errs []error
	for _, h := range c.m {
		if err := h.f.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	c.m = map[string]*handle{}
	c.lru.Init()
	return errors.Join(errs...)
}
