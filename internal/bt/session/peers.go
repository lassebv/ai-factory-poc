package session

import (
	"errors"
	"io"
	"net/netip"
	"time"

	"torrentd/internal/bt/peer"
	"torrentd/internal/bt/piece"
)

// Pipeline depth bounds (D1).
const (
	minDepth = 5
	maxDepth = 250
)

// Reconnect backoff for a tracker peer that failed or disconnected.
const (
	retryBase = 5 * time.Second
	retryMax  = 5 * time.Minute
)

// candidate is a peer address learned from a tracker.
type candidate struct {
	addr      netip.AddrPort
	connected bool
	dialing   bool
	fails     int
	nextTry   time.Time
}

// peerState is the actor's view of one connected peer.
type peerState struct {
	conn         *peer.Conn
	c            *candidate
	has          *piece.Bitfield
	gotMessage   bool
	amInterested bool
	choked       bool // the peer is choking us
	outstanding  map[piece.Block]bool
	depth        int
	rate         float64
	tickBytes    int64
}

func (p *peerState) tick() {
	p.rate += (float64(p.tickBytes) - p.rate) * emaAlpha
	p.tickBytes = 0
	// D1: clamp(downloadRate × 2 s / 16 KiB, 5, 250).
	p.depth = min(max(int(p.rate*2/piece.BlockSize), minDepth), maxDepth)
}

// addCandidates records tracker peers and connects to new ones.
func (t *Torrent) addCandidates(addrs []netip.AddrPort) {
	for _, a := range addrs {
		if !a.IsValid() || a.Port() == 0 {
			continue
		}
		if _, ok := t.candidates[a]; !ok {
			t.candidates[a] = &candidate{addr: a}
		}
	}
	t.connectMore()
}

// connectMore dials candidates while the peer and half-open limits allow.
func (t *Torrent) connectMore() {
	if t.state != StateDownloading {
		return
	}
	now := t.s.clk.Now()
	for _, c := range t.candidates {
		if len(t.peers)+t.dialing >= t.s.cfg.MaxPeers {
			return
		}
		if c.connected || c.dialing || now.Before(c.nextTry) {
			continue
		}
		select {
		case t.s.halfOpen <- struct{}{}:
		default:
			return // the half-open limit is reached
		}
		c.dialing = true
		t.dialing++
		go t.dial(c)
	}
}

func (t *Torrent) dial(c *candidate) {
	cfg := peer.Config{
		InfoHash:    t.mi.InfoHash,
		PeerID:      t.s.peerID,
		MaxBitfield: (t.picker.NumPieces() + 7) / 8,
		Clock:       t.s.clk,
	}
	conn, err := peer.Dial(t.ctx, c.addr.String(), cfg)
	<-t.s.halfOpen
	if err == nil && conn.Remote().PeerID == t.s.peerID {
		conn.Close() // ourselves
		err = errors.New("connected to self")
	}
	if err != nil {
		t.log.Debug("peer connect failed", "addr", c.addr, "err", err)
		t.send(evDialFailed{c})
		return
	}
	t.send(evConnected{c, conn})
}

func (t *Torrent) retryLater(c *candidate) {
	c.dialing, c.connected = false, false
	c.fails++
	d := retryBase << min(c.fails-1, 10)
	c.nextTry = t.s.clk.Now().Add(min(d, retryMax))
}

func (t *Torrent) onConnected(c *candidate, conn *peer.Conn) {
	t.dialing--
	c.dialing = false
	if t.state != StateDownloading && t.state != StateSeeding || t.ctx.Err() != nil {
		conn.Close()
		return
	}
	c.connected = true
	p := &peerState{
		conn:        conn,
		c:           c,
		has:         piece.NewBitfield(t.picker.NumPieces()),
		choked:      true,
		outstanding: map[piece.Block]bool{},
		depth:       minDepth,
	}
	t.peers[p] = true
	t.log.Debug("peer connected", "addr", c.addr)
	conn.Start(t.ctx,
		func(m *peer.Message) { t.send(evMsg{p, m}) },
		func(err error) { t.send(evClosed{p, err}) })
	if t.picker.Have().Count() > 0 {
		conn.Send(&peer.Message{ID: peer.MsgBitfield, Payload: t.picker.Have().Bytes()})
	}
}

// drop closes a misbehaving peer. The close event cleans up.
func (t *Torrent) drop(p *peerState, why string) {
	t.log.Debug("dropping peer", "addr", p.c.addr, "reason", why)
	p.conn.Close()
}

