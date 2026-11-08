package session

import (
	"context"
	"net"
	"net/netip"
	"sync/atomic"
	"time"

	"torrentd/internal/bt/choke"
	"torrentd/internal/bt/peer"
	"torrentd/internal/bt/piece"
	"torrentd/internal/bt/storage"
)

// peerState is the actor's view of one connected peer.
type peerState struct {
	key      piece.PeerKey
	addr     netip.AddrPort
	incoming bool
	conn     peer.Conn
	bf       *piece.Bitfield // the peer's pieces

	choked         bool // the peer chokes us
	amInterested   bool // we told the peer we are interested
	amChoking      bool // we choke the peer
	peerInterested bool // the peer is interested in us

	down, up  choke.Meter
	since     time.Time
	lastBlock time.Time

	reply chan bool // block decisions for the read goroutine

	uploads  []piece.Block    // queued requests from the peer
	readCh   chan piece.Block // to the upload helper
	inflight atomic.Int32     // blocks handed to the helper, not yet sent
}

// addPeers queues tracker peers for dialling, skipping duplicates, banned
// IPs, backed-off addresses and our own listen address.
func (t *torrent) addPeers(addrs []netip.AddrPort) {
	if !t.active() {
		return
	}
	queued := map[netip.AddrPort]bool{}
	for _, a := range t.queue {
		queued[a] = true
	}
	now := t.s.clk.Now()
	for _, a := range addrs {
		a = netip.AddrPortFrom(normAddr(a.Addr()), a.Port())
		if !a.IsValid() || a.Port() == 0 || t.addrs[a] || queued[a] || t.isSelf(a) || t.s.isBanned(a.Addr()) {
			continue
		}
		if r, ok := t.backoff[a]; ok && now.Before(r.until) {
			continue
		}
		queued[a] = true
		t.queue = append(t.queue, a)
	}
	t.dialMore()
}

// isSelf reports whether a is our own peer listener.
func (t *torrent) isSelf(a netip.AddrPort) bool {
	if t.s.port == 0 || int(a.Port()) != t.s.port {
		return false
	}
	ip := a.Addr()
	if ip.IsLoopback() || ip.IsUnspecified() {
		return true
	}
	ifaddrs, err := net.InterfaceAddrs()
	if err != nil {
		return false
	}
	for _, ia := range ifaddrs {
		if n, ok := ia.(*net.IPNet); ok {
			if x, ok := netip.AddrFromSlice(n.IP); ok && x.Unmap() == ip {
				return true
			}
		}
	}
	return false
}

// dialMore starts dials while the peer and half-open limits allow.
func (t *torrent) dialMore() {
	for len(t.queue) > 0 && t.active() && t.halfOpen < MaxHalfOpen && len(t.peers)+t.halfOpen < MaxPeersPerTorrent {
		a := t.queue[0]
		t.queue = t.queue[1:]
		if t.addrs[a] || t.s.isBanned(a.Addr()) {
			continue
		}
		t.addrs[a] = true
		t.halfOpen++
		t.goJob(func() { t.dial(a) })
	}
	if len(t.queue) == 0 {
		t.queue = nil
	}
}

// dial connects to a and performs the handshake (dial goroutine).
func (t *torrent) dial(a netip.AddrPort) {
	ctx, cancel := context.WithTimeout(t.ctx, DialTimeout)
	defer cancel()
	var d net.Dialer
	nc, err := d.DialContext(ctx, "tcp", a.String())
	if err != nil {
		t.send(evDialFailed{a})
		return
	}
	hs, err := peer.ExchangeHandshake(ctx, nc, peer.Handshake{InfoHash: t.m.InfoHash, PeerID: t.s.peerID})
	if err != nil {
		_ = nc.Close()
		t.send(evDialFailed{a})
		return
	}
	if !t.send(evHandshaken{addr: a, nc: nc, hs: hs}) {
		_ = nc.Close()
	}
}

