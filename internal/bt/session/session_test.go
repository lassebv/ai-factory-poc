package session

import (
	"context"
	"crypto/sha1"
	"errors"
	"math/rand/v2"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"torrentd/internal/bt/bencode"
	"torrentd/internal/bt/metainfo"
	"torrentd/internal/bt/peer"
	"torrentd/internal/bt/piece"
	"torrentd/internal/bt/storage"
	"torrentd/internal/bt/tracker"
	"torrentd/internal/clock"
)

// fakeConn is a peer.Conn that records what the actor sends.
type fakeConn struct {
	mu     sync.Mutex
	sent   []peer.Message
	closed bool
	done   chan struct{}
}

func newFakeConn() *fakeConn { return &fakeConn{done: make(chan struct{})} }

func (c *fakeConn) Send(m peer.Message) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return peer.ErrClosed
	}
	c.sent = append(c.sent, m)
	return nil
}

func (c *fakeConn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.closed {
		c.closed = true
		close(c.done)
	}
	return nil
}

func (c *fakeConn) Done() <-chan struct{}  { return c.done }
func (c *fakeConn) Err() error             { return nil }
func (c *fakeConn) Remote() peer.Handshake { return peer.Handshake{} }
func (c *fakeConn) RemoteAddr() net.Addr   { return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1} }
func (c *fakeConn) isClosed() bool         { c.mu.Lock(); defer c.mu.Unlock(); return c.closed }
func (c *fakeConn) count(id peer.MessageID) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, m := range c.sent {
		if m.ID == id {
			n++
		}
	}
	return n
}

func (c *fakeConn) requests() []peer.Message {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []peer.Message
	for _, m := range c.sent {
		if m.ID == peer.MsgRequest {
			out = append(out, m)
		}
	}
	c.sent = nil
	return out
}

// unitTorrent builds a 2-piece single-file torrent of 40000 bytes.
func unitTorrent(t *testing.T) (*metainfo.MetaInfo, []byte) {
	t.Helper()
	data := make([]byte, 40000)
	for i := range data {
		data[i] = byte(i * 7)
	}
	h1, h2 := sha1.Sum(data[:32768]), sha1.Sum(data[32768:])
	info, _ := bencode.Marshal(map[string]any{"name": "u.bin", "length": len(data), "piece length": 32768,
		"pieces": append(h1[:], h2[:]...)})
	raw, err := bencode.Marshal(map[string]any{"announce": "http://127.0.0.1:1/announce", "info": bencode.RawMessage(info)})
	if err != nil {
		t.Fatal(err)
	}
	m, err := metainfo.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return m, data
}

// unitSession is a session without a listener, on a fake clock.
func unitSession() *session {
	return &session{
		clk:      clock.NewFake(time.Date(2026, 11, 7, 0, 0, 0, 0, time.UTC)),
		log:      quiet,
		rnd:      rand.New(rand.NewPCG(3, 4)),
		torrents: map[[20]byte]*torrent{},
		port:     6881,
		tracker:  tracker.NewHTTPClient(nil),
	}
}

// openTorrent returns a torrent in state downloading with real storage.
func openTorrent(t *testing.T) (*torrent, []byte) {
	t.Helper()
	m, data := unitTorrent(t)
	s := unitSession()
	dir := t.TempDir()
	tor := newTorrent(s, m, dir, false)
	st, err := storage.Open(dir, m.Info, storage.Options{})
	if err != nil {
		t.Fatal(err)
	}
	tor.handle(evOpened{st: st})
	if tor.state != StateDownloading {
		t.Fatalf("state %s after open", tor.state)
	}
	t.Cleanup(func() { tor.shutdown(context.Background()) })
	return tor, data
}

