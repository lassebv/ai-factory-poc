package session

// FROZEN CONTRACT TESTS (ADR-0011, ADR-0017, ADR-0019). Owned by the architect. In-process,
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
	"torrentd/internal/bt/btest"
	"torrentd/internal/bt/metainfo"
	"torrentd/internal/store"
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
	dict  []string // "host:port"; when set, a dict peer list is served
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
		var dict []any
		for _, hp := range ft.dict {
			h, p, _ := net.SplitHostPort(hp)
			n, _ := strconv.Atoi(p)
			dict = append(dict, map[string]any{"ip": h, "port": n})
		}
		ft.mu.Unlock()
		var peers any = compact
		if dict != nil {
			peers = dict
		}
		body, _ := bencode.Marshal(map[string]any{"interval": 1800, "peers": peers})
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
	corruptAll  bool // every block is corrupted
	stall       bool // requests are accepted and never answered
	conns       int
	cancels     int
}

func newSeeder(t *testing.T, tt *testTorrent, corrupt ...int) *seeder {
	return newSeederAt(t, "127.0.0.1:0", tt, corrupt...)
}

func newSeederAt(t *testing.T, addr string, tt *testTorrent, corrupt ...int) *seeder {
	ln, err := net.Listen("tcp", addr)
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
	s.conns++
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
		case 8:
			s.mu.Lock()
			s.cancels++
			s.mu.Unlock()
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
			stall := s.stall
			if s.corruptAll {
				block[0] ^= 0xff
			}
			s.mu.Unlock()
			if stall {
				continue
			}
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

// ---- M4 (ADR-0016..0019) ----

func startWith(t *testing.T, cfg Config) Session {
	t.Helper()
	if cfg.Logger == nil {
		cfg.Logger = quiet
	}
	if cfg.Rand == nil {
		cfg.Rand = rand.New(rand.NewPCG(3, 4))
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close(context.Background()) })
	return s
}

// writeData puts the complete data of tt under dir.
func writeData(t *testing.T, tt *testTorrent, dir string) {
	t.Helper()
	for _, f := range tt.mi.Info.Files {
		if f.Pad {
			continue
		}
		p := filepath.Join(append([]string{dir}, f.Path...)...)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, tt.flat[f.Offset:f.Offset+f.Length], 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func readFull(c net.Conn) (byte, []byte, error) {
	var lb [4]byte
	for {
		if _, err := io.ReadFull(c, lb[:]); err != nil {
			return 0, nil, err
		}
		if l := binary.BigEndian.Uint32(lb[:]); l > 0 {
			if l > 1<<20 {
				return 0, nil, errors.New("message too large")
			}
			m := make([]byte, l)
			if _, err := io.ReadFull(c, m); err != nil {
				return 0, nil, err
			}
			return m[0], m[1:], nil
		}
	}
}

// leech downloads tt from 127.0.0.1:port over the raw wire and returns the
// flat data. It fails if it is not unchoked within 15 s.
func leech(port int, tt *testTorrent) ([]byte, error) {
	c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 5*time.Second)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(30 * time.Second))
	hs := append([]byte{19}, "BitTorrent protocol"...)
	hs = append(hs, make([]byte, 8)...)
	hs = append(hs, tt.mi.InfoHash[:]...)
	hs = append(hs, "-LE0001-000000000000"...)
	if _, err := c.Write(hs); err != nil {
		return nil, err
	}
	var rh [68]byte
	if _, err := io.ReadFull(c, rh[:]); err != nil {
		return nil, fmt.Errorf("no handshake reply: %w", err)
	}
	c.Write(msg(2, nil)) // interested
	unchokeBy := time.Now().Add(15 * time.Second)
	for {
		c.SetReadDeadline(unchokeBy)
		id, _, err := readFull(c)
		if err != nil {
			return nil, fmt.Errorf("waiting for unchoke: %w", err)
		}
		if id == 1 {
			break
		}
	}
	c.SetDeadline(time.Now().Add(30 * time.Second))
	out := make([]byte, len(tt.flat))
	n := tt.mi.Info.NumPieces()
	var reqs [][3]uint32
	for i := 0; i < n; i++ {
		pl := len(tt.piece(i))
		for b := 0; b < pl; b += 16384 {
			reqs = append(reqs, [3]uint32{uint32(i), uint32(b), uint32(min(16384, pl-b))})
		}
	}
	go func() {
		for _, r := range reqs {
			p := binary.BigEndian.AppendUint32(nil, r[0])
			p = binary.BigEndian.AppendUint32(p, r[1])
			p = binary.BigEndian.AppendUint32(p, r[2])
			if _, err := c.Write(msg(6, p)); err != nil {
				return
			}
		}
	}()
	for got := 0; got < len(reqs); {
		id, m, err := readFull(c)
		if err != nil {
			return nil, fmt.Errorf("after %d/%d blocks: %w", got, len(reqs), err)
		}
		if id != 7 || len(m) < 8 {
			continue
		}
		idx, begin := binary.BigEndian.Uint32(m), binary.BigEndian.Uint32(m[4:])
		copy(out[int64(idx)*int64(tt.pieceLen)+int64(begin):], m[8:])
		got++
	}
	return out, nil
}