// noteEnded starts or doubles the redial backoff of a.
func (t *torrent) noteEnded(a netip.AddrPort) {
	r := t.backoff[a]
	r.n++
	r.until = t.s.clk.Now().Add(RedialBackoff << min(r.n-1, 4))
	t.backoff[a] = r
}

// connected registers a handshaken peer and starts its Conn.
func (t *torrent) connected(ev evHandshaken) {
	reject := !t.active() || ev.hs.PeerID == t.s.peerID || t.s.isBanned(ev.addr.Addr()) ||
		len(t.peers) >= MaxPeersPerTorrent || (ev.incoming && t.addrs[ev.addr])
	if reject {
		_ = ev.nc.Close()
		if !ev.incoming {
			delete(t.addrs, ev.addr)
			t.noteEnded(ev.addr)
		}
		return
	}
	t.addrs[ev.addr] = true
	t.nextKey++
	key := t.nextKey
	now := t.s.clk.Now()
	p := &peerState{
		key: key, addr: ev.addr, incoming: ev.incoming,
		bf:     piece.NewBitfield(t.layout.NumPieces),
		choked: true, amChoking: true,
		down: choke.NewMeter(t.s.clk), up: choke.NewMeter(t.s.clk),
		since: now, reply: make(chan bool, 1),
		readCh: make(chan piece.Block, UploadReadAhead),
	}
	st := t.store
	p.conn = peer.NewConn(ev.nc, ev.hs, peer.Config{
		Clock:          t.s.clk,
		MaxBitfieldLen: (t.layout.NumPieces + 7) / 8,
		OnMessage:      func(m peer.Message) { t.onMessage(p, st, m) },
		OnClose:        func(err error) { t.send(evPeerClosed{key, err}) },
		OnPieceSent: func(_, _ uint32, n int) {
			t.uploaded.Add(int64(n))
			p.up.Add(int64(n))
			p.inflight.Add(-1)
			select {
			case t.events <- evPieceSent{key: key, n: n}:
			default: // the next tick pumps the queue
			}
		},
		Logger: t.s.log,
	})
	t.peers[key] = p
	t.picker.AddPeer(key, ev.addr.Addr())
	conn, ch := p.conn, p.readCh
	t.goJob(func() { uploader(st, conn, ch) })
	if t.haveSet().Count() > 0 {
		_ = p.conn.Send(peer.Message{ID: peer.MsgBitfield, Bitfield: t.haveBytes()})
	}
	t.rechoke(true)
}

// uploader is a peer's upload helper: it reads requested blocks from
// storage and queues them on the Conn. It exits when ch is closed.
func uploader(st storage.Storage, conn peer.Conn, ch <-chan piece.Block) {
	for b := range ch {
		buf := make([]byte, b.Length)
		if err := st.ReadBlock(int(b.Index), int(b.Begin), buf); err != nil {
			_ = conn.Close()
			continue
		}
		_ = conn.Send(peer.Message{ID: peer.MsgPiece, Index: b.Index, Begin: b.Begin, Block: buf})
	}
}

// onMessage runs on the peer's read goroutine. Piece messages wait for the
// actor's decision and are written here (back-pressure, ADR-0017).
func (t *torrent) onMessage(p *peerState, st storage.Storage, m peer.Message) {
	if m.ID != peer.MsgPiece {
		t.send(evMessage{p.key, m})
		return
	}
	if !t.send(evBlock{key: p.key, msg: m, reply: p.reply}) {
		return
	}
	var ok bool
	select {
	case ok = <-p.reply:
	case <-t.ctx.Done():
		return
	}
	if !ok {
		return
	}
	err := st.WriteBlock(int(m.Index), int(m.Begin), m.Block)
	t.send(evWritten{int(m.Index), err})
}

