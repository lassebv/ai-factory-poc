package session

import (
	"bytes"
	"crypto/sha1"
	"encoding/binary"
	"io"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"torrentd/internal/bt/bencode"
	"torrentd/internal/bt/metainfo"
	"torrentd/internal/bt/peer"
	"torrentd/internal/clock"
)

var realClock = clock.Real()

// content builds a deterministic multi-file torrent announcing to url.
func content(t *testing.T, url string, pl int64, sizes ...int64) (*metainfo.MetaInfo, []byte) {
	t.Helper()
	var data []byte
	var files []map[string]any
	for i, n := range sizes {
		for j := int64(0); j < n; j++ {
			data = append(data, byte(i*7+int(j%253)))
		}
		files = append(files, map[string]any{"length": n, "path": []string{"f" + strconv.Itoa(i)}})
	}
	var pieces []byte
	for off := int64(0); off < int64(len(data)); off += pl {
		s := sha1.Sum(data[off:min(off+pl, int64(len(data)))])
		pieces = append(pieces, s[:]...)
	}
	info := map[string]any{"name": "tor", "piece length": pl, "pieces": pieces, "files": files}
	top := map[string]any{"info": info}
	if url != "" {
		top["announce"] = url
	}
	raw, err := bencode.Marshal(top)
	if err != nil {
		t.Fatal(err)
	}
	m, err := metainfo.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return m, data
}

// tracker is a minimal HTTP tracker that returns a fixed peer list.
type fakeTracker struct {
	srv    *httptest.Server
	mu     sync.Mutex
	events []string
	peers  []string
}

func newTracker(t *testing.T) *fakeTracker {
	tr := &fakeTracker{}
	tr.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tr.mu.Lock()
		tr.events = append(tr.events, r.URL.Query().Get("event"))
		peers := tr.peers
		tr.mu.Unlock()
		var compact []byte
		for _, p := range peers {
			ap, _ := net.ResolveTCPAddr("tcp", p)
			compact = append(compact, ap.IP.To4()...)
			compact = binary.BigEndian.AppendUint16(compact, uint16(ap.Port))
		}
		b, _ := bencode.Marshal(map[string]any{"interval": 1800, "complete": 3, "incomplete": 4, "peers": compact})
		w.Write(b)
	}))
	t.Cleanup(tr.srv.Close)
	return tr
}

func (tr *fakeTracker) url() string { return tr.srv.URL + "/announce" }

func (tr *fakeTracker) setPeers(p ...string) {
	tr.mu.Lock()
	tr.peers = p
	tr.mu.Unlock()
}

func (tr *fakeTracker) seen(ev string) bool {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	for _, e := range tr.events {
		if e == ev {
			return true
		}
	}
	return false
}

// seeder serves a torrent's data. Its behavior is scripted per test.
type seeder struct {
	ln       net.Listener
	m        *metainfo.MetaInfo
	data     []byte
	corrupt  map[int]bool // flip a byte the first time this piece is served
	preamble [][]byte     // raw frames sent before the bitfield
	mu       sync.Mutex
}

func newSeeder(t *testing.T, m *metainfo.MetaInfo, data []byte) *seeder {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &seeder{ln: ln, m: m, data: data, corrupt: map[int]bool{}}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go s.serve(c)
		}
	}()
	return s
}

func (s *seeder) addr() string { return s.ln.Addr().String() }