func seedingTorrent(t *testing.T, tr *fakeTracker, name string) (*testTorrent, string) {
	tt := makeTorrent(t, name, 32768, tr.url(), tfile{[]string{"a.bin"}, 100000}, tfile{[]string{"b.bin"}, 70000})
	dir := t.TempDir()
	writeData(t, tt, dir)
	return tt, dir
}

func TestContractSessionSeedFromDisk(t *testing.T) {
	btest.CheckGoroutines(t)
	tr := newTracker(t)
	tt, dir := seedingTorrent(t, tr, "seed")
	s := startWith(t, Config{Store: btest.NewMemStore()})
	if _, err := s.Add(context.Background(), tt.mi, AddOptions{DownloadDir: dir}); err != nil {
		t.Fatal(err)
	}
	st := waitState(t, s, tt.mi.InfoHash, StateSeeding, 15*time.Second)
	if st.CompletedBytes != st.SizeBytes || st.CompletedAt.IsZero() || st.DownloadedTotal != 0 {
		t.Fatalf("seeding from disk: %+v", st)
	}
	tr.waitEvent(t, "started", 5*time.Second)
	got, err := leech(s.PeerPort(), tt)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, tt.flat) {
		t.Fatal("leecher received wrong data")
	}
	deadline := time.Now().Add(5 * time.Second)
	for st, _ = s.Get(tt.mi.InfoHash); st.UploadedTotal < int64(len(tt.flat)) && time.Now().Before(deadline); st, _ = s.Get(tt.mi.InfoHash) {
		time.Sleep(20 * time.Millisecond)
	}
	if st.UploadedTotal < int64(len(tt.flat)) {
		t.Fatalf("UploadedTotal = %d after serving %d bytes", st.UploadedTotal, len(tt.flat))
	}
	if err := s.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	tr.waitEvent(t, "stopped", time.Second)
}