func addFakePeer(tor *torrent, port uint16) (*fakeConn, piece.PeerKey) {
	c := newFakeConn()
	tor.nextKey++
	k := tor.nextKey
	tor.peers[k] = &peerState{key: k, addr: netip.AddrPortFrom(netip.MustParseAddr("10.0.0.1"), port),
		conn: c, choked: true, since: tor.s.clk.Now()}
	tor.picker.AddPeer(k, netip.Addr{})
	return c, k
}

// drain handles queued actor events until none arrive for a while.
func drain(t *testing.T, tor *torrent) {
	t.Helper()
	for {
		select {
		case ev := <-tor.events:
			tor.handle(ev)
		case <-time.After(200 * time.Millisecond):
			return
		}
	}
}

func serveBlocks(tor *torrent, k piece.PeerKey, reqs []peer.Message, data []byte) {
	for _, r := range reqs {
		off := int(r.Index)*32768 + int(r.Begin)
		tor.handle(evMessage{k, peer.Message{ID: peer.MsgPiece, Index: r.Index, Begin: r.Begin,
			Block: append([]byte(nil), data[off:off+int(r.Length)]...)}})
	}
}

func TestActorDownloadsAndHandlesMessages(t *testing.T) {
	tor, data := openTorrent(t)
	c, k := addFakePeer(tor, 1000)

	tor.handle(evMessage{k, peer.Message{ID: peer.MsgHave, Index: 1}})
	if c.count(peer.MsgInterested) != 1 {
		t.Fatal("have of a needed piece must send interested")
	}
	tor.handle(evMessage{k, peer.Message{ID: peer.MsgBitfield, Bitfield: []byte{0xc0}}})
	tor.handle(evMessage{k, peer.Message{ID: peer.MsgUnchoke}})
	reqs := c.requests()
	if len(reqs) != 3 { // every block: fewer than the minimum depth
		t.Fatalf("%d requests, want all 3 blocks", len(reqs))
	}
	// A choke releases the outstanding blocks.
	tor.handle(evMessage{k, peer.Message{ID: peer.MsgChoke}})
	if n := tor.picker.Outstanding(k); n != 0 {
		t.Fatalf("outstanding after choke = %d", n)
	}
	tor.handle(evMessage{k, peer.Message{ID: peer.MsgUnchoke}})
	for i := 0; i < 10 && tor.state == StateDownloading; i++ {
		serveBlocks(tor, k, c.requests(), data)
		drain(t, tor)
	}
	if tor.state != StateSeeding {
		t.Fatalf("state %s, want seeding", tor.state)
	}
	tor.publish()
	st := tor.status.Load()
	if st.CompletedBytes != int64(len(data)) || st.Progress != 1 || st.CompletedAt.IsZero() || st.PeersConnected != 1 || st.SeedsConnected != 1 {
		t.Fatalf("status %+v", st)
	}
	got, err := os.ReadFile(filepath.Join(tor.dir, "u.bin"))
	if err != nil || string(got) != string(data) {
		t.Fatalf("file content wrong (%v)", err)
	}
	if c.count(peer.MsgNotInterested) != 1 || c.count(peer.MsgHave) != 2 {
		t.Errorf("not-interested %d, have %d", c.count(peer.MsgNotInterested), c.count(peer.MsgHave))
	}
	// A block after completion is wasted.
	w := tor.wasted.Load()
	tor.handle(evMessage{k, peer.Message{ID: peer.MsgPiece, Index: 0, Begin: 0, Block: make([]byte, 100)}})
	if tor.wasted.Load() != w+100 {
		t.Error("a block received while seeding must count as wasted")
	}
	tor.handle(evPeerClosed{k, errors.New("boom")})
	if len(tor.peers) != 0 {
		t.Error("closed peer still registered")
	}
	tor.handle(evPeerClosed{k, nil}) // unknown peer: ignored
	tor.handle(evMessage{k, peer.Message{ID: peer.MsgUnchoke}})
}

