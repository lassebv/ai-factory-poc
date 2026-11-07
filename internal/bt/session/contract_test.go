package session

// FROZEN CONTRACT TESTS (ADR-0011). Owned by the architect. In-process,
// loopback-only end-to-end checks of the Session against a fake HTTP tracker
// and a raw-wire fake seeder written here (independent of package peer).

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"torrentd/internal/bt/bencode"
	"torrentd/internal/bt/metainfo"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

type tfile struct {
	path []string
	size int
}

type testTorrent struct {
	mi       *metainfo.MetaInfo
	flat     []byte
	pieceLen int
}

func (tt *testTorrent) piece(i int) []byte {
	off := i * tt.pieceLen
	return tt.flat[off:min(off+tt.pieceLen, len(tt.flat))]
}

func makeTorrent(t *testing.T, name string, pieceLen int, announce string, files ...tfile) *testTorrent {
	t.Helper()
	rng := rand.New(rand.NewPCG(uint64(len(name)), 7))
	var flat []byte
	var list []map[string]any
	for _, f := range files {
		b := make([]byte, f.size)
		for i := range b {
			b[i] = byte(rng.Uint32())
		}
		flat = append(flat, b...)
		list = append(list, map[string]any{"length": f.size, "path": f.path})
	}
	var pieces []byte
	for off := 0; off < len(flat); off += pieceLen {
		h := sha1.Sum(flat[off:min(off+pieceLen, len(flat))])
		pieces = append(pieces, h[:]...)
	}
	info := map[string]any{"name": name, "piece length": pieceLen, "pieces": pieces}
	if len(files) == 1 {
		info["length"] = files[0].size
	} else {
		info["files"] = list
	}
	raw, err := bencode.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	data, err := bencode.Marshal(map[string]any{"announce": announce, "info": bencode.RawMessage(raw)})
	if err != nil {
		t.Fatal(err)
	}
	mi, err := metainfo.Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	return &testTorrent{mi: mi, flat: flat, pieceLen: pieceLen}
}

// announce is one request seen by the fake tracker.
type announce struct {
	at    time.Time
	q     url.Values
	event string
}

type fakeTracker struct {
	srv   *httptest.Server
	mu    sync.Mutex
	got   []announce
	peers []peerAddr
}

type peerAddr struct {
	ip   [4]byte
	port uint16
}

func newTracker(t *testing.T) *fakeTracker {
	ft := &fakeTracker{}
	ft.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		ft.mu.Lock()
		ft.got = append(ft.got, announce{time.Now(), q, q.Get("event")})
		var compact []byte
		for _, p := range ft.peers {
			compact = append(compact, p.ip[:]...)
			compact = binary.BigEndian.AppendUint16(compact, p.port)
		}
		ft.mu.Unlock()
		body, _ := bencode.Marshal(map[string]any{"interval": 1800, "peers": compact})
		w.Write(body)
	}))
	t.Cleanup(ft.srv.Close)
	return ft
}

func (ft *fakeTracker) url() string { return ft.srv.URL + "/announce" }

func (ft *fakeTracker) setPeers(ports ...int) {
	ft.mu.Lock()
	defer ft.mu.Unlock()
	ft.peers = nil
	for _, p := range ports {
		ft.peers = append(ft.peers, peerAddr{[4]byte{127, 0, 0, 1}, uint16(p)})
	}
}