func TestContractSessionLifecycleActions(t *testing.T) {
	btest.CheckGoroutines(t)
	ctx := context.Background()
	tr := newTracker(t)
	tt, dir := seedingTorrent(t, tr, "life")
	s := startWith(t, Config{Store: btest.NewMemStore()})
	var unknown [20]byte
	for name, f := range map[string]func(context.Context, [20]byte) error{"pause": s.Pause, "resume": s.Resume, "stop": s.StopSeeding} {
		if err := f(ctx, unknown); !errors.Is(err, ErrNotFound) {
			t.Errorf("%s on unknown id = %v, want ErrNotFound", name, err)
		}
	}
	s.Add(ctx, tt.mi, AddOptions{DownloadDir: dir})
	id := tt.mi.InfoHash
	waitState(t, s, id, StateSeeding, 15*time.Second)
	if err := s.Pause(ctx, id); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("pause on a complete torrent = %v, want ErrInvalidState", err)
	}
	if err := s.Resume(ctx, id); err != nil {
		t.Fatalf("resume on an active torrent = %v, want nil (no-op)", err)
	}
	if err := s.StopSeeding(ctx, id); err != nil {
		t.Fatal(err)
	}
	st, _ := s.Get(id)
	if st.State != StateStopped || st.StopReason != StopReasonUser || st.CompletedBytes != st.SizeBytes {
		t.Fatalf("after StopSeeding: state %s reason %q", st.State, st.StopReason)
	}
	if err := s.StopSeeding(ctx, id); err != nil {
		t.Fatalf("StopSeeding on stopped = %v, want nil", err)
	}
	tr.waitEvent(t, "stopped", 4*time.Second)
	if _, err := leech(s.PeerPort(), tt); err == nil {
		t.Fatal("a stopped torrent served a leecher")
	}
	if err := s.Resume(ctx, id); err != nil {
		t.Fatal(err)
	}
	if st, _ := s.Get(id); st.State != StateSeeding || st.StopReason != StopReasonNone {
		t.Fatalf("after Resume: %s %q", st.State, st.StopReason)
	}
	if got, err := leech(s.PeerPort(), tt); err != nil || !bytes.Equal(got, tt.flat) {
		t.Fatalf("leech after resume seeding: %v", err)
	}

	// An incomplete torrent (no peers): pause/resume, not stop-seeding.
	inc := makeTorrent(t, "inc", 32768, tr.url(), tfile{[]string{"x.bin"}, 50000})
	s.Add(ctx, inc.mi, AddOptions{DownloadDir: t.TempDir()})
	waitState(t, s, inc.mi.InfoHash, StateDownloading, 10*time.Second)
	if err := s.StopSeeding(ctx, inc.mi.InfoHash); !errors.Is(err, ErrNotComplete) {
		t.Fatalf("stop-seeding on incomplete = %v, want ErrNotComplete", err)
	}
	if err := s.Pause(ctx, inc.mi.InfoHash); err != nil {
		t.Fatal(err)
	}
	if st, _ := s.Get(inc.mi.InfoHash); st.State != StatePaused {
		t.Fatalf("after Pause: %s", st.State)
	}
	if err := s.Pause(ctx, inc.mi.InfoHash); err != nil {
		t.Fatalf("pause on paused = %v, want nil", err)
	}
	if err := s.Resume(ctx, inc.mi.InfoHash); err != nil {
		t.Fatal(err)
	}
	if st, _ := s.Get(inc.mi.InfoHash); st.State != StateDownloading {
		t.Fatalf("after Resume: %s", st.State)
	}
}

func TestContractSessionPersistence(t *testing.T) {
	btest.CheckGoroutines(t)
	ctx := context.Background()
	tr := newTracker(t)
	ms := btest.NewMemStore()
	a, dirA := seedingTorrent(t, tr, "persist-a")
	b := makeTorrent(t, "persist-b", 32768, tr.url(), tfile{[]string{"y.bin"}, 50000})
	s, err := New(Config{Logger: quiet, Store: ms, PreviousClean: true})
	if err != nil {
		t.Fatal(err)
	}
	s.Add(ctx, a.mi, AddOptions{DownloadDir: dirA})
	s.Add(ctx, b.mi, AddOptions{DownloadDir: t.TempDir(), StartPaused: true})
	waitState(t, s, a.mi.InfoHash, StateSeeding, 15*time.Second)
	waitState(t, s, b.mi.InfoHash, StatePaused, 10*time.Second)
	if _, ok := ms.Torrent(b.mi.InfoHash); !ok {
		t.Fatal("Add must persist the torrent before returning")
	}
	if err := s.StopSeeding(ctx, a.mi.InfoHash); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(ctx); err != nil {
		t.Fatal(err)
	}
	ra, _ := ms.Torrent(a.mi.InfoHash)
	rb, _ := ms.Torrent(b.mi.InfoHash)
	if ra.DesiredState != store.DesiredStopped || ra.StopReason != "user" || len(ra.Bitfield) == 0 || ra.ResumeFingerprint == nil || ra.CompletedAt.IsZero() {
		t.Fatalf("record A after Close: %+v", ra)
	}
	if rb.DesiredState != store.DesiredPaused || !bytes.Equal(ra.RawInfo, a.mi.RawInfo) || len(ra.Trackers) == 0 {
		t.Fatalf("record B %+v / A raw info or trackers missing", rb)
	}

	// Change one byte of A behind our back, keeping size and mtime: a clean
	// restart trusts the bitfield (RES-02).
	p := filepath.Join(append([]string{dirA}, a.mi.Info.Files[0].Path...)...)
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(p)
	data[10] ^= 0xff
	os.WriteFile(p, data, 0o644)
	os.Chtimes(p, fi.ModTime(), fi.ModTime())

	s2 := startWith(t, Config{Store: ms, PreviousClean: true})
	if len(s2.List()) != 2 {
		t.Fatalf("after restart %d torrents listed, want 2", len(s2.List()))
	}
	sa := waitState(t, s2, a.mi.InfoHash, StateStopped, 15*time.Second)
	if sa.StopReason != StopReasonUser || sa.CompletedBytes != sa.SizeBytes || sa.UploadedTotal != ra.UploadedTotal {
		t.Fatalf("restored A: %+v", sa)
	}
	waitState(t, s2, b.mi.InfoHash, StatePaused, 10*time.Second)
	if st, _ := s2.Get(a.mi.InfoHash); st.CompletedBytes != st.SizeBytes {
		t.Fatal("fast resume rechecked although size and mtime were unchanged")
	}
}

