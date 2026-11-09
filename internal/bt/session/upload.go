package session

import (
	"math/rand/v2"
	"slices"
	"time"

	"torrentd/internal/bt/peer"
	"torrentd/internal/bt/piece"
)

// Choking (§4.1).
const (
	unchokeSlots      = 4
	rechokeInterval   = 10 * time.Second
	optimisticRotate  = 30 * time.Second
	maxRequestLength  = piece.BlockSize
	rateSmoothingDivs = 2 // rate = (rate + bytes in the last second) / 2
)

// request queues a block request from p (§4.1 seeding).
func (t *torrent) request(p *peerState, m peer.Message) {
	if m.Length > maxRequestLength {
		// No fast extension, so no reject_request: disconnect (T6).
		t.s.log.Debug("oversized request", "peer", p.addr, "length", m.Length)
		t.dropPeer(p.key, p)
		return
	}
	i := int(m.Index)
	if m.Length == 0 || i >= t.layout.NumPieces || int64(m.Begin)+int64(m.Length) > t.layout.PieceSize(i) {
		t.dropPeer(p.key, p)
		return
	}
	if p.amChoking || !t.active() || !t.picker.Have().Has(i) || len(p.upQueue) >= maxUpQueue {
		return
	}
	p.upQueue = append(p.upQueue, piece.Block{Index: m.Index, Begin: m.Begin, Length: m.Length})
	t.pump(p)
}

// cancelRequest drops a queued request.
func (t *torrent) cancelRequest(p *peerState, b piece.Block) {
	if k := slices.Index(p.upQueue, b); k >= 0 {
		p.upQueue = slices.Delete(p.upQueue, k, k+1)
	}
}

// pump starts disk reads for p's queued requests.
func (t *torrent) pump(p *peerState) {
	for p.upReads < maxUpReads && len(p.upQueue) > 0 && t.store != nil {
		b := p.upQueue[0]
		p.upQueue = p.upQueue[1:]
		p.upReads++
		key, st := p.key, t.store
		t.goJob(t.s.disk, func() {
			buf := make([]byte, b.Length)
			err := st.ReadBlock(int(b.Index), int(b.Begin), buf)
			t.send(evRead{key: key, b: b, data: buf, err: err})
		})
	}
}

// readDone sends a block that was read for a peer.
func (t *torrent) readDone(ev evRead) {
	p := t.peers[ev.key]
	if p == nil {
		return
	}
	p.upReads--
	if ev.err != nil {
		t.s.log.Debug("read block for upload", "torrent", t.m.HexHash(), "err", ev.err)
	} else if !p.amChoking && t.active() {
		if p.conn.Send(peer.Message{ID: peer.MsgPiece, Index: ev.b.Index, Begin: ev.b.Begin, Block: ev.data}) == nil {
			n := int64(len(ev.data))
			t.uploaded.Add(n)
			p.sentWin += n
		}
	}
	t.pump(p)
}

func (t *torrent) choke(p *peerState) {
	p.amChoking = true
	p.upQueue = nil // BEP 3: a choke discards pending requests
	_ = p.conn.Send(peer.Message{ID: peer.MsgChoke})
	if t.optimistic == p.key {
		t.optimistic = 0
	}
}

func (t *torrent) unchoke(p *peerState) {
	p.amChoking = false
	_ = p.conn.Send(peer.Message{ID: peer.MsgUnchoke})
}

// regularUnchoked counts unchoked peers outside the optimistic slot.
func (t *torrent) regularUnchoked() int {
	n := 0
	for k, p := range t.peers {
		if !p.amChoking && k != t.optimistic {
			n++
		}
	}
	return n
}

// fillSlots unchokes interested peers while slots are free, without
// waiting for the next rechoke round.
func (t *torrent) fillSlots() {
	if !t.active() {
		return
	}
	free := unchokeSlots - t.regularUnchoked()
	for _, p := range t.sortedPeers() {
		if !p.amChoking || !p.peerWants || (p.snubbed && t.state == StateDownloading) {
			continue
		}
		switch {
		case free > 0:
			t.unchoke(p)
			free--
		case t.optimistic == 0:
			t.optimistic, t.optSince = p.key, t.s.clk.Now()
			t.unchoke(p)
		}
	}
}

// rate is the choker's ranking rate for p.
func (t *torrent) rate(p *peerState) int64 {
	if t.state == StateSeeding {
		return p.rateUp
	}
	return p.rateDown
}

// sortedPeers returns the peers ordered by ranking rate (highest first),
// then by key (longest connected first).
func (t *torrent) sortedPeers() []*peerState {
	ps := make([]*peerState, 0, len(t.peers))
	for _, p := range t.peers {
		ps = append(ps, p)
	}
	slices.SortFunc(ps, func(a, b *peerState) int {
		if ra, rb := t.rate(a), t.rate(b); ra != rb {
			if ra > rb {
				return -1
			}
			return 1
		}
		return int(a.key) - int(b.key)
	})
	return ps
}

// rechoke is the 10 s tit-for-tat round: the 4 best interested peers get
// the regular slots, and every 30 s the optimistic slot moves to a random
// choked interested peer.
func (t *torrent) rechoke(now time.Time) {
	if !t.active() {
		return
	}
	downloading := t.state == StateDownloading
	if o := t.peers[t.optimistic]; o == nil || !o.peerWants {
		t.optimistic = 0
	}
	var regular, rest []*peerState
	for _, p := range t.sortedPeers() {
		if !p.peerWants || p.key == t.optimistic {
			continue
		}
		if len(regular) < unchokeSlots && !(downloading && p.snubbed) {
			regular = append(regular, p)
		} else {
			rest = append(rest, p)
		}
	}
	if (t.optimistic == 0 || now.Sub(t.optSince) >= optimisticRotate) && len(rest) > 0 {
		var pick *peerState
		t.s.withRand(func(r *rand.Rand) { pick = rest[r.IntN(len(rest))] })
		t.optimistic, t.optSince = pick.key, now
	}
	keep := map[piece.PeerKey]bool{t.optimistic: t.optimistic != 0}
	for _, p := range regular {
		keep[p.key] = true
	}
	for _, p := range t.peers { // chokes first, so at most 5 are ever unchoked
		if !p.amChoking && !keep[p.key] {
			t.choke(p)
		}
	}
	for k := range keep {
		if p := t.peers[k]; p != nil && p.amChoking {
			t.unchoke(p)
		}
	}
	t.lastRechoke = now
}

// tick runs every second: rates, request timeouts and the choker.
func (t *torrent) tick(now time.Time) {
	for _, p := range t.peers {
		p.rateDown = (p.rateDown + p.recvWin) / rateSmoothingDivs
		p.rateUp = (p.rateUp + p.sentWin) / rateSmoothingDivs
		p.recvWin, p.sentWin = 0, 0
	}
	t.checkTimeouts(now)
	t.fillAll() // stale requests free pipeline slots over time
	if now.Sub(t.lastRechoke) >= rechokeInterval {
		t.rechoke(now)
	}
}