func (ft *fakeTracker) waitEvent(t *testing.T, event string, timeout time.Duration) announce {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		ft.mu.Lock()
		for _, a := range ft.got {
			if a.event == event {
				ft.mu.Unlock()
				return a
			}
		}
		n := len(ft.got)
		ft.mu.Unlock()
		if time.Now().After(deadline) {
			t.Fatalf("no announce with event=%q within %s (%d announces)", event, timeout, n)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (ft *fakeTracker) count() int {
	ft.mu.Lock()
	defer ft.mu.Unlock()
	return len(ft.got)
}

// seeder is a minimal BEP 3 seeder speaking the raw wire format.
type seeder struct {
	ln          net.Listener
	tt          *testTorrent
	mu          sync.Mutex
	corruptOnce map[int]bool
	handshakes  [][68]byte
	badRequests []string
	wg          sync.WaitGroup
}

func newSeeder(t *testing.T, tt *testTorrent, corrupt ...int) *seeder {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &seeder{ln: ln, tt: tt, corruptOnce: map[int]bool{}}
	for _, c := range corrupt {
		s.corruptOnce[c] = true
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			s.wg.Add(1)
			go func() { defer s.wg.Done(); s.serve(c) }()
		}
	}()
	t.Cleanup(func() { ln.Close(); s.wg.Wait() })
	return s
}

func (s *seeder) port() int { return s.ln.Addr().(*net.TCPAddr).Port }

func (s *seeder) serve(c net.Conn) {
	defer c.Close()
	c.SetDeadline(time.Now().Add(60 * time.Second))
	var hs [68]byte
	if _, err := io.ReadFull(c, hs[:]); err != nil {
		return
	}
	s.mu.Lock()
	s.handshakes = append(s.handshakes, hs)
	s.mu.Unlock()
	if hs[0] != 19 || string(hs[1:20]) != "BitTorrent protocol" || !bytes.Equal(hs[28:48], s.tt.mi.InfoHash[:]) {
		return
	}
	out := append([]byte{19}, "BitTorrent protocol"...)
	out = append(out, make([]byte, 8)...)
	out = append(out, s.tt.mi.InfoHash[:]...)
	out = append(out, "-ST0001-000000000000"...)
	n := s.tt.mi.Info.NumPieces()
	bf := make([]byte, (n+7)/8)
	for i := 0; i < n; i++ {
		bf[i/8] |= 0x80 >> (i % 8)
	}
	out = append(out, msg(5, bf)...)
	if _, err := c.Write(out); err != nil {
		return
	}
	for {
		var lb [4]byte
		if _, err := io.ReadFull(c, lb[:]); err != nil {
			return
		}
		l := binary.BigEndian.Uint32(lb[:])
		if l == 0 {
			continue
		}
		if l > 1<<18 {
			return
		}
		m := make([]byte, l)
		if _, err := io.ReadFull(c, m); err != nil {
			return
		}
		switch m[0] {
		case 2:
			c.Write(msg(1, nil))
		case 6:
			if len(m) != 13 {
				s.bad("request length %d", len(m))
				return
			}
			idx, begin, length := binary.BigEndian.Uint32(m[1:]), binary.BigEndian.Uint32(m[5:]), binary.BigEndian.Uint32(m[9:])
			if int(idx) >= n || length == 0 || length > 16384 || int(begin)+int(length) > len(s.tt.piece(int(idx))) {
				s.bad("invalid request %d/%d/%d", idx, begin, length)
				continue
			}
			block := bytes.Clone(s.tt.piece(int(idx))[begin : begin+length])
			s.mu.Lock()
			if s.corruptOnce[int(idx)] && begin == 0 {
				delete(s.corruptOnce, int(idx))
				block[0] ^= 0xff
			}
			s.mu.Unlock()
			p := binary.BigEndian.AppendUint32(nil, idx)
			p = binary.BigEndian.AppendUint32(p, begin)
			if _, err := c.Write(msg(7, append(p, block...))); err != nil {
				return
			}
		}
	}
}

func (s *seeder) bad(format string, a ...any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.badRequests = append(s.badRequests, fmt.Sprintf(format, a...))
}

func msg(id byte, payload []byte) []byte {
	b := binary.BigEndian.AppendUint32(nil, uint32(1+len(payload)))
	return append(append(b, id), payload...)
}

func startSession(t *testing.T) Session {
	t.Helper()
	s, err := New(Config{Logger: quiet, Rand: rand.New(rand.NewPCG(1, 2))})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close(context.Background()) })
	return s
}

