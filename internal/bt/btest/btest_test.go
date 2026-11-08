package btest

import (
	"context"
	"errors"
	"testing"
	"time"

	"torrentd/internal/store"
)

// fakeTB records failures of CheckGoroutines' cleanup.
type fakeTB struct {
	testing.TB
	cleanups []func()
	failed   bool
}

func (f *fakeTB) Helper()               { f.TB.Helper() }
func (f *fakeTB) Cleanup(fn func())     { f.cleanups = append(f.cleanups, fn) }
func (f *fakeTB) Errorf(string, ...any) { f.failed = true }
func (f *fakeTB) run() {
	for i := len(f.cleanups) - 1; i >= 0; i-- {
		f.cleanups[i]()
	}
}

func TestCheckGoroutinesDetectsLeak(t *testing.T) {
	CheckGoroutines(t)
	f := &fakeTB{TB: t}
	CheckGoroutines(f)
	stop := make(chan struct{})
	go func() { <-stop }()
	go func() { time.Sleep(6 * time.Second); close(stop) }()
	f.run()
	if !f.failed {
		t.Fatal("a leaked goroutine was not reported")
	}
	<-stop
	g := &fakeTB{TB: t}
	CheckGoroutines(g, "ignored-substring")
	g.run()
	if g.failed {
		t.Fatal("no leak, but one was reported")
	}
}

func TestMemStore(t *testing.T) {
	ctx := context.Background()
	m := NewMemStore()
	r := store.TorrentRecord{InfoHash: [20]byte{2}, Name: "b", Bitfield: []byte{1}, QueuePosition: 1}
	if err := m.SaveTorrents(ctx, []store.TorrentRecord{r}); err != nil {
		t.Fatal(err)
	}
	m.Put(store.TorrentRecord{InfoHash: [20]byte{1}, Name: "a"})
	recs, err := m.LoadTorrents(ctx)
	if err != nil || len(recs) != 2 || m.Saves() != 1 {
		t.Fatalf("load %v %v saves %d", recs, err, m.Saves())
	}
	recs[0].Name = "changed"
	if got, ok := m.Torrent([20]byte{2}); !ok || got.Name != "b" {
		t.Fatalf("Torrent = %+v", got)
	}
	now := time.Now()
	m.SaveBan(ctx, store.Ban{IP: "10.0.0.1", ExpiresAt: now.Add(time.Hour)})
	m.SaveBan(ctx, store.Ban{IP: "10.0.0.2", ExpiresAt: now.Add(-time.Hour)})
	bans, _ := m.LoadBans(ctx, now)
	if len(bans) != 1 || bans[0].IP != "10.0.0.1" || len(m.Bans()) < 1 {
		t.Fatalf("bans %v", bans)
	}
	m.FailSaves = true
	if err := m.SaveTorrents(ctx, nil); !errors.Is(err, ErrInjected) {
		t.Fatal(err)
	}
	if err := m.SaveBan(ctx, store.Ban{}); !errors.Is(err, ErrInjected) {
		t.Fatal(err)
	}
}