func (t *Torrent) onClosed(p *peerState, err error) {
	if !t.peers[p] {
		return
	}
	delete(t.peers, p)
	t.picker.RemovePeer(p.has)
	for b := range p.outstanding {
		t.picker.Unrequest(b)
	}
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, peer.ErrClosed) {
		t.log.Debug("peer disconnected", "addr", p.c.addr, "err", err)
	}
	t.retryLater(p.c)
	for q := range t.peers {
		t.fillRequests(q)
	}
	t.connectMore()
}

// onMessage applies one message from a connected peer.
func (t *Torrent) onMessage(p *peerState, m *peer.Message) {
	first := !p.gotMessage
	p.gotMessage = true
	n := t.picker.NumPieces()
	switch m.ID {
	case peer.MsgBitfield:
		if !first {
			t.drop(p, "bitfield after other messages")
			return
		}
		bf, err := piece.FromBytes(m.Payload, n)
		if err != nil {
			t.drop(p, "bad bitfield")
			return
		}
		p.has = bf
		t.picker.AddPeer(bf)
		t.updateInterest(p)
	case peer.MsgHave:
		if int64(m.Index) >= int64(n) {
			t.drop(p, "have out of range")
			return
		}
		if !p.has.Has(int(m.Index)) {
			p.has.Set(int(m.Index))
			t.picker.PeerHave(int(m.Index))
		}
		t.updateInterest(p)
	case peer.MsgChoke:
		p.choked = true
		for b := range p.outstanding {
			t.picker.Unrequest(b)
		}
		clear(p.outstanding)
	case peer.MsgUnchoke:
		p.choked = false
		t.fillRequests(p)
	case peer.MsgPiece:
		t.onBlock(p, m)
	default:
		// interested, not interested, request, cancel and port: serving
		// peers is milestone 4. Unknown IDs are ignored (BEP 3).
	}
}

func (t *Torrent) updateInterest(p *peerState) {
	want := t.state == StateDownloading && t.picker.Interesting(p.has)
	if want != p.amInterested {
		p.amInterested = want
		id := peer.MsgNotInterested
		if want {
			id = peer.MsgInterested
		}
		p.conn.Send(&peer.Message{ID: id})
	}
	t.fillRequests(p)
}

// onBlock handles a piece message.
func (t *Torrent) onBlock(p *peerState, m *peer.Message) {
	b := piece.Block{Piece: int(m.Index), Begin: m.Begin, Length: uint32(len(m.Payload))}
	size := int64(len(m.Payload))
	t.downloaded += size
	t.tickBytes += size
	p.tickBytes += size
	if !p.outstanding[b] {
		t.wasted += size // unrequested, or the request was cancelled
		return
	}
	delete(p.outstanding, b)
	fresh, done := t.picker.Received(b)
	if !fresh {
		t.wasted += size // an endgame duplicate
		t.fillRequests(p)
		return
	}
	buf := t.bufs[b.Piece]
	if buf == nil {
		buf = make([]byte, t.picker.PieceSize(b.Piece))
		t.bufs[b.Piece] = buf
	}
	copy(buf[b.Begin:], m.Payload)
	// Cancel endgame duplicates of this block at other peers.
	for q := range t.peers {
		if q != p && q.outstanding[b] {
			delete(q.outstanding, b)
			q.conn.Send(&peer.Message{ID: peer.MsgCancel, Index: m.Index, Begin: m.Begin, Length: b.Length})
		}
	}
	if done {
		delete(t.bufs, b.Piece)
		i, st := b.Piece, t.st
		t.s.hash.Submit(func() {
			t.send(evHashed{piece: i, ok: st.Verify(i, buf), data: buf})
		})
	}
	t.fillRequests(p)
}

// fillRequests tops up the peer's request pipeline.
func (t *Torrent) fillRequests(p *peerState) {
	if p.choked || !p.amInterested || t.state != StateDownloading {
		return
	}
	want := p.depth - len(p.outstanding)
	if want <= 0 {
		return
	}
	for _, b := range t.picker.Pick(p.has, want, func(b piece.Block) bool { return p.outstanding[b] }) {
		p.outstanding[b] = true
		p.conn.Send(&peer.Message{ID: peer.MsgRequest, Index: uint32(b.Piece), Begin: b.Begin, Length: b.Length})
	}
}
