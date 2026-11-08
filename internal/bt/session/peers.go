package session

import (
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"time"

	"torrentd/internal/bt/peer"
	"torrentd/internal/bt/piece"
)

// Request timing (§4.1 anti-snubbing).
const (
	requestTimeout = 30 * time.Second
	snubTimeout    = 60 * time.Second
	maxUpQueue     = 512 // queued requests from one peer
	maxUpReads     = 4   // block reads in flight for one peer
)

// peerState is the actor's view of one connected peer.
type peerState struct {
	key      piece.PeerKey
	addr     netip.AddrPort
	conn     peer.Conn
	have     *piece.Bitfield
	incoming bool

	choked     bool // the peer chokes us
	interested bool // we told the peer we are interested
	amChoking  bool // we choke the peer
	peerWants  bool // the peer is interested in us

	since     time.Time
	waitSince time.Time // when we last started waiting for a block
	slow      bool      // requests timed out; depth 1 until it delivers
	snubbed   bool      // no data for snubTimeout while we waited

	received         int64 // payload bytes received
	recvWin, sentWin int64 // bytes in the current tick
	rateDown, rateUp int64 // smoothed bytes/s

	upQueue []piece.Block
	upReads int
}

// wantsPeers reports whether new peers are useful.
func (t *torrent) wantsPeers() bool { return t.active() }