// removePeer closes and forgets one peer at once (its later events are
// ignored).
func (t *torrent) removePeer(key piece.PeerKey) {
	p := t.peers[key]
	if p == nil {
		return
	}
	_ = p.conn.Close()
	close(p.readCh)
	delete(t.peers, key)
	delete(t.addrs, p.addr)
	if !p.incoming {
		t.noteEnded(p.addr)
	}
	if t.picker != nil {
		t.picker.RemovePeer(key)
	}
}

// peerClosed forgets a disconnected peer and hands its blocks to others.
func (t *torrent) peerClosed(key piece.PeerKey) {
	if _, ok := t.peers[key]; !ok {
		return
	}
	t.removePeer(key)
	t.dialMore()
	t.fillAll()
	t.rechoke(true)
}

// dropIP closes every connection from a banned IP.
func (t *torrent) dropIP(ip netip.Addr) {
	ip = normAddr(ip)
	for key, p := range t.peers {
		if p.addr.Addr() == ip {
			t.removePeer(key)
		}
	}
	kept := t.queue[:0]
	for _, a := range t.queue {
		if a.Addr() != ip {
			kept = append(kept, a)
		}
	}
	t.queue = kept
	t.fillAll()
	t.rechoke(true)
}

// message handles one non-piece message from a peer.
func (t *torrent) message(key piece.PeerKey, m peer.Message) {
	p, ok := t.peers[key]
	if !ok {
		return
	}
	switch m.ID {
	case peer.MsgBitfield:
		bf, err := piece.FromBytes(m.Bitfield, t.layout.NumPieces)
		if err != nil {
			t.removePeer(key)
			return
		}
		p.bf = bf
		t.picker.PeerBitfield(key, bf)
		t.availabilityChanged(p)
	case peer.MsgHave:
		if int(m.Index) >= t.layout.NumPieces {
			t.removePeer(key)
			return
		}
		p.bf.Set(int(m.Index))
		t.picker.PeerHave(key, int(m.Index))
		t.availabilityChanged(p)
	case peer.MsgUnchoke:
		p.choked = false
		t.fill(p)
	case peer.MsgChoke:
		p.choked = true
		t.picker.Release(key)
		t.fillAll()
	case peer.MsgInterested:
		p.peerInterested = true
		t.rechoke(true)
	case peer.MsgNotInterested:
		p.peerInterested = false
		t.rechoke(true)
	case peer.MsgRequest:
		t.request(p, m)
	case peer.MsgCancel:
		for i, b := range p.uploads {
			if b.Index == m.Index && b.Begin == m.Begin && b.Length == m.Length {
				p.uploads = append(p.uploads[:i], p.uploads[i+1:]...)
				break
			}
		}
	}
}

// availabilityChanged reacts to a bitfield or have from p.
func (t *torrent) availabilityChanged(p *peerState) {
	if t.state == StateSeeding && t.picker.IsSeed(p.key) {
		t.removePeer(p.key) // seed to seed
		t.rechoke(true)
		return
	}
	t.updateInterest(p)
	t.fill(p)
}

// updateInterest keeps our interested state in line with the picker.
func (t *torrent) updateInterest(p *peerState) {
	want := t.state == StateDownloading && t.picker.Interesting(p.key)
	if want == p.amInterested {
		return
	}
	p.amInterested = want
	id := peer.MsgNotInterested
	if want {
		id = peer.MsgInterested
	}
	_ = p.conn.Send(peer.Message{ID: id})
}

// block decides about a received block (actor side of the data path).
func (t *torrent) block(key piece.PeerKey, m peer.Message) bool {
	n := int64(len(m.Block))
	t.downloaded.Add(n)
	p := t.peers[key]
	if p == nil || t.state != StateDownloading {
		t.wasted.Add(n)
		return false
	}
	p.down.Add(n)
	b := piece.Block{Index: m.Index, Begin: m.Begin, Length: uint32(len(m.Block))}
	rc := t.picker.Received(key, b)
	if !rc.Accepted {
		t.wasted.Add(n)
		t.fill(p)
		return false
	}
	p.lastBlock = t.s.clk.Now()
	idx := int(m.Index)
	t.writes[idx]++
	if rc.PieceDone {
		t.toVerify[idx] = true
	}
	for _, c := range rc.Cancel {
		if cp := t.peers[c]; cp != nil {
			_ = cp.conn.Send(peer.Message{ID: peer.MsgCancel, Index: b.Index, Begin: b.Begin, Length: b.Length})
			t.fill(cp)
		}
	}
	t.fill(p)
	return true
}