func TestContractSessionFilesMissing(t *testing.T) {
	btest.CheckGoroutines(t)
	ctx := context.Background()
	tr := newTracker(t)
	ms := btest.NewMemStore()
	tt, dir := seedingTorrent(t, tr, "missing")
	s, _ := New(Config{Logger: quiet, Store: ms, PreviousClean: true})
	s.Add(ctx, tt.mi, AddOptions{DownloadDir: dir})
	waitState(t, s, tt.mi.InfoHash, StateSeeding, 15*time.Second)
	s.Close(ctx)
	os.RemoveAll(dir)
	s2 := startWith(t, Config{Store: ms, PreviousClean: true})
	deadline := time.Now().Add(10 * time.Second)
	var st Status
	for st, _ = s2.Get(tt.mi.InfoHash); st.State != StateError && time.Now().Before(deadline); st, _ = s2.Get(tt.mi.InfoHash) {
		time.Sleep(20 * time.Millisecond)
	}
	if st.State != StateError || st.ErrorMessage != ErrorFilesMissing {
		t.Fatalf("all files missing: state %s error %q", st.State, st.ErrorMessage)
	}
	if _, err := os.Stat(dir); err == nil {
		t.Fatal("files or the download dir were recreated for a Files missing torrent")
	}
}

func TestContractSessionBansCorruptPeer(t *testing.T) {
	ln6, err := net.Listen("tcp", "[::1]:0")
	if err != nil {
		t.Skip("no IPv6 loopback")
	}
	ln6.Close()
	btest.CheckGoroutines(t)
	tr := newTracker(t)
	tt := makeTorrent(t, "ban", 32768, tr.url(), tfile{[]string{"z.bin"}, 32768 * 24})
	bad := newSeeder(t, tt)
	bad.mu.Lock()
	bad.corruptAll = true
	bad.mu.Unlock()
	good := newSeederAt(t, "[::1]:0", tt)
	tr.mu.Lock()
	tr.dict = []string{bad.ln.Addr().String(), good.ln.Addr().String()}
	tr.mu.Unlock()
	ms := btest.NewMemStore()
	s := startWith(t, Config{Store: ms})
	s.Add(context.Background(), tt.mi, AddOptions{DownloadDir: t.TempDir()})
	st := waitState(t, s, tt.mi.InfoHash, StateSeeding, 30*time.Second)
	if st.WastedTotal == 0 {
		t.Log("the corrupt peer was never used")
	}
	bad.mu.Lock()
	conns := bad.conns
	bad.mu.Unlock()
	if conns > 2 {
		t.Errorf("reconnected %d times to a peer whose pieces always fail", conns)
	}
	if st.WastedTotal > 0 {
		found := false
		for _, b := range ms.Bans() {
			found = found || b.IP == "127.0.0.1"
			if b.IP == "::1" {
				t.Error("the good peer was banned")
			}
		}
		if !found {
			t.Errorf("no ban stored for the corrupt peer: %+v", ms.Bans())
		}
	}
}

func TestContractSessionStalledPeerDoesNotHang(t *testing.T) {
	btest.CheckGoroutines(t)
	tr := newTracker(t)
	tt := makeTorrent(t, "stall", 32768, tr.url(), tfile{[]string{"s.bin"}, 32768 * 16})
	staller := newSeeder(t, tt)
	staller.mu.Lock()
	staller.stall = true
	staller.mu.Unlock()
	good := newSeeder(t, tt)
	tr.setPeers(staller.port(), good.port())
	s := startWith(t, Config{})
	s.Add(context.Background(), tt.mi, AddOptions{DownloadDir: t.TempDir()})
	waitState(t, s, tt.mi.InfoHash, StateSeeding, 30*time.Second)
	staller.mu.Lock()
	cancels := staller.cancels
	staller.mu.Unlock()
	if cancels == 0 {
		t.Error("no cancel sent to the peer holding blocks that arrived from another peer")
	}
}