// addPeers queues tracker peers for dialling, skipping duplicates, banned
// IPs and our own listen address.
func (t *torrent) addPeers(addrs []netip.AddrPort) {
	if !t.wantsPeers() {
		return
	}
	queued := map[netip.AddrPort]bool{}
	for _, a := range t.queue {
		queued[a] = true
	}
	for _, a := range addrs {
		a = netip.AddrPortFrom(a.Addr().Unmap(), a.Port())
		if !a.IsValid() || a.Port() == 0 || t.addrs[a] || queued[a] || t.isSelf(a) || t.s.banned(a.Addr()) {
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

// dialMore starts dials while the peer and half-open limits allow. Seeding
// torrents do not dial; leechers come to them.
func (t *torrent) dialMore() {
	for len(t.queue) > 0 && t.state == StateDownloading && t.halfOpen < maxHalfOpen && len(t.peers)+t.halfOpen < maxPeers {
		a := t.queue[0]
		t.queue = t.queue[1:]
		if t.addrs[a] || t.s.banned(a.Addr()) {
			continue
		}
		t.addrs[a] = true
		t.halfOpen++
		t.goRoutine(func() { t.dial(a) })
	}
	if len(t.queue) == 0 {
		t.queue = nil
	}
}

// dial connects to a and performs the handshake (dial goroutine).
func (t *torrent) dial(a netip.AddrPort) {
	ctx, cancel := context.WithTimeout(t.ctx, dialTimeout)
	defer cancel()
	var d net.Dialer
	nc, err := d.DialContext(ctx, "tcp", a.String())
	if err != nil {
		t.s.log.Debug("dial failed", "peer", a, "err", err)
		t.send(evDialFailed{a})
		return
	}
	hs, err := peer.ExchangeHandshake(ctx, nc, peer.Handshake{InfoHash: t.m.InfoHash, PeerID: t.s.peerID})
	if err != nil {
		_ = nc.Close()
		t.s.log.Debug("handshake failed", "peer", a, "err", err)
		t.send(evDialFailed{a})
		return
	}
	if !t.send(evHandshaken{a, nc, hs}) {
		_ = nc.Close()
	}
}

// connected registers a handshaken peer (dialled or incoming) and starts
// its Conn.
func (t *torrent) connected(addr netip.AddrPort, nc net.Conn, hs peer.Handshake, incoming bool) {
	if !t.wantsPeers() || hs.PeerID == t.s.peerID || len(t.peers) >= maxPeers || t.s.banned(addr.Addr()) {
		_ = nc.Close()
		if !incoming {
			delete(t.addrs, addr)
		}
		return
	}
	t.nextKey++
	key := t.nextKey
	now := t.s.clk.Now()
	p := &peerState{key: key, addr: addr, incoming: incoming, choked: true, amChoking: true, since: now, waitSince: now}
	p.conn = peer.NewConn(nc, hs, peer.Config{
		Clock:          t.s.clk,
		MaxBitfieldLen: (t.layout.NumPieces + 7) / 8,
		OnMessage:      func(m peer.Message) { t.send(evMessage{key, m}) },
		OnClose:        func(err error) { t.send(evPeerClosed{key, err}) },
		Logger:         t.s.log,
	})
	t.peers[key] = p
	if !incoming {
		t.addrs[addr] = true
	}
	t.picker.AddPeer(key)
	if t.picker.HaveCount() > 0 {
		_ = p.conn.Send(peer.Message{ID: peer.MsgBitfield, Bitfield: t.picker.Have().Bytes()})
	}
	t.s.log.Debug("peer connected", "torrent", t.m.HexHash(), "peer", addr, "incoming", incoming)
}

// peerClosed forgets a disconnected peer and hands its blocks to the others.
func (t *torrent) peerClosed(key piece.PeerKey, err error) {
	delete(t.closing, key)
	p, ok := t.peers[key]
	if !ok {
		return
	}
	if err != nil && !errors.Is(err, io.EOF) {
		t.s.log.Debug("peer disconnected", "peer", p.addr, "err", err)
	}
	delete(t.peers, key)
	delete(t.addrs, p.addr)
	t.picker.RemovePeer(key)
	wasUnchoked := !p.amChoking
	if t.optimistic == key {
		t.optimistic = 0
	}
	t.dialMore()
	t.fillAll()
	if wasUnchoked {
		t.fillSlots()
	}
}

// message handles one message from a peer.
func (t *torrent) message(key piece.PeerKey, m peer.Message) {
	p, ok := t.peers[key]
	if !ok {
		return
	}
	switch m.ID {
	case peer.MsgBitfield:
		bf, err := piece.FromBytes(m.Bitfield, t.layout.NumPieces)
		if err != nil {
			t.s.log.Debug("invalid bitfield", "peer", p.addr, "err", err)
			t.dropPeer(key, p)
			return
		}
		p.have = bf
		t.picker.PeerBitfield(key, bf)
		if t.state == StateSeeding && bf.Full() {
			t.dropPeer(key, p)
			return
		}
		t.updateInterest(p)
		t.fill(p)
	case peer.MsgHave:
		if int(m.Index) >= t.layout.NumPieces {
			t.dropPeer(key, p)
			return
		}
		if p.have == nil {
			p.have = piece.NewBitfield(t.layout.NumPieces)
		}
		p.have.Set(int(m.Index))
		t.picker.PeerHave(key, int(m.Index))
		t.updateInterest(p)
		t.fill(p)
	case peer.MsgUnchoke:
		if p.choked {
			p.choked = false
			p.waitSince = t.s.clk.Now()
		}
		t.fill(p)
	case peer.MsgChoke:
		p.choked = true
		t.picker.Release(key)
		t.fillAll()
	case peer.MsgPiece:
		t.block(p, m)
	case peer.MsgInterested:
		p.peerWants = true
		t.fillSlots()
	case peer.MsgNotInterested:
		p.peerWants = false
		if !p.amChoking {
			t.choke(p)
			t.fillSlots()
		}
	case peer.MsgRequest:
		t.request(p, m)
	case peer.MsgCancel:
		t.cancelRequest(p, piece.Block{Index: m.Index, Begin: m.Begin, Length: m.Length})
	}
}

// updateInterest sends interested once the peer has something we need.
func (t *torrent) updateInterest(p *peerState) {
	if p.interested || t.state != StateDownloading || p.have == nil {
		return
	}
	have := t.picker.Have()
	for i := 0; i < t.layout.NumPieces; i++ {
		if p.have.Has(i) && !have.Has(i) {
			p.interested = true
			_ = p.conn.Send(peer.Message{ID: peer.MsgInterested})
			return
		}
	}
}

// block handles a piece message.
func (t *torrent) block(p *peerState, m peer.Message) {
	n := int64(len(m.Block))
	t.downloaded.Add(n)
	p.received += n
	p.recvWin += n
	if t.state != StateDownloading {
		t.wasted.Add(n)
		return
	}
	b := piece.Block{Index: m.Index, Begin: m.Begin, Length: uint32(len(m.Block))}
	accepted, pieceDone, cancel := t.picker.Received(p.key, b)
	if !accepted {
		t.wasted.Add(n)
		return
	}
	p.waitSince = t.s.clk.Now()
	p.slow, p.snubbed = false, false
	idx, begin, data, st := int(m.Index), int(m.Begin), m.Block, t.store
	ips := t.contrib[idx]
	if ips == nil {
		ips = map[netip.Addr]struct{}{}
		t.contrib[idx] = ips
	}
	ips[p.addr.Addr()] = struct{}{}
	t.writes[idx]++
	t.pending++
	if pieceDone {
		t.toVerify[idx] = true
	}
	t.goJob(t.s.disk, func() {
		if t.ctx.Err() != nil {
			t.send(evWritten{idx, context.Canceled})
			return
		}
		t.send(evWritten{idx, st.WriteBlock(idx, begin, data)})
	})
	t.fill(p)
	for _, o := range cancel {
		if q := t.peers[o]; q != nil {
			_ = q.conn.Send(peer.Message{ID: peer.MsgCancel, Index: b.Index, Begin: b.Begin, Length: b.Length})
			t.fill(q)
		}
	}
}

// pipelineRate is the rate that sizes p's pipeline: the smoothed rate, or
// the average since connecting (at least over 1 s) while that is higher,
// so a fast new peer ramps up before the first rate sample.
func (t *torrent) pipelineRate(p *peerState) int64 {
	el := t.s.clk.Now().Sub(p.since)
	if el < time.Second {
		el = time.Second
	}
	return max(p.rateDown, int64(float64(p.received)/el.Seconds()))
}

// fillAll tops up every unchoked peer's pipeline.
func (t *torrent) fillAll() {
	for _, p := range t.peers {
		t.fill(p)
	}
}

// fill tops up p's request pipeline to piece.PipelineDepth(rate). It only
// picks when a useful batch is free, so Pick is not called for every block.
func (t *torrent) fill(p *peerState) {
	if p.choked || !p.interested || t.state != StateDownloading {
		return
	}
	if t.pending >= maxPendingWrites {
		t.throttled = true
		return
	}
	depth := piece.PipelineDepth(t.pipelineRate(p))
	if p.slow {
		depth = 1
	}
	out := t.picker.Outstanding(p.key)
	want := depth - out
	if want <= 0 || (out > 0 && want < max(1, depth/8)) {
		return
	}
	bs := t.picker.Pick(p.key, want)
	if out == 0 && len(bs) > 0 {
		p.waitSince = t.s.clk.Now()
	}
	for _, b := range bs {
		if err := p.conn.Send(peer.Message{ID: peer.MsgRequest, Index: b.Index, Begin: b.Begin, Length: b.Length}); err != nil {
			return
		}
	}
}

// checkTimeouts releases the requests of peers that left them unanswered
// for requestTimeout and marks peers snubbed after snubTimeout.
func (t *torrent) checkTimeouts(now time.Time) {
	if t.state != StateDownloading {
		return
	}
	released := false
	for _, p := range t.peers {
		if p.choked || t.picker.Outstanding(p.key) == 0 {
			continue
		}
		waited := now.Sub(p.waitSince)
		if waited >= snubTimeout {
			p.snubbed = true
		}
		if waited >= requestTimeout && !p.slow {
			for _, b := range t.picker.Release(p.key) {
				_ = p.conn.Send(peer.Message{ID: peer.MsgCancel, Index: b.Index, Begin: b.Begin, Length: b.Length})
			}
			p.slow = true
			p.waitSince = now
			released = true
		}
	}
	if released {
		t.fillAll()
	}
}
