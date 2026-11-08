package session

import (
	"context"
	"errors"
	"math/rand/v2"
	"net/netip"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// memStore is an in-memory Store.
type memStore struct {
	mu    sync.Mutex
	recs  map[[20]byte]Record
	bans  []Ban
	saves int
	fail  error
}

func newMemStore() *memStore { return &memStore{recs: map[[20]byte]Record{}} }

func (m *memStore) LoadTorrents(context.Context) ([]Record, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail != nil {
		return nil, m.fail
	}
	out := make([]Record, 0, len(m.recs))
	for _, r := range m.recs {
		out = append(out, r)
	}
	return out, nil
}

func (m *memStore) SaveTorrents(_ context.Context, recs []Record) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.saves++
	for _, r := range recs {
		m.recs[r.InfoHash] = r
	}
	return nil
}

func (m *memStore) LoadBans(_ context.Context, now time.Time) ([]Ban, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail != nil {
		return nil, m.fail
	}
	var out []Ban
	for _, b := range m.bans {
		if b.ExpiresAt.After(now) {
			out = append(out, b)
		}
	}
	return out, nil
}

func (m *memStore) SaveBan(_ context.Context, b Ban) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.bans = append(m.bans, b)
	return nil
}

func (m *memStore) get(id [20]byte) (Record, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.recs[id]
	return r, ok
}

var sessionSeed atomic.Uint64

func newStoreSession(t *testing.T, st Store, clean bool) Session {
	t.Helper()
	s, err := New(Config{Logger: quiet, Rand: rand.New(rand.NewPCG(sessionSeed.Add(1), 6)), Store: st, PreviousShutdownClean: clean, DiskWorkers: 2, HashWorkers: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close(context.Background()) })
	return s
}

// writeContent puts tt's data on disk under dir (single-file torrents).
func writeContent(t *testing.T, tt *testTorrent, dir string) string {
	t.Helper()
	p := filepath.Join(dir, tt.mi.Info.Name)
	if err := os.WriteFile(p, tt.flat, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func waitFor(t *testing.T, what string, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestSwarmSeedLeechLifecycleAndResume(t *testing.T) {
	tr := newTracker(t)
	tt := makeTorrent(t, "swarm.bin", 32768, tr.url(), tfile{[]string{"swarm.bin"}, 20*32768 + 777})
	id := tt.mi.InfoHash

	// Seeder: data already on disk -> checking -> seeding.
	seedStore := newMemStore()
	seed := newStoreSession(t, seedStore, true)
	seedDir := t.TempDir()
	writeContent(t, tt, seedDir)
	if _, err := seed.Add(context.Background(), tt.mi, AddOptions{DownloadDir: seedDir}); err != nil {
		t.Fatal(err)
	}
	st := waitState(t, seed, id, StateSeeding, 10*time.Second)
	if st.CompletedAt.IsZero() || st.Progress != 1 {
		t.Fatalf("seeding from disk: %+v", st)
	}
	if err := seed.Pause(id); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("Pause on a complete torrent = %v", err)
	}

	// Leecher downloads from the seeder alone.
	tr.setPeers(seed.PeerPort())
	leechStore := newMemStore()
	leech := newStoreSession(t, leechStore, true)
	leechDir := t.TempDir()
	if _, err := leech.Add(context.Background(), tt.mi, AddOptions{DownloadDir: leechDir}); err != nil {
		t.Fatal(err)
	}
	waitState(t, leech, id, StateSeeding, 20*time.Second)
	got, _ := os.ReadFile(filepath.Join(leechDir, "swarm.bin"))
	if string(got) != string(tt.flat) {
		t.Fatal("downloaded data differs")
	}
	waitFor(t, "uploadedTotal", 5*time.Second, func() bool { s, _ := seed.Get(id); return s.UploadedTotal >= int64(len(tt.flat)) })

	// Lifecycle on the seeder.
	if err := seed.StopSeeding(id); err != nil {
		t.Fatal(err)
	}
	if s, _ := seed.Get(id); s.State != StateStopped || s.StopReason != StopReasonUser || s.PeersConnected != 0 {
		t.Fatalf("after StopSeeding: %+v", s)
	}
	if err := seed.StopSeeding(id); err != nil {
		t.Fatalf("StopSeeding twice = %v", err)
	}
	if err := seed.Pause(id); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("Pause on stopped = %v", err)
	}
	if err := seed.Resume(id); err != nil {
		t.Fatal(err)
	}
	waitState(t, seed, id, StateSeeding, 5*time.Second)
	if err := seed.Resume(id); err != nil {
		t.Fatalf("Resume on active = %v", err)
	}
	var unknown [20]byte
	for _, f := range []func([20]byte) error{seed.Pause, seed.Resume, seed.StopSeeding} {
		if err := f(unknown); !errors.Is(err, ErrNotFound) {
			t.Fatalf("unknown id: %v", err)
		}
	}

	// Clean restart of the leecher: fast resume, totals kept.
	before, _ := leech.Get(id)
	leech.Close(context.Background())
	rec, ok := leechStore.get(id)
	if !ok || rec.Fingerprint == "" || rec.DownloadedTotal != before.DownloadedTotal {
		t.Fatalf("saved record: ok=%v %+v", ok, rec.DownloadedTotal)
	}
	leech2 := newStoreSession(t, leechStore, true)
	st = waitState(t, leech2, id, StateSeeding, 10*time.Second)
	if st.DownloadedTotal != before.DownloadedTotal {
		t.Fatalf("downloadedTotal %d, want %d", st.DownloadedTotal, before.DownloadedTotal)
	}
	if err := leech2.StopSeeding(id); err != nil {
		t.Fatal(err)
	}
	leech2.Close(context.Background())

	// Stopped survives a restart.
	leech3 := newStoreSession(t, leechStore, false)
	if s, _ := leech3.Get(id); s.State != StateStopped || s.StopReason != StopReasonUser || s.Progress != 1 {
		t.Fatalf("restored stopped torrent: %+v", s)
	}
	if err := leech3.Resume(id); err != nil {
		t.Fatal(err)
	}
	waitState(t, leech3, id, StateSeeding, 10*time.Second)
	leech3.Close(context.Background())

	// Changed mtime after a clean shutdown: full recheck finds the damage
	// and downloads the piece again from the seeder.
	p := filepath.Join(leechDir, "swarm.bin")
	data, _ := os.ReadFile(p)
	data[10] ^= 0xff
	os.WriteFile(p, data, 0o644)
	later := time.Now().Add(time.Minute)
	os.Chtimes(p, later, later)
	leech4 := newStoreSession(t, leechStore, true)
	waitState(t, leech4, id, StateSeeding, 20*time.Second)
	got, _ = os.ReadFile(p)
	if string(got) != string(tt.flat) {
		t.Fatal("recheck did not repair the data")
	}
	leech4.Close(context.Background())

	// All files missing -> error "Files missing"; Resume retries.
	os.Remove(p)
	leech5 := newStoreSession(t, leechStore, false)
	st = waitState(t, leech5, id, StateError, 10*time.Second)
	if st.ErrorMessage != FilesMissingMessage {
		t.Fatalf("error = %q", st.ErrorMessage)
	}
	if err := leech5.Pause(id); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("Pause in error = %v", err)
	}
	if err := leech5.Resume(id); err != nil {
		t.Fatal(err)
	}
	waitState(t, leech5, id, StateError, 10*time.Second)
}

