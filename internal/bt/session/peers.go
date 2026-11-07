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

// peerState is the actor's view of one connected peer.
type peerState struct {
	key        piece.PeerKey
	addr       netip.AddrPort
	conn       peer.Conn
	have       *piece.Bitfield
	choked     bool // the peer chokes us
	interested bool // we told the peer we are interested
	since      time.Time
	received   int64 // payload bytes received
}

// rate is the peer's average payload rate since it connected, in bytes/s.
func (p *peerState) rate(now time.Time) int64 {
	el := now.Sub(p.since)
	if el < time.Second {
		el = time.Second
	}
	return int64(float64(p.received) / el.Seconds())
}

// wantsPeers reports whether new peers are useful.
func (t *torrent) wantsPeers() bool { return t.state == StateDownloading }

// addPeers queues tracker peers for dialling, skipping duplicates and our
// own listen address.
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
		if !a.IsValid() || a.Port() == 0 || t.addrs[a] || queued[a] || t.isSelf(a) {
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
	for len(t.queue) > 0 && t.wantsPeers() && t.halfOpen < maxHalfOpen && len(t.peers)+t.halfOpen < maxPeers {
		a := t.queue[0]
		t.queue = t.queue[1:]
		if t.addrs[a] {
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

// connected registers a handshaken peer and starts its Conn.
func (t *torrent) connected(ev evHandshaken) {
	if !t.wantsPeers() || ev.hs.PeerID == t.s.peerID {
		_ = ev.nc.Close()
		delete(t.addrs, ev.addr)
		return
	}
	t.nextKey++
	key := t.nextKey
	p := &peerState{key: key, addr: ev.addr, choked: true, since: t.s.clk.Now()}
	p.conn = peer.NewConn(ev.nc, ev.hs, peer.Config{
		Clock:          t.s.clk,
		MaxBitfieldLen: (t.layout.NumPieces + 7) / 8,
		OnMessage:      func(m peer.Message) { t.send(evMessage{key, m}) },
		OnClose:        func(err error) { t.send(evPeerClosed{key, err}) },
		Logger:         t.s.log,
	})
	t.peers[key] = p
	t.picker.AddPeer(key)
	t.s.log.Debug("peer connected", "torrent", t.m.HexHash(), "peer", ev.addr)
}

// peerClosed forgets a disconnected peer and hands its blocks to the others.
func (t *torrent) peerClosed(key piece.PeerKey, err error) {
	p, ok := t.peers[key]
	if !ok {
		return
	}
	if err != nil && !errors.Is(err, io.EOF) {
		t.s.log.Debug("peer disconnected", "peer", p.addr, "err", err)
	}
	delete(t.peers, key)
	delete(t.addrs, p.addr)
	if t.picker != nil {
		t.picker.RemovePeer(key)
	}
	t.dialMore()
	t.fillAll()
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
			_ = p.conn.Close()
			return
		}
		p.have = bf
		t.picker.PeerBitfield(key, bf)
		t.updateInterest(p)
		t.fill(p)
	case peer.MsgHave:
		if int(m.Index) >= t.layout.NumPieces {
			_ = p.conn.Close()
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
		p.choked = false
		t.fill(p)
	case peer.MsgChoke:
		p.choked = true
		t.picker.Release(key)
		t.fillAll()
	case peer.MsgPiece:
		t.block(p, m)
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
	if t.state != StateDownloading {
		t.wasted.Add(n)
		return
	}
	b := piece.Block{Index: m.Index, Begin: m.Begin, Length: uint32(len(m.Block))}
	accepted, pieceDone := t.picker.Received(p.key, b)
	if !accepted {
		t.wasted.Add(n)
	} else {
		idx, begin, data, st := int(m.Index), int(m.Begin), m.Block, t.store
		t.writes[idx]++
		if pieceDone {
			t.toVerify[idx] = true
		}
		t.goJob(func() {
			t.send(evWritten{idx, st.WriteBlock(idx, begin, data)})
		})
	}
	t.fillAll()
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
	want := piece.PipelineDepth(p.rate(t.s.clk.Now())) - t.picker.Outstanding(p.key)
	if want <= 0 {
		return
	}
	for _, b := range t.picker.Pick(p.key, want) {
		if err := p.conn.Send(peer.Message{ID: peer.MsgRequest, Index: b.Index, Begin: b.Begin, Length: b.Length}); err != nil {
			return
		}
	}
}