func (s *seeder) serve(c net.Conn) {
	defer c.Close()
	if _, err := peer.ReadHandshake(c); err != nil {
		return
	}
	c.Write(peer.Handshake{InfoHash: s.m.InfoHash, PeerID: [20]byte{'s'}}.Marshal())
	for _, f := range s.preamble {
		c.Write(f)
	}
	n := s.m.Info.NumPieces()
	bf := make([]byte, (n+7)/8)
	for i := 0; i < n; i++ {
		bf[i/8] |= 0x80 >> (i % 8)
	}
	c.Write((&peer.Message{ID: peer.MsgBitfield, Payload: bf}).Marshal())
	pl := s.m.Info.PieceLength
	for {
		m, err := peer.ReadMessage(c, 1<<20)
		if err != nil {
			return
		}
		if m == nil {
			continue
		}
		switch m.ID {
		case peer.MsgInterested:
			c.Write((&peer.Message{ID: peer.MsgUnchoke}).Marshal())
		case peer.MsgRequest:
			off := int64(m.Index)*pl + int64(m.Begin)
			block := append([]byte(nil), s.data[off:off+int64(m.Length)]...)
			s.mu.Lock()
			if s.corrupt[int(m.Index)] && m.Begin == 0 {
				delete(s.corrupt, int(m.Index))
				block[0] ^= 0xff
			}
			s.mu.Unlock()
			c.Write((&peer.Message{ID: peer.MsgPiece, Index: m.Index, Begin: m.Begin, Payload: block}).Marshal())
		}
	}
}

func newSession(t *testing.T) *Session {
	t.Helper()
	s, err := New(Config{
		PeerPort: 0,
		Rand:     rand.New(rand.NewPCG(1, 2)),
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := realClock.Now().Add(20 * time.Second)
	for !cond() {
		if realClock.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		<-realClock.After(10 * time.Millisecond)
	}
}

func verify(t *testing.T, dir string, m *metainfo.MetaInfo, data []byte) {
	t.Helper()
	for _, f := range m.Info.Files {
		got, err := os.ReadFile(filepath.Join(append([]string{dir}, f.Path...)...))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, data[f.Offset:f.Offset+f.Length]) {
			t.Fatalf("%s differs", f.PathString())
		}
	}
}

func TestDownloadLifecycle(t *testing.T) {
	tr := newTracker(t)
	m, data := content(t, tr.url(), 32768, 50000, 0, 70001)
	sd := newSeeder(t, m, data)
	sd.corrupt[1] = true
	// Unknown and keep-alive messages before the bitfield are harmless.
	sd.preamble = [][]byte{(*peer.Message)(nil).Marshal()}
	tr.setPeers(sd.addr())
	s := newSession(t)
	if s.Port() == 0 || peerIDPrefix(s) != PeerIDPrefix {
		t.Fatalf("port %d, peer id %q", s.Port(), s.PeerID())
	}
	dir := t.TempDir()
	st, err := s.Add(m, dir, false)
	if err != nil || st.ID != m.HexHash() || st.SizeBytes != 120001 || st.DownloadDir != dir {
		t.Fatalf("Add = %+v, %v", st, err)
	}
	if _, err := s.Add(m, dir, false); err != ErrExists {
		t.Fatalf("duplicate Add: %v", err)
	}
	waitFor(t, "seeding", func() bool { st, _ := s.Get(m.HexHash()); return st.State == StateSeeding })
	st, ok := s.Get(m.HexHash())
	if !ok || st.Progress != 1 || st.CompletedBytes != 120001 || st.Left != 0 || st.CompletedAt.IsZero() {
		t.Fatalf("final stats %+v", st)
	}
	if st.WastedTotal < 32768 || st.DownloadedTotal < 120001+32768 {
		t.Errorf("wasted %d downloaded %d, want the corrupted piece counted", st.WastedTotal, st.DownloadedTotal)
	}
	verify(t, dir, m, data)
	waitFor(t, "completed announce", func() bool { return tr.seen("completed") })
	waitFor(t, "swarm counts", func() bool { st, _ := s.Get(m.HexHash()); return st.SwarmSeeders == 3 })
	if l := s.List(); len(l) != 1 || l[0].ID != m.HexHash() {
		t.Fatalf("List = %+v", l)
	}
	if _, ok := s.Get("zz"); ok {
		t.Fatal("Get(bad id)")
	}
	if _, ok := s.Get("0000000000000000000000000000000000000000"); ok {
		t.Fatal("Get(unknown id)")
	}
	s.Close()
	if !tr.seen("stopped") || !tr.seen("started") {
		t.Fatal("no started/stopped announce")
	}
	if _, err := s.Add(m, dir, false); err == nil {
		t.Fatal("Add after Close")
	}
}

func TestPausedNoTrackersAndErrors(t *testing.T) {
	s := newSession(t)
	m, _ := content(t, "", 16384, 1000)
	st, err := s.Add(m, t.TempDir(), true)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "paused", func() bool { st, _ = s.Get(st.ID); return st.State == StatePaused })

	// No trackers: the torrent still starts and waits for peers.
	m2, _ := content(t, "udp://only.udp:80", 16384, 2000)
	if _, err := s.Add(m2, t.TempDir(), false); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "downloading", func() bool { st, _ := s.Get(m2.HexHash()); return st.State == StateDownloading })

	// A download dir that can't hold the files puts the torrent in error.
	blocker := filepath.Join(t.TempDir(), "file")
	os.WriteFile(blocker, nil, 0o644)
	m3, _ := content(t, "", 16384, 3000)
	if _, err := s.Add(m3, blocker, false); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "error", func() bool { st, _ := s.Get(m3.HexHash()); return st.State == StateError && st.ErrorMessage != "" })

	// The peer port is busy.
	if _, err := New(Config{PeerPort: s.Port()}); err == nil {
		t.Fatal("binding a busy port succeeded")
	}
}