func waitState(t *testing.T, s Session, id [20]byte, want State, timeout time.Duration) Status {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var st Status
	for time.Now().Before(deadline) {
		var ok bool
		st, ok = s.Get(id)
		if !ok {
			t.Fatal("Get: torrent not found")
		}
		if st.State == want {
			return st
		}
		if st.State == StateError {
			t.Fatalf("torrent in state error: %s", st.ErrorMessage)
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("state %s, want %s after %s (progress %.3f)", st.State, want, timeout, st.Progress)
	return st
}

func TestContractSessionDownload(t *testing.T) {
	base := runtime.NumGoroutine()
	tr := newTracker(t)
	tt := makeTorrent(t, "multi", 32768, tr.url(),
		tfile{[]string{"first.bin"}, 70000}, tfile{[]string{"empty.txt"}, 0},
		tfile{[]string{"dir", "small.bin"}, 5000}, tfile{[]string{"dir", "big.bin"}, 131079})
	sd := newSeeder(t, tt, 2)
	tr.setPeers(sd.port())

	s, err := New(Config{Logger: quiet, Rand: rand.New(rand.NewPCG(1, 2))})
	if err != nil {
		t.Fatal(err)
	}
	if s.PeerPort() <= 0 || s.ListenErr() != nil {
		t.Fatalf("PeerPort = %d, ListenErr = %v", s.PeerPort(), s.ListenErr())
	}
	if c, err := net.Dial("tcp", "127.0.0.1:"+strconv.Itoa(s.PeerPort())); err != nil {
		t.Fatalf("the peer port is not listening: %v", err)
	} else {
		c.Close()
	}
	dir := filepath.Join(t.TempDir(), "a", "b")
	st, err := s.Add(context.Background(), tt.mi, AddOptions{DownloadDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if st.ID != tt.mi.InfoHash || st.Name != "multi" || st.DownloadDir != dir || st.SizeBytes != int64(len(tt.flat)) || st.AddedAt.IsZero() {
		t.Fatalf("initial status = %+v", st)
	}
	if st.State != StateChecking && st.State != StateDownloading && st.State != StateSeeding {
		t.Fatalf("initial state %s", st.State)
	}
	final := waitState(t, s, tt.mi.InfoHash, StateSeeding, 30*time.Second)
	if final.Progress != 1 || final.CompletedBytes != final.SizeBytes || final.CompletedAt.IsZero() {
		t.Errorf("final status = %+v", final)
	}
	if final.WastedTotal < int64(tt.pieceLen) {
		t.Errorf("WastedTotal = %d, want >= %d (one corrupted piece)", final.WastedTotal, tt.pieceLen)
	}
	if final.DownloadedTotal < int64(len(tt.flat))+int64(tt.pieceLen) {
		t.Errorf("DownloadedTotal = %d, want >= %d", final.DownloadedTotal, len(tt.flat)+tt.pieceLen)
	}
	for _, f := range tt.mi.Info.Files {
		p := filepath.Join(append([]string{dir}, f.Path...)...)
		got, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		if !bytes.Equal(got, tt.flat[f.Offset:f.Offset+f.Length]) {
			t.Errorf("%s has wrong content", p)
		}
		if _, err := os.Stat(p + ".part"); err == nil {
			t.Errorf("%s.part remains", p)
		}
	}
	sd.mu.Lock()
	if len(sd.badRequests) > 0 {
		t.Errorf("invalid requests: %v", sd.badRequests)
	}
	hs := sd.handshakes[0]
	sd.mu.Unlock()
	pid := s.PeerID()
	if !bytes.Equal(hs[48:68], pid[:]) || !strings.HasPrefix(string(pid[:]), "-TD0100-") {
		t.Errorf("handshake peer id %q, PeerID() %q", hs[48:68], pid)
	}

	started := tr.waitEvent(t, "started", time.Second)
	if started.q.Get("left") != strconv.Itoa(len(tt.flat)) || started.q.Get("port") != strconv.Itoa(s.PeerPort()) ||
		started.q.Get("peer_id") != string(pid[:]) || started.q.Get("compact") != "1" || started.q.Get("numwant") != "50" {
		t.Errorf("started announce = %v", started.q)
	}
	done := tr.waitEvent(t, "completed", 5*time.Second)
	if done.q.Get("left") != "0" || done.q.Get("key") != started.q.Get("key") || done.q.Get("key") == "" {
		t.Errorf("completed announce = %v", done.q)
	}
	if l := s.List(); len(l) != 1 || l[0].ID != tt.mi.InfoHash {
		t.Errorf("List = %+v", l)
	}

	closeStart := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.Close(ctx); err != nil {
		t.Errorf("Close: %v", err)
	}
	stopped := tr.waitEvent(t, "stopped", time.Second)
	if d := stopped.at.Sub(closeStart); d > ShutdownAnnounceTimeout {
		t.Errorf("stopped announce after %s", d)
	}
	if err := s.Close(ctx); err != nil {
		t.Errorf("second Close: %v", err)
	}
	if _, ok := s.Get(tt.mi.InfoHash); !ok {
		t.Error("Get after Close should still return the last status")
	}
	if _, err := s.Add(context.Background(), tt.mi, AddOptions{DownloadDir: dir}); !errors.Is(err, ErrClosed) && !errors.Is(err, ErrAlreadyExists) {
		t.Errorf("Add after Close = %v, want ErrClosed", err)
	}
	tr.srv.CloseClientConnections()
	deadline := time.Now().Add(3 * time.Second)
	for runtime.NumGoroutine() > base+4 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if n := runtime.NumGoroutine(); n > base+4 {
		buf := make([]byte, 1<<16)
		t.Errorf("goroutines after Close: %d, before: %d\n%s", n, base, buf[:runtime.Stack(buf, true)])
	}
}

func TestContractSessionAddRules(t *testing.T) {
	tr := newTracker(t)
	s := startSession(t)
	a := makeTorrent(t, "a.bin", 16384, tr.url(), tfile{[]string{"a.bin"}, 20000})
	b := makeTorrent(t, "bb.bin", 16384, tr.url(), tfile{[]string{"bb.bin"}, 20000})
	if _, err := s.Add(context.Background(), b.mi, AddOptions{DownloadDir: "relative/dir"}); !errors.Is(err, ErrInvalidDownloadDir) {
		t.Fatalf("relative dir: %v, want ErrInvalidDownloadDir", err)
	}
	if _, ok := s.Get(b.mi.InfoHash); ok || len(s.List()) != 0 {
		t.Fatal("a rejected Add must not register the torrent")
	}
	dir := filepath.Join(t.TempDir(), "x")
	st, err := s.Add(context.Background(), a.mi, AddOptions{DownloadDir: dir, StartPaused: true})
	if err != nil {
		t.Fatal(err)
	}
	if st.State != StatePaused {
		t.Fatalf("StartPaused state = %s, want paused", st.State)
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		t.Fatal("the download dir should be created")
	}
	if _, err := s.Add(context.Background(), a.mi, AddOptions{DownloadDir: "relative"}); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("duplicate: %v, want ErrAlreadyExists (checked before the dir)", err)
	}
	time.Sleep(300 * time.Millisecond)
	if st, _ := s.Get(a.mi.InfoHash); st.State != StatePaused || tr.count() != 0 {
		t.Fatalf("a paused torrent has no network activity: state=%s announces=%d", st.State, tr.count())
	}
	if _, ok := s.Get([20]byte{1}); ok {
		t.Fatal("Get of an unknown id must report false")
	}
}

func TestContractSessionBusyPort(t *testing.T) {
	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port
	s, err := New(Config{PeerPort: port, Logger: quiet})
	if err != nil {
		t.Fatalf("New with a busy port must still succeed: %v", err)
	}
	defer s.Close(context.Background())
	if s.ListenErr() == nil {
		t.Fatal("ListenErr must report the busy port")
	}
}