func TestActorHashFailureAndRejectedBlocks(t *testing.T) {
	tor, data := openTorrent(t)
	c, k := addFakePeer(tor, 1001)
	tor.handle(evMessage{k, peer.Message{ID: peer.MsgBitfield, Bitfield: []byte{0xc0}}})
	tor.handle(evMessage{k, peer.Message{ID: peer.MsgUnchoke}})
	bad := append([]byte(nil), data...)
	bad[5] ^= 0xff
	serveBlocks(tor, k, c.requests(), bad)
	// A misaligned block is rejected and wasted.
	tor.handle(evMessage{k, peer.Message{ID: peer.MsgPiece, Index: 0, Begin: 3, Block: make([]byte, 10)}})
	drain(t, tor)
	if tor.wasted.Load() < 32768+10 {
		t.Fatalf("wasted = %d, want >= a piece + 10", tor.wasted.Load())
	}
	for i := 0; i < 10 && tor.state == StateDownloading; i++ {
		serveBlocks(tor, k, c.requests(), data)
		drain(t, tor)
	}
	if tor.state != StateSeeding {
		t.Fatalf("state %s after recovering from a hash failure", tor.state)
	}
}

func TestActorProtocolViolations(t *testing.T) {
	tor, _ := openTorrent(t)
	c1, k1 := addFakePeer(tor, 1)
	tor.handle(evMessage{k1, peer.Message{ID: peer.MsgBitfield, Bitfield: []byte{0xff}}}) // spare bits set
	if !c1.isClosed() {
		t.Error("an invalid bitfield must disconnect")
	}
	c2, k2 := addFakePeer(tor, 2)
	tor.handle(evMessage{k2, peer.Message{ID: peer.MsgHave, Index: 99}})
	if !c2.isClosed() {
		t.Error("an out-of-range have must disconnect")
	}
}

func TestActorErrors(t *testing.T) {
	tor, _ := openTorrent(t)
	c, _ := addFakePeer(tor, 3)
	tor.handle(evVerified{index: 0, err: errors.New("disk on fire")})
	if tor.state != StateError || !c.isClosed() {
		t.Fatalf("state %s closed %v", tor.state, c.isClosed())
	}
	tor.publish()
	if st := tor.status.Load(); st.ErrorMessage == "" {
		t.Error("error message missing")
	}
	tor.handle(evVerified{index: 0, ok: true}) // ignored in state error
	tor.handle(evWritten{index: 0, err: errors.New("again")})

	tor2, _ := openTorrent(t)
	tor2.writes[1] = 1
	tor2.handle(evWritten{index: 1, err: errors.New("short write")})
	if tor2.state != StateError {
		t.Fatalf("write error: state %s", tor2.state)
	}

	m, _ := unitTorrent(t)
	tor3 := newTorrent(unitSession(), m, t.TempDir(), false)
	tor3.handle(evOpened{err: errors.New("no space")})
	if tor3.state != StateError {
		t.Fatalf("open error: state %s", tor3.state)
	}
	tor3.shutdown(context.Background())
}