func TestSwarmPauseResumeDownloadAndPersistPaused(t *testing.T) {
	tr := newTracker(t)
	tt := makeTorrent(t, "pause.bin", 16384, tr.url(), tfile{[]string{"pause.bin"}, 40 * 16384})
	id := tt.mi.InfoHash
	seed := newStoreSession(t, nil, true)
	seedDir := t.TempDir()
	writeContent(t, tt, seedDir)
	seed.Add(context.Background(), tt.mi, AddOptions{DownloadDir: seedDir})
	waitState(t, seed, id, StateSeeding, 10*time.Second)
	tr.setPeers(seed.PeerPort())

	store := newMemStore()
	leech := newStoreSession(t, store, true)
	if _, err := leech.Add(context.Background(), tt.mi, AddOptions{DownloadDir: t.TempDir(), StartPaused: true}); err != nil {
		t.Fatal(err)
	}
	if err := leech.Pause(id); err != nil {
		t.Fatalf("Pause on paused = %v", err)
	}
	if err := leech.StopSeeding(id); !errors.Is(err, ErrNotComplete) {
		t.Fatalf("StopSeeding on incomplete = %v", err)
	}
	leech.Close(context.Background())
	if r, _ := store.get(id); r.DesiredState != DesiredPaused {
		t.Fatalf("desired state %q", r.DesiredState)
	}
	leech2 := newStoreSession(t, store, true)
	if s, _ := leech2.Get(id); s.State != StatePaused {
		t.Fatalf("restored state %s", s.State)
	}
	if err := leech2.Resume(id); err != nil {
		t.Fatal(err)
	}
	waitState(t, leech2, id, StateSeeding, 20*time.Second)
}

func TestLoadErrorsAndBans(t *testing.T) {
	st := newMemStore()
	st.fail = errors.New("db down")
	s, err := New(Config{Logger: quiet, Store: st})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(context.Background())
	if err := s.Load(context.Background()); err == nil {
		t.Fatal("Load must report store errors")
	}
	st2 := newMemStore()
	now := time.Now()
	st2.bans = []Ban{{IP: mustAddr("10.0.0.1"), ExpiresAt: now.Add(time.Hour)}, {IP: mustAddr("10.0.0.2"), ExpiresAt: now.Add(-time.Hour)}}
	st2.recs[[20]byte{1}] = Record{InfoHash: [20]byte{1}, RawInfo: []byte("garbage")}
	s2 := newStoreSession(t, st2, true)
	ss := s2.(*session)
	if !ss.banned(mustAddr("10.0.0.1")) || ss.banned(mustAddr("10.0.0.2")) {
		t.Fatal("bans not restored")
	}
	if len(s2.List()) != 0 {
		t.Fatal("a record with a broken info dict must be skipped")
	}
	s2.Close(context.Background())
	if err := s2.Load(context.Background()); !errors.Is(err, ErrClosed) {
		t.Fatalf("Load after Close = %v", err)
	}
}

func mustAddr(s string) netip.Addr { return netip.MustParseAddr(s) }