// fillAll tops up every unchoked peer's pipeline.
func (t *torrent) fillAll() {
	for _, p := range t.peers {
		t.fill(p)
	}
}

// fill tops up p's request pipeline to piece.PipelineDepth(rate).
func (t *torrent) fill(p *peerState) {
	if p.choked || t.state != StateDownloading || t.picker == nil {
		return
	}
	want := piece.PipelineDepth(p.down.Rate()) - t.picker.Outstanding(p.key)
	if want <= 0 {
		return
	}
	for _, b := range t.picker.Pick(p.key, want) {
		if err := p.conn.Send(peer.Message{ID: peer.MsgRequest, Index: b.Index, Begin: b.Begin, Length: b.Length}); err != nil {
			return
		}
	}
}

// rechoke re-evaluates (or, with fill, tops up) the unchoke set.
func (t *torrent) rechoke(fill bool) {
	if !t.active() || t.choker == nil {
		return
	}
	now := t.s.clk.Now()
	views := make([]choke.Peer, 0, len(t.peers))
	for _, p := range t.peers {
		views = append(views, choke.Peer{
			Key: p.key, Interested: p.peerInterested, AmInterested: p.amInterested,
			DownloadRate: p.down.Rate(), UploadRate: p.up.Rate(),
			ConnectedAt: p.since, LastBlock: p.lastBlock,
		})
	}
	seeding := t.state == StateSeeding
	var keys []piece.PeerKey
	if fill {
		keys = t.choker.Fill(now, views, seeding)
	} else {
		keys = t.choker.Rechoke(now, views, seeding)
	}
	on := map[piece.PeerKey]bool{}
	for _, k := range keys {
		on[k] = true
	}
	for k, p := range t.peers {
		if !on[k] && !p.amChoking {
			p.amChoking = true
			p.uploads = nil
			_ = p.conn.Send(peer.Message{ID: peer.MsgChoke})
		}
	}
	for _, k := range keys {
		if p := t.peers[k]; p != nil && p.amChoking {
			p.amChoking = false
			_ = p.conn.Send(peer.Message{ID: peer.MsgUnchoke})
		}
	}
}

// request queues an upload request from p (validated, bounded).
func (t *torrent) request(p *peerState, m peer.Message) {
	if m.Length > piece.BlockSize {
		t.removePeer(p.key) // WIRE-03
		t.rechoke(true)
		return
	}
	idx := int(m.Index)
	if p.amChoking || !t.active() || m.Length == 0 || !t.haveSet().Has(idx) ||
		int64(m.Begin)+int64(m.Length) > t.layout.PieceSize(idx) || len(p.uploads) >= MaxUploadQueue {
		return
	}
	p.uploads = append(p.uploads, piece.Block{Index: m.Index, Begin: m.Begin, Length: m.Length})
	t.pump(p)
}

// pump hands queued requests to p's upload helper within the read-ahead.
func (t *torrent) pump(p *peerState) {
	for len(p.uploads) > 0 && int(p.inflight.Load()) < UploadReadAhead {
		b := p.uploads[0]
		p.uploads = p.uploads[1:]
		p.inflight.Add(1)
		p.readCh <- b // never blocks: at most UploadReadAhead in flight
	}
	if len(p.uploads) == 0 {
		p.uploads = nil
	}
}

// pieceSent continues uploading after a piece was written to the peer.
func (t *torrent) pieceSent(ev evPieceSent) {
	if p := t.peers[ev.key]; p != nil {
		t.pump(p)
	}
}