func TestDialing(t *testing.T) {
	tor, _ := openTorrent(t)
	// A closed port: the dial fails.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dead := netip.MustParseAddrPort(ln.Addr().String())
	ln.Close()
	// A listener that hangs up without a handshake.
	ln2, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln2.Close()
	go func() {
		for {
			c, err := ln2.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	rude := netip.MustParseAddrPort(ln2.Addr().String())
	self := netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), uint16(tor.s.port))
	tor.handle(evPeers{[]netip.AddrPort{dead, rude, dead, self, {}}})
	if tor.halfOpen != 2 {
		t.Fatalf("halfOpen = %d, want 2 (duplicates, self and invalid skipped)", tor.halfOpen)
	}
	drain(t, tor)
	if tor.halfOpen != 0 || len(tor.addrs) != 0 {
		t.Fatalf("after failed dials: halfOpen %d addrs %d", tor.halfOpen, len(tor.addrs))
	}
	// A handshaken connection to ourselves (same peer id) is dropped.
	a, b := net.Pipe()
	defer b.Close()
	tor.halfOpen++
	tor.handle(evHandshaken{addr: rude, nc: a, hs: peer.Handshake{PeerID: tor.s.peerID}})
	if len(tor.peers) != 0 {
		t.Error("self connection registered")
	}
	// A real handshaken connection becomes a peer.
	a2, b2 := net.Pipe()
	defer b2.Close()
	tor.halfOpen++
	tor.handle(evHandshaken{addr: rude, nc: a2, hs: peer.Handshake{PeerID: [20]byte{9}}})
	if len(tor.peers) != 1 {
		t.Fatal("peer not registered")
	}
	// Not wanting peers (seeding) ignores tracker peers.
	tor.state = StateSeeding
	tor.handle(evPeers{[]netip.AddrPort{dead}})
	if len(tor.queue) != 0 || tor.halfOpen != 0 {
		t.Error("peers queued while seeding")
	}
}

func TestIsSelf(t *testing.T) {
	tor, _ := openTorrent(t)
	p := uint16(tor.s.port)
	if !tor.isSelf(netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), p)) {
		t.Error("loopback with our port is self")
	}
	if tor.isSelf(netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), p+1)) {
		t.Error("another port is not self")
	}
	if tor.isSelf(netip.AddrPortFrom(netip.MustParseAddr("192.0.2.1"), p)) {
		t.Error("a TEST-NET address is not self")
	}
	tor.s.port = 0
	if tor.isSelf(netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), 0)) {
		t.Error("unbound session has no self")
	}
}

func TestResolveDirAndDefaults(t *testing.T) {
	f := filepath.Join(t.TempDir(), "file")
	os.WriteFile(f, nil, 0o644)
	if _, err := resolveDir(f); !errors.Is(err, ErrInvalidDownloadDir) {
		t.Errorf("a file as dir: %v", err)
	}
	if _, err := resolveDir(filepath.Join(f, "sub")); !errors.Is(err, ErrInvalidDownloadDir) {
		t.Errorf("a dir under a file: %v", err)
	}
	if _, err := resolveDir(""); !errors.Is(err, ErrInvalidDownloadDir) {
		t.Errorf("empty dir: %v", err)
	}
	if _, err := New(Config{PeerPort: 70000}); err == nil {
		t.Error("an invalid peer port must fail")
	}
	dir := t.TempDir()
	s, err := New(Config{DefaultDownloadDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	m, _ := unitTorrent(t)
	st, err := s.Add(context.Background(), m, AddOptions{StartPaused: true})
	if err != nil || st.DownloadDir != dir {
		t.Fatalf("Add with the default dir: %+v %v", st, err)
	}
	if _, err := s.Add(context.Background(), nil, AddOptions{}); err == nil {
		t.Error("Add(nil) must fail")
	}
	if err := s.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Add(context.Background(), &metainfo.MetaInfo{InfoHash: [20]byte{1}, Info: m.Info}, AddOptions{}); !errors.Is(err, ErrClosed) {
		t.Errorf("Add after Close: %v", err)
	}
}

func TestCloseHonorsContext(t *testing.T) {
	s, err := New(Config{Logger: quiet})
	if err != nil {
		t.Fatal(err)
	}
	ss := s.(*session)
	ss.wg.Add(1) // a goroutine that never finishes
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.Close(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("Close with a cancelled ctx = %v", err)
	}
	ss.wg.Done()
}

func TestRateAndEmptyStatus(t *testing.T) {
	now := time.Unix(100, 0)
	p := &peerState{since: now.Add(-4 * time.Second), received: 4000}
	if r := p.rate(now); r != 1000 {
		t.Errorf("rate = %d", r)
	}
	if r := p.rate(now.Add(-4 * time.Second)); r != 4000 {
		t.Errorf("rate within the first second = %d", r)
	}
}
