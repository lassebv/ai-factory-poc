package piece

import (
	"slices"
	"time"
)

// pickState carries one Pick call.
type pickState struct {
	p   PeerKey
	pi  *peerInfo
	now time.Time
	max int
	out []Block
}

func (s *pickState) full() bool { return len(s.out) >= s.max }

// Pick returns up to max blocks to request from p (ADR-0018 rules 1-5).
func (pk *picker) Pick(p PeerKey, max int) []Block {
	pi := pk.peers[p]
	if pi == nil || max <= 0 || pi.interest == 0 {
		return nil
	}
	now := pk.clk.Now()
	if now.Before(pi.cooldown) {
		return nil
	}
	s := &pickState{p: p, pi: pi, now: now, max: max}
	for c := PriorityHigh; c > PrioritySkip && !s.full(); c-- {
		before := len(s.out)
		pk.pickOwned(s, c)
		if !s.full() {
			pk.pickFresh(s, c)
		}
		if !s.full() && len(s.out) == before {
			pk.pickStarted(s, c)
		}
		if !s.full() && pk.needed == 0 {
			pk.pickEndgame(s, c)
		}
	}
	if len(s.out) == 0 {
		return nil
	}
	return s.out
}

// mayTake reports whether p may get blocks of piece i through rules 3-5
// (exclusive pieces go to one peer only).
func (pk *picker) mayTake(s *pickState, i int) bool {
	ps := &pk.pieces[i]
	return !ps.exclusive || !ps.exHeld || ps.exOwner == s.p
}

// claim records that p takes blocks of piece i.
func (pk *picker) claim(s *pickState, i int) {
	ps := &pk.pieces[i]
	if ps.blocks == nil {
		ps.blocks = make([]blockInfo, pk.l.NumBlocks(i))
	}
	if ps.exclusive && !ps.exHeld {
		ps.exHeld, ps.exOwner = true, s.p
	}
	s.pi.owned.add(i)
}

// request marks block j of piece i outstanding for p.
func (pk *picker) request(s *pickState, i, j int) {
	ps := &pk.pieces[i]
	bi := &ps.blocks[j]
	if len(bi.owners) == 0 {
		ps.nBusy++
		pk.addNeeded(i, -1)
	}
	bi.owners = append(bi.owners, owner{peer: s.p, at: s.now})
	b := pk.block(i, j)
	s.pi.outstanding[b] = s.now
	pk.queue = append(pk.queue, expiry{peer: s.p, block: b, at: s.now})
	s.out = append(s.out, b)
}

// takeNeeded requests the needed blocks of piece i in ascending order.
func (pk *picker) takeNeeded(s *pickState, i int) {
	if pk.needOf(i) == 0 {
		return
	}
	pk.claim(s, i)
	ps := &pk.pieces[i]
	for j := range ps.blocks {
		if s.full() {
			break
		}
		bi := &ps.blocks[j]
		if !bi.received && len(bi.owners) == 0 {
			pk.request(s, i, j)
		}
	}
	pk.settle(i)
}

// pickOwned is rule 2: finish the pieces p already works on.
func (pk *picker) pickOwned(s *pickState, c Priority) {
	owned := &s.pi.owned
	for k := 0; k < len(owned.items) && !s.full(); {
		i := owned.items[k]
		ps := &pk.pieces[i]
		if ps.have || ps.pending {
			owned.remove(i) // swaps another item into position k
			continue
		}
		k++
		if ps.prio != c || !s.pi.bf.Has(i) || !pk.mayTake(s, i) {
			continue
		}
		pk.takeNeeded(s, i)
	}
}

// pickFresh is rule 3: random-first, then rarest-first among fresh pieces.
func (pk *picker) pickFresh(s *pickState, c Priority) {
	for !s.full() {
		i, ok := pk.nextFresh(s, c)
		if !ok {
			return
		}
		pk.takeNeeded(s, i)
	}
}

// nextFresh returns a fresh piece of class c that p has.
func (pk *picker) nextFresh(s *pickState, c Priority) (int, bool) {
	if pk.haveN < RandomFirstPieces {
		return pk.scan(s, &pk.freshAll[c])
	}
	for a := 1; a < len(pk.buckets[c]); a++ {
		if i, ok := pk.scan(s, &pk.buckets[c][a]); ok {
			return i, true
		}
	}
	return 0, false
}

// scan returns a member of set that p has and may take, starting at a
// random position (the random tie-break).
func (pk *picker) scan(s *pickState, set *idxSet) (int, bool) {
	n := len(set.items)
	if n == 0 {
		return 0, false
	}
	start := pk.rnd.IntN(n)
	for k := 0; k < n; k++ {
		i := set.items[(start+k)%n]
		if s.pi.bf.Has(i) && pk.mayTake(s, i) {
			return i, true
		}
	}
	return 0, false
}

// pickStarted is rule 4: needed blocks of pieces other peers started.
func (pk *picker) pickStarted(s *pickState, c Priority) {
	items := slices.Clone(pk.active[c].items)
	for _, i := range items {
		if s.full() {
			return
		}
		if s.pi.bf.Has(i) && pk.mayTake(s, i) {
			pk.takeNeeded(s, i)
		}
	}
}

// pickEndgame is rule 5: duplicate outstanding blocks, fewest owners first.
func (pk *picker) pickEndgame(s *pickState, c Priority) {
	type cand struct{ i, j, owners int }
	var cands []cand
	for _, i := range pk.active[c].items {
		ps := &pk.pieces[i]
		if !s.pi.bf.Has(i) || !pk.mayTake(s, i) || ps.blocks == nil {
			continue
		}
		for j := range ps.blocks {
			bi := &ps.blocks[j]
			n := len(bi.owners)
			if bi.received || n == 0 || n >= EndgameMaxOwners {
				continue
			}
			if _, mine := s.pi.outstanding[pk.block(i, j)]; mine {
				continue
			}
			cands = append(cands, cand{i, j, n})
		}
	}
	slices.SortStableFunc(cands, func(a, b cand) int {
		if a.owners != b.owners {
			return a.owners - b.owners
		}
		if a.i != b.i {
			return a.i - b.i
		}
		return a.j - b.j
	})
	for _, x := range cands {
		if s.full() {
			return
		}
		pk.claim(s, x.i)
		pk.request(s, x.i, x.j)
	}
}
