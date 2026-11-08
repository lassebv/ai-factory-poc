// Package btest holds shared test helpers for the engine packages: an
// in-repo goroutine-leak checker (§4.5, goleak-style) and an in-memory fake
// of the session's persistence (session.Store).
//
// Owned by the architect (shared test helpers, ADR-0017). Product code must
// never import this package; only _test.go files may.
package btest

import (
	"bytes"
	"context"
	"errors"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"torrentd/internal/store"
)

// defaultIgnore are goroutines that are not leaks of the code under test.
var defaultIgnore = []string{
	"testing.(*T).Run",
	"testing.(*M).",
	"testing.tRunner",
	"testing.runFuzzing",
	"runtime.goexit0",
	"os/signal.signal_recv",
	"os/signal.loop",
	"runtime.ensureSigM",
	"created by runtime.gc",
	"runtime.MHeap_Scavenger",
	"interestingGoroutines",
}

// stacks returns the stack of every goroutine except the caller's, keyed
// by goroutine id.
func stacks() map[string]string {
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			buf = buf[:n]
			break
		}
		buf = make([]byte, 2*len(buf))
	}
	out := map[string]string{}
	for i, g := range bytes.Split(buf, []byte("\n\n")) {
		if i == 0 {
			continue // the calling goroutine
		}
		s := string(g)
		id, _, _ := strings.Cut(strings.TrimPrefix(s, "goroutine "), " ")
		out[id] = s
	}
	return out
}

// CheckGoroutines snapshots the running goroutines and registers a cleanup
// that fails t if, after the test and every cleanup registered after this
// call, goroutines that did not exist at the snapshot are still running
// after a grace period of 5 s. Call it first in a test so that its cleanup
// runs last. Stacks containing any of ignore (or the testing runtime's own
// goroutines) are not leaks.
func CheckGoroutines(t testing.TB, ignore ...string) {
	t.Helper()
	before := stacks()
	ign := append(append([]string{}, defaultIgnore...), ignore...)
	t.Cleanup(func() {
		var leaked []string
		deadline := time.Now().Add(5 * time.Second)
		for {
			leaked = leaked[:0]
			for id, s := range stacks() {
				if _, ok := before[id]; ok || ignored(s, ign) {
					continue
				}
				leaked = append(leaked, s)
			}
			if len(leaked) == 0 || time.Now().After(deadline) {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		if len(leaked) > 0 {
			sort.Strings(leaked)
			t.Errorf("%d leaked goroutine(s):\n\n%s", len(leaked), strings.Join(leaked, "\n\n"))
		}
	})
}

func ignored(stack string, ign []string) bool {
	for _, s := range ign {
		if strings.Contains(stack, s) {
			return true
		}
	}
	return false
}

// ErrInjected is returned by MemStore when a failure is injected.
var ErrInjected = errors.New("btest: injected store failure")

// MemStore is an in-memory session.Store. It is safe for concurrent use.
// Records are deep-copied on the way in and out.
type MemStore struct {
	mu       sync.Mutex
	torrents map[[20]byte]store.TorrentRecord
	bans     map[string]store.Ban
	saves    int
	// FailSaves makes SaveTorrents and SaveBan fail with ErrInjected.
	FailSaves bool
}

// NewMemStore returns an empty MemStore.
func NewMemStore() *MemStore {
	return &MemStore{torrents: map[[20]byte]store.TorrentRecord{}, bans: map[string]store.Ban{}}
}

func cloneRec(r store.TorrentRecord) store.TorrentRecord {
	r.RawInfo = bytes.Clone(r.RawInfo)
	r.Bitfield = bytes.Clone(r.Bitfield)
	r.ResumeFingerprint = bytes.Clone(r.ResumeFingerprint)
	r.Trackers = append([]store.TrackerRecord(nil), r.Trackers...)
	r.Files = append([]store.FileRecord(nil), r.Files...)
	return r
}

// LoadTorrents returns the records ordered like store.Store does.
func (m *MemStore) LoadTorrents(ctx context.Context) ([]store.TorrentRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []store.TorrentRecord
	for _, r := range m.torrents {
		out = append(out, cloneRec(r))
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.QueuePosition != b.QueuePosition {
			return a.QueuePosition < b.QueuePosition
		}
		if !a.AddedAt.Equal(b.AddedAt) {
			return a.AddedAt.Before(b.AddedAt)
		}
		return bytes.Compare(a.InfoHash[:], b.InfoHash[:]) < 0
	})
	return out, nil
}

// SaveTorrents upserts recs atomically.
func (m *MemStore) SaveTorrents(ctx context.Context, recs []store.TorrentRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.FailSaves {
		return ErrInjected
	}
	for _, r := range recs {
		m.torrents[r.InfoHash] = cloneRec(r)
	}
	m.saves++
	return nil
}

// LoadBans drops expired bans and returns the rest.
func (m *MemStore) LoadBans(ctx context.Context, now time.Time) ([]store.Ban, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []store.Ban
	for ip, b := range m.bans {
		if !b.ExpiresAt.After(now) {
			delete(m.bans, ip)
			continue
		}
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].IP < out[j].IP })
	return out, nil
}

// SaveBan upserts b.
func (m *MemStore) SaveBan(ctx context.Context, b store.Ban) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.FailSaves {
		return ErrInjected
	}
	m.bans[b.IP] = b
	return nil
}

// Torrent returns the stored record for id.
func (m *MemStore) Torrent(id [20]byte) (store.TorrentRecord, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.torrents[id]
	return cloneRec(r), ok
}

// Put stores r directly (to seed a test).
func (m *MemStore) Put(r store.TorrentRecord) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.torrents[r.InfoHash] = cloneRec(r)
}

// Saves returns the number of successful SaveTorrents calls.
func (m *MemStore) Saves() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.saves
}

// Bans returns every stored ban, expired or not.
func (m *MemStore) Bans() []store.Ban {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []store.Ban
	for _, b := range m.bans {
		out = append(out, b)
	}
	return out
}
