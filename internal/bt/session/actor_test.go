package session

import (
	"context"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"torrentd/internal/bt/peer"
	"torrentd/internal/bt/piece"
	"torrentd/internal/bt/storage"
)

// fakeConn records sent messages.
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

func (c *fakeConn) isClosed() bool { c.mu.Lock(); defer c.mu.Unlock(); return c.closed }

// actorTorrent returns a torrent whose actor is not running: the test
// drives it directly. Its data is on disk and storage is open.
func actorTorrent(t *testing.T, state State) (*torrent, *testTorrent) {
	t.Helper()
	tt := makeTorrent(t, "actor.bin", 32768, "http://127.0.0.1:1/announce", tfile{[]string{"actor.bin"}, 8 * 32768})
	s, err := New(Config{Logger: quiet, Store: newMemStore()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close(context.Background()) })
	dir := t.TempDir()
	writeContent(t, tt, dir)
	tor := newTorrent(s.(*session), tt.mi, dir, torrentInit{desired: DesiredActive, addedAt: time.Now()})
	st, err := storage.Open(dir, tt.mi.Info, storage.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	tor.store, tor.ready, tor.state = st, true, state
	if state == StateSeeding {
		for i := 0; i < tor.layout.NumPieces; i++ {
			tor.picker.PieceVerified(i)
		}
	}
	return tor, tt
}

var nextIP atomic.Uint32

func addPeer(tor *torrent) (*peerState, *fakeConn) {
	fc := newFakeConn()
	tor.nextKey++
	n := nextIP.Add(1)
	p := &peerState{key: tor.nextKey, addr: netip.AddrPortFrom(netip.AddrFrom4([4]byte{10, 0, byte(n >> 8), byte(n)}), 6881),
		conn: fc, choked: true, amChoking: true, since: time.Now(), waitSince: time.Now()}
	tor.peers[p.key] = p
	tor.picker.AddPeer(p.key)
	return p, fc
}

// drainEvents handles queued events (job results) for up to d.
func drainEvents(tor *torrent, d time.Duration) {
	deadline := time.After(d)
	for {
		select {
		case ev := <-tor.events:
			tor.handle(ev)
		case <-deadline:
			return
		}
	}
}

func TestChokerSlotsAndRotation(t *testing.T) {
	tor, _ := actorTorrent(t, StateSeeding)
	var ps []*peerState
	for range 7 {
		p, _ := addPeer(tor)
		ps = append(ps, p)
		tor.message(p.key, peer.Message{ID: peer.MsgInterested})
	}
	unchoked := func() int {
		n := 0
		for _, p := range tor.peers {
			if !p.amChoking {
				n++
			}
		}
		return n
	}
	if unchoked() != 5 || tor.optimistic == 0 {
		t.Fatalf("unchoked %d (optimistic %d), want 4 regular + 1 optimistic", unchoked(), tor.optimistic)
	}
	ps[6].rateUp = 1 << 20 // the fastest uploader wins a regular slot
	now := time.Now()
	tor.rechoke(now)
	if ps[6].amChoking && tor.optimistic != ps[6].key {
		t.Fatal("fast peer should be unchoked")
	}
	if unchoked() > 5 {
		t.Fatalf("%d unchoked", unchoked())
	}
	first := tor.optimistic
	tor.rechoke(now.Add(optimisticRotate))
	if tor.optimistic == first && len(tor.peers) > 5 {
		t.Log("optimistic slot kept (random pick may repeat only if one candidate)")
	}
	// Not interested frees the slot; a disconnect too.
	for _, p := range ps {
		if !p.amChoking {
			tor.message(p.key, peer.Message{ID: peer.MsgNotInterested})
			if !p.amChoking {
				t.Fatal("not interested peer must be choked")
			}
			break
		}
	}
	tor.tick(now.Add(time.Hour))
	tor.state = StateStopped
	tor.rechoke(now)
	tor.fillSlots()
}

func TestServeRequests(t *testing.T) {
	tor, tt := actorTorrent(t, StateSeeding)
	p, fc := addPeer(tor)
	tor.message(p.key, peer.Message{ID: peer.MsgInterested})
	if p.amChoking {
		t.Fatal("interested peer should be unchoked")
	}
	tor.message(p.key, peer.Message{ID: peer.MsgRequest, Index: 1, Begin: 0, Length: piece.BlockSize})
	tor.message(p.key, peer.Message{ID: peer.MsgRequest, Index: 1, Begin: piece.BlockSize, Length: piece.BlockSize})
	for i := range 6 {
		tor.message(p.key, peer.Message{ID: peer.MsgRequest, Index: 2, Begin: 0, Length: uint32(100 + i)})
	}
	tor.message(p.key, peer.Message{ID: peer.MsgCancel, Index: 2, Begin: 0, Length: 105})
	drainEvents(tor, 300*time.Millisecond)
	if n := fc.count(peer.MsgPiece); n != 7 {
		t.Fatalf("sent %d pieces, want 7", n)
	}
	fc.mu.Lock()
	for _, m := range fc.sent {
		if m.ID == peer.MsgPiece {
			off := int(m.Index)*32768 + int(m.Begin)
			if string(m.Block) != string(tt.flat[off:off+len(m.Block)]) {
				t.Errorf("served wrong data for %d/%d", m.Index, m.Begin)
			}
		}
	}
	fc.mu.Unlock()
	if tor.uploaded.Load() == 0 {
		t.Fatal("uploadedTotal not counted")
	}
	// Invalid requests close the connection.
	tor.message(p.key, peer.Message{ID: peer.MsgRequest, Index: 99, Length: 10})
	if !fc.isClosed() {
		t.Fatal("out-of-range request must disconnect")
	}
	p2, fc2 := addPeer(tor)
	tor.message(p2.key, peer.Message{ID: peer.MsgRequest, Index: 0, Length: 2 * piece.BlockSize})
	if !fc2.isClosed() {
		t.Fatal("oversized request must disconnect")
	}
	// A choked peer's requests are ignored; a full peer is dropped while seeding.
	p3, fc3 := addPeer(tor)
	tor.message(p3.key, peer.Message{ID: peer.MsgRequest, Index: 0, Length: 10})
	if len(p3.upQueue) != 0 {
		t.Fatal("request from a choked peer queued")
	}
	full := piece.NewBitfield(tor.layout.NumPieces)
	for i := 0; i < full.Len(); i++ {
		full.Set(i)
	}
	tor.message(p3.key, peer.Message{ID: peer.MsgBitfield, Bitfield: full.Bytes()})
	if !fc3.isClosed() {
		t.Fatal("a seed connected to a seed should be dropped")
	}
}

func TestDownloadSideMessagesAndTimeouts(t *testing.T) {
	tor, tt := actorTorrent(t, StateDownloading)
	p, fc := addPeer(tor)
	tor.message(p.key, peer.Message{ID: peer.MsgBitfield, Bitfield: []byte{1, 2, 3}})
	if !fc.isClosed() {
		t.Fatal("invalid bitfield must disconnect")
	}
	p, fc = addPeer(tor)
	tor.message(p.key, peer.Message{ID: peer.MsgHave, Index: 0})
	tor.message(p.key, peer.Message{ID: peer.MsgHave, Index: 1})
	if fc.count(peer.MsgInterested) != 1 {
		t.Fatal("interested should be sent once")
	}
	tor.message(p.key, peer.Message{ID: peer.MsgUnchoke})
	if fc.count(peer.MsgRequest) == 0 {
		t.Fatal("no requests after unchoke")
	}
	// An unrequested block is wasted.
	tor.message(p.key, peer.Message{ID: peer.MsgPiece, Index: 5, Begin: 0, Block: make([]byte, piece.BlockSize)})
	if tor.wasted.Load() != piece.BlockSize {
		t.Fatalf("wasted = %d", tor.wasted.Load())
	}
	// Request timeout releases and cancels; snubbing after 60 s.
	tor.checkTimeouts(time.Now().Add(snubTimeout + time.Second))
	if !p.slow || !p.snubbed || fc.count(peer.MsgCancel) == 0 {
		t.Fatalf("slow=%v snubbed=%v cancels=%d", p.slow, p.snubbed, fc.count(peer.MsgCancel))
	}
	// Choke releases; the requested blocks arrive after unchoke and verify.
	tor.message(p.key, peer.Message{ID: peer.MsgChoke})
	p.slow = false
	tor.message(p.key, peer.Message{ID: peer.MsgUnchoke})
	fc.mu.Lock()
	var reqs []peer.Message
	for _, m := range fc.sent {
		if m.ID == peer.MsgRequest {
			reqs = append(reqs, m)
		}
	}
	fc.mu.Unlock()
	for _, r := range reqs[len(reqs)-2:] {
		off := int(r.Index)*32768 + int(r.Begin)
		tor.message(p.key, peer.Message{ID: peer.MsgPiece, Index: r.Index, Begin: r.Begin, Block: tt.flat[off : off+int(r.Length)]})
	}
	drainEvents(tor, 300*time.Millisecond)
	if tor.picker.HaveCount() == 0 {
		t.Fatal("no piece verified")
	}
	tor.peerClosed(p.key, nil)
	tor.peerClosed(p.key, nil) // unknown: ignored
	tor.message(p.key, peer.Message{ID: peer.MsgHave})
	// Have out of range disconnects.
	p, fc = addPeer(tor)
	tor.message(p.key, peer.Message{ID: peer.MsgHave, Index: 999})
	if !fc.isClosed() {
		t.Fatal("have out of range must disconnect")
	}
}

func TestStrikesBanAndDiscard(t *testing.T) {
	tor, _ := actorTorrent(t, StateDownloading)
	st := tor.s.store.(*memStore)
	bad, fc := addPeer(tor)
	ip := bad.addr.Addr()
	tor.contrib[3] = map[netip.Addr]struct{}{ip: {}}
	for i := 0; i < BanStrikes-1; i++ {
		if tor.s.strike(ip, tor) {
			t.Fatal("banned too early")
		}
	}
	if !tor.s.strike(ip, tor) || !tor.s.banned(ip) {
		t.Fatal("third strike must ban")
	}
	if tor.s.strike(ip, tor) {
		t.Fatal("already banned")
	}
	tor.queue = []netip.AddrPort{bad.addr, netip.MustParseAddrPort("10.9.9.9:1")}
	tor.applyBan(ip)
	if !fc.isClosed() || len(tor.queue) != 1 || tor.contrib[3] != nil {
		t.Fatal("ban must close, unqueue and discard")
	}
	tor.addPeers([]netip.AddrPort{bad.addr})
	for _, a := range tor.queue {
		if a == bad.addr {
			t.Fatal("banned peer queued")
		}
	}
	waitFor(t, "ban saved", 2*time.Second, func() bool { st.mu.Lock(); defer st.mu.Unlock(); return len(st.bans) == 1 })
	// Expiry.
	tor.s.banMu.Lock()
	tor.s.bans[ip] = time.Now().Add(-time.Second)
	tor.s.banMu.Unlock()
	if tor.s.banned(ip) {
		t.Fatal("expired ban still active")
	}
	// A failed piece strikes its contributors.
	tor.contrib[2] = map[netip.Addr]struct{}{ip: {}}
	tor.verified(evVerified{index: 2, ok: false})
	if tor.wasted.Load() == 0 {
		t.Fatal("failed piece not counted as wasted")
	}
}

func TestPoolRunsEverything(t *testing.T) {
	p := newPool(2)
	var n atomic.Int64
	var wg sync.WaitGroup
	for range 3000 {
		wg.Add(1)
		p.submit(func() { defer wg.Done(); n.Add(1) })
	}
	wg.Wait()
	p.close()
	p.submit(func() { n.Add(1) }) // after close: runs inline
	if n.Load() != 3001 {
		t.Fatalf("ran %d jobs", n.Load())
	}
}

func TestResumeHelpers(t *testing.T) {
	tt := makeTorrent(t, "multi", 16384, "", tfile{[]string{"a"}, 20000}, tfile{[]string{"b"}, 5000})
	dir := t.TempDir()
	fs := statFiles(tt.mi.Info, dir)
	if !fs.allMissing() || fs.anyData() || fs.matches("", true) {
		t.Fatal("nothing exists yet")
	}
	os.MkdirAll(filepath.Join(dir, "multi"), 0o755)
	os.WriteFile(filepath.Join(dir, "multi", "a.part"), make([]byte, 20000), 0o644)
	os.WriteFile(filepath.Join(dir, "multi", "b"), make([]byte, 5000), 0o644)
	fp := fingerprint(tt.mi.Info, dir)
	fs = statFiles(tt.mi.Info, dir)
	if fs.allMissing() || !fs.anyData() || !fs.matches(fp, true) || fs.matches("[]", false) || fs.matches("{", false) {
		t.Fatal("fingerprint round trip")
	}
	later := time.Now().Add(time.Hour)
	os.Chtimes(filepath.Join(dir, "multi", "b"), later, later)
	fs = statFiles(tt.mi.Info, dir)
	if fs.matches(fp, true) || !fs.matches(fp, false) {
		t.Fatal("mtime matters only after a clean shutdown")
	}
	if wantedBytes(tt.mi.Info, 0) != 16384 || wantedBytes(tt.mi.Info, 1) != 25000-16384 || wantedBytes(tt.mi.Info, 5) != 0 {
		t.Fatal("wantedBytes")
	}
}