func TestTrackerRetryAndBadPeers(t *testing.T) {
	// The tracker fails first, so the started announce is retried; it lists
	// misbehaving peers, an unreachable one and finally a good seeder.
	tr := newTracker(t)
	m, data := content(t, tr.url(), 16384, 40000)
	frames := map[string][]byte{
		"bad bitfield":   (&peer.Message{ID: peer.MsgBitfield, Payload: []byte{1, 2, 3, 4, 5}}).Marshal(),
		"have too large": (&peer.Message{ID: peer.MsgHave, Index: 99}).Marshal(),
		"garbage":        []byte{0, 0, 0, 3, 4, 0, 0},
	}
	addrs := []string{"127.0.0.1:1"}
	for _, f := range frames {
		bad := newSeeder(t, m, data)
		bad.preamble = [][]byte{f}
		addrs = append(addrs, bad.addr())
	}
	late := newSeeder(t, m, data) // sends its bitfield after a have
	late.preamble = [][]byte{(&peer.Message{ID: peer.MsgHave, Index: 0}).Marshal()}
	addrs = append(addrs, late.addr())
	good := newSeeder(t, m, data)
	good.preamble = [][]byte{(&peer.Message{ID: 42, Payload: []byte("x")}).Marshal(), (&peer.Message{ID: peer.MsgChoke}).Marshal()}
	tr.setPeers(append(addrs, good.addr())...)

	s := newSession(t)
	dir := t.TempDir()
	if _, err := s.Add(m, dir, false); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "seeding", func() bool { st, _ := s.Get(m.HexHash()); return st.State == StateSeeding })
	verify(t, dir, m, data)
}

func TestAnnounceFailureBackoff(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		w.Write([]byte("d14:failure reason4:nopee"))
	}))
	defer srv.Close()
	fake := clock.NewFake(realClock.Now())
	s, err := New(Config{Clock: fake, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	m, _ := content(t, srv.URL, 16384, 1000)
	if _, err := s.Add(m, t.TempDir(), false); err != nil {
		t.Fatal(err)
	}
	count := func() int { mu.Lock(); defer mu.Unlock(); return calls }
	waitFor(t, "first announce", func() bool { return count() >= 1 })
	for i := 0; i < 3; i++ {
		fake.Advance(time.Hour)
		<-realClock.After(20 * time.Millisecond)
	}
	waitFor(t, "retries", func() bool { return count() >= 2 })
}

func peerIDPrefix(s *Session) string { id := s.PeerID(); return string(id[:8]) }
