package piece

import (
	"math/rand/v2"
	"slices"
)

// Layout geometry.

// PieceSize returns the size of piece i (see Layout); 0 for i out of range.
func (l Layout) PieceSize(i int) int64 {
	if i < 0 || i >= l.NumPieces {
		return 0
	}
	if i < l.NumPieces-1 {
		return l.PieceLength
	}
	s := l.TotalLength - int64(l.NumPieces-1)*l.PieceLength
	if s < 0 {
		return 0
	}
	return s
}

// NumBlocks returns ceil(PieceSize(i)/BlockSize); 0 for i out of range.
func (l Layout) NumBlocks(i int) int {
	return int((l.PieceSize(i) + BlockSize - 1) / BlockSize)
}

// blockLen returns the length of block j of piece i.
func (l Layout) blockLen(i, j int) uint32 {
	rest := l.PieceSize(i) - int64(j)*BlockSize
	if rest > BlockSize {
		return BlockSize
	}
	return uint32(rest)
}

func pipelineDepth(rate int64) int {
	if rate <= 0 {
		return MinPipelineDepth
	}
	d := rate / BlockSize * 2
	if r := rate % BlockSize; r != 0 {
		// rate*2/BlockSize without overflowing for huge rates.
		d += r * 2 / BlockSize
	}
	if d < MinPipelineDepth {
		return MinPipelineDepth
	}
	if d > MaxPipelineDepth {
		return MaxPipelineDepth
	}
	return int(d)
}

// Picker state.

type pieceState uint8

const (
	pieceNeeded  pieceState = iota // no block outstanding or received
	pieceActive                    // in progress: some block outstanding or received
	piecePending                   // every block received; awaiting the hash result
	pieceHave                      // verified
)

type blockState uint8

const (
	blockNeeded blockState = iota
	blockOutstanding
	blockReceived
)

// pieceInfo is the per-piece bookkeeping. blocks and owners are allocated
// only while the piece is active.
type pieceInfo struct {
	state       pieceState
	prio        Priority
	avail       int    // number of registered peers having the piece
	rank        uint32 // random tie-break (the index when rnd is nil)
	outstanding int    // blocks with at least one owner
	received    int
	blocks      []blockState
	owners      [][]PeerKey // owners[j]: peers block j is outstanding for
}

type peerInfo struct {
	have        *Bitfield
	outstanding map[Block]struct{}
}

type picker struct {
	l      Layout
	pieces []pieceInfo
	peers  map[PeerKey]*peerInfo
	have   *Bitfield
	rnd    *rand.Rand
	active []int // indices of active pieces (unordered)

	unrequested int // needed blocks of wanted pieces that are neither have nor pending
	missing     int // wanted pieces that are not have
	verified    int // pieces that reached have (random-first counts these)

	cand []int // scratch buffer for Pick
}

func newPicker(l Layout, rnd *rand.Rand) Picker {
	n := max(l.NumPieces, 0)
	l.NumPieces = n
	pk := &picker{
		l:       l,
		pieces:  make([]pieceInfo, n),
		peers:   make(map[PeerKey]*peerInfo),
		have:    NewBitfield(n),
		rnd:     rnd,
		missing: n,
	}
	var perm []int
	if rnd != nil {
		perm = rnd.Perm(n)
	}
	for i := range pk.pieces {
		ps := &pk.pieces[i]
		ps.prio = PriorityNormal
		ps.rank = uint32(i)
		if perm != nil {
			ps.rank = uint32(perm[i])
		}
		pk.unrequested += l.NumBlocks(i)
	}
	return pk
}

func (pk *picker) peer(p PeerKey) *peerInfo {
	pi := pk.peers[p]
	if pi == nil {
		pi = &peerInfo{have: NewBitfield(pk.l.NumPieces), outstanding: make(map[Block]struct{})}
		pk.peers[p] = pi
	}
	return pi
}

// AddPeer registers p with no pieces.
func (pk *picker) AddPeer(p PeerKey) { pk.peer(p) }

// RemovePeer forgets p's availability and releases its outstanding blocks.
func (pk *picker) RemovePeer(p PeerKey) {
	pi := pk.peers[p]
	if pi == nil {
		return
	}
	pk.Release(p)
	for i := range pk.pieces {
		if pi.have.Has(i) {
			pk.pieces[i].avail--
		}
	}
	delete(pk.peers, p)
}

// PeerBitfield replaces p's availability with bf.
func (pk *picker) PeerBitfield(p PeerKey, bf *Bitfield) {
	if bf == nil || bf.Len() != pk.l.NumPieces {
		return
	}
	pi := pk.peer(p)
	for i := range pk.pieces {
		was, now := pi.have.Has(i), bf.Has(i)
		switch {
		case was && !now:
			pk.pieces[i].avail--
		case !was && now:
			pk.pieces[i].avail++
		}
	}
	pi.have = bf.Clone()
}

// PeerHave adds index to p's availability.
func (pk *picker) PeerHave(p PeerKey, index int) {
	if index < 0 || index >= pk.l.NumPieces {
		return
	}
	pi := pk.peer(p)
	if !pi.have.Has(index) {
		pi.have.Set(index)
		pk.pieces[index].avail++
	}
}

// wanted reports whether piece i may be picked at all.
func (pk *picker) wanted(i int) bool { return pk.pieces[i].prio > PrioritySkip }

// needed returns the number of needed blocks of an active piece.
func (ps *pieceInfo) needed() int { return len(ps.blocks) - ps.outstanding - ps.received }

// foreign reports whether an active piece has a block outstanding for a
// peer other than p.
func (ps *pieceInfo) foreign(p PeerKey) bool {
	for _, os := range ps.owners {
		for _, o := range os {
			if o != p {
				return true
			}
		}
	}
	return false
}

// sortedActive returns the active pieces ordered by priority (highest
// first), then index.
func (pk *picker) sortedActive() []int {
	act := slices.Clone(pk.active)
	slices.SortFunc(act, func(a, b int) int {
		if d := int(pk.pieces[b].prio) - int(pk.pieces[a].prio); d != 0 {
			return d
		}
		return a - b
	})
	return act
}

// Pick returns up to max blocks for p (see the Picker doc for the order)
// and marks them outstanding for p.
func (pk *picker) Pick(p PeerKey, max int) []Block {
	pi := pk.peers[p]
	if pi == nil || max <= 0 || pk.missing == 0 {
		return nil
	}
	var out []Block
	act := pk.sortedActive()
	// 1. Active pieces nobody else is working on (piece affinity).
	for _, i := range act {
		if len(out) >= max {
			return out
		}
		ps := &pk.pieces[i]
		if pi.have.Has(i) && pk.wanted(i) && ps.needed() > 0 && !ps.foreign(p) {
			out = pk.take(p, pi, i, out, max)
		}
	}
	// 2. New pieces.
	if len(out) < max {
		out = pk.pickNew(p, pi, out, max)
	}
	// 3. Needed blocks of pieces other peers are working on.
	for _, i := range act {
		if len(out) >= max {
			return out
		}
		ps := &pk.pieces[i]
		if ps.state == pieceActive && pi.have.Has(i) && pk.wanted(i) && ps.needed() > 0 {
			out = pk.take(p, pi, i, out, max)
		}
	}
	// 4. Endgame: every wanted block has been requested; duplicate the
	// outstanding ones (A5).
	if len(out) < max && pk.unrequested == 0 {
		out = pk.endgame(p, pi, act, out, max)
	}
	return out
}

// better reports whether new piece a should be picked before b.
func (pk *picker) better(a, b int, randomFirst bool) bool {
	pa, pb := &pk.pieces[a], &pk.pieces[b]
	if pa.prio != pb.prio {
		return pa.prio > pb.prio
	}
	if !randomFirst && pa.avail != pb.avail {
		return pa.avail < pb.avail
	}
	return pa.rank < pb.rank
}

// pickNew selects new pieces for p: strict priority classes, random-first
// until 4 pieces are verified (only with a random source), then rarest
// first with random ties. It scans the pieces once and keeps only as many
// candidates as can fill max blocks, so it never sorts every piece.
func (pk *picker) pickNew(p PeerKey, pi *peerInfo, out []Block, max int) []Block {
	randomFirst := pk.rnd != nil && pk.verified < 4
	need := max - len(out)
	per := pk.l.NumBlocks(0)
	if per <= 0 {
		per = 1
	}
	k := (need+per-1)/per + 1
	cand := pk.cand[:0]
	worst := -1 // index into cand of the worst kept candidate
	for i := range pk.pieces {
		ps := &pk.pieces[i]
		if ps.state != pieceNeeded || ps.prio <= PrioritySkip || !pi.have.Has(i) || pk.l.NumBlocks(i) == 0 {
			continue
		}
		if len(cand) < k {
			cand = append(cand, i)
			if worst < 0 || pk.better(cand[worst], i, randomFirst) {
				worst = len(cand) - 1
			}
			continue
		}
		if !pk.better(i, cand[worst], randomFirst) {
			continue
		}
		cand[worst] = i
		worst = 0
		for j := 1; j < len(cand); j++ {
			if pk.better(cand[worst], cand[j], randomFirst) {
				worst = j
			}
		}
	}
	slices.SortFunc(cand, func(a, b int) int {
		if pk.better(a, b, randomFirst) {
			return -1
		}
		if pk.better(b, a, randomFirst) {
			return 1
		}
		return 0
	})
	for _, i := range cand {
		if len(out) >= max {
			break
		}
		out = pk.take(p, pi, i, out, max)
	}
	pk.cand = cand[:0]
	return out
}

// endgame appends blocks outstanding for other peers (and not for p).
func (pk *picker) endgame(p PeerKey, pi *peerInfo, act []int, out []Block, max int) []Block {
	for _, i := range act {
		ps := &pk.pieces[i]
		if ps.state != pieceActive || !pi.have.Has(i) || !pk.wanted(i) {
			continue
		}
		for j := range ps.blocks {
			if len(out) >= max {
				return out
			}
			if ps.blocks[j] != blockOutstanding || slices.Contains(ps.owners[j], p) {
				continue
			}
			b := pk.block(i, j)
			ps.owners[j] = append(ps.owners[j], p)
			pi.outstanding[b] = struct{}{}
			out = append(out, b)
		}
	}
	return out
}

func (pk *picker) block(i, j int) Block {
	return Block{Index: uint32(i), Begin: uint32(j * BlockSize), Length: pk.l.blockLen(i, j)}
}

// take appends needed blocks of piece i to out (up to max), marking them
// outstanding for p.
func (pk *picker) take(p PeerKey, pi *peerInfo, i int, out []Block, max int) []Block {
	ps := &pk.pieces[i]
	if ps.state == pieceNeeded {
		pk.activate(i)
	}
	for j := range ps.blocks {
		if len(out) >= max || ps.needed() == 0 {
			break
		}
		if ps.blocks[j] != blockNeeded {
			continue
		}
		b := pk.block(i, j)
		ps.blocks[j] = blockOutstanding
		ps.owners[j] = append(ps.owners[j][:0], p)
		ps.outstanding++
		pk.unrequested--
		pi.outstanding[b] = struct{}{}
		out = append(out, b)
	}
	return out
}

func (pk *picker) activate(i int) {
	ps := &pk.pieces[i]
	nb := pk.l.NumBlocks(i)
	ps.state = pieceActive
	ps.blocks = make([]blockState, nb)
	ps.owners = make([][]PeerKey, nb)
	ps.outstanding, ps.received = 0, 0
	pk.active = append(pk.active, i)
}

// deactivate removes i from the active list.
func (pk *picker) deactivate(i int) {
	if k := slices.Index(pk.active, i); k >= 0 {
		last := len(pk.active) - 1
		pk.active[k] = pk.active[last]
		pk.active = pk.active[:last]
	}
	ps := &pk.pieces[i]
	ps.blocks, ps.owners = nil, nil
	ps.outstanding, ps.received = 0, 0
}

// blockIndex validates b against the layout and returns its block number.
func (pk *picker) blockIndex(b Block) (int, bool) {
	i := int(b.Index)
	if i >= pk.l.NumPieces || b.Begin%BlockSize != 0 {
		return 0, false
	}
	j := int(b.Begin / BlockSize)
	if j >= pk.l.NumBlocks(i) || b.Length != pk.l.blockLen(i, j) {
		return 0, false
	}
	return j, true
}

// disown removes every owner of block j of piece i and returns them. The
// block state is left to the caller.
func (pk *picker) disown(i, j int) []PeerKey {
	ps := &pk.pieces[i]
	owners := ps.owners[j]
	b := pk.block(i, j)
	for _, o := range owners {
		if op := pk.peers[o]; op != nil {
			delete(op.outstanding, b)
		}
	}
	ps.owners[j] = nil
	return owners
}

// Received records a block from p; see the Picker doc.
func (pk *picker) Received(p PeerKey, b Block) (accepted, pieceDone bool, cancel []PeerKey) {
	j, ok := pk.blockIndex(b)
	if !ok {
		return false, false, nil
	}
	i := int(b.Index)
	ps := &pk.pieces[i]
	if ps.state != pieceActive || ps.blocks[j] != blockOutstanding || !slices.Contains(ps.owners[j], p) {
		return false, false, nil
	}
	for _, o := range pk.disown(i, j) {
		if o != p {
			cancel = append(cancel, o)
		}
	}
	ps.blocks[j] = blockReceived
	ps.outstanding--
	ps.received++
	if ps.received == len(ps.blocks) {
		ps.state = piecePending
		pk.deactivate(i)
		return true, true, cancel
	}
	return true, false, cancel
}

// Release makes p's outstanding blocks needed again (unless another peer
// still has them outstanding) and returns them.
func (pk *picker) Release(p PeerKey) []Block {
	pi := pk.peers[p]
	if pi == nil || len(pi.outstanding) == 0 {
		return nil
	}
	out := make([]Block, 0, len(pi.outstanding))
	for b := range pi.outstanding {
		out = append(out, b)
		i, j := int(b.Index), int(b.Begin/BlockSize)
		ps := &pk.pieces[i]
		if ps.state != pieceActive || ps.blocks[j] != blockOutstanding {
			continue
		}
		ps.owners[j] = slices.DeleteFunc(ps.owners[j], func(o PeerKey) bool { return o == p })
		if len(ps.owners[j]) == 0 {
			ps.blocks[j] = blockNeeded
			ps.outstanding--
			if pk.wanted(i) {
				pk.unrequested++
			}
			pk.settle(i)
		}
	}
	clear(pi.outstanding)
	slices.SortFunc(out, func(a, b Block) int {
		if a.Index != b.Index {
			return int(a.Index) - int(b.Index)
		}
		return int(a.Begin) - int(b.Begin)
	})
	return out
}

// settle returns an active piece with nothing outstanding or received to
// the needed state, so that it is ranked as a new piece again.
func (pk *picker) settle(i int) {
	ps := &pk.pieces[i]
	if ps.state == pieceActive && ps.outstanding == 0 && ps.received == 0 {
		ps.state = pieceNeeded
		pk.deactivate(i)
	}
}

// Outstanding returns the number of blocks outstanding for p.
func (pk *picker) Outstanding(p PeerKey) int {
	if pi := pk.peers[p]; pi != nil {
		return len(pi.outstanding)
	}
	return 0
}

// remaining returns the needed blocks piece i contributes to unrequested.
func (pk *picker) remaining(i int) int {
	ps := &pk.pieces[i]
	switch ps.state {
	case pieceNeeded:
		return pk.l.NumBlocks(i)
	case pieceActive:
		return ps.needed()
	}
	return 0
}

// PieceVerified marks piece index as have, whatever its state.
func (pk *picker) PieceVerified(index int) {
	if index < 0 || index >= pk.l.NumPieces || pk.pieces[index].state == pieceHave {
		return
	}
	ps := &pk.pieces[index]
	if pk.wanted(index) {
		pk.unrequested -= pk.remaining(index)
		pk.missing--
	}
	if ps.state == pieceActive {
		for j := range ps.blocks {
			pk.disown(index, j)
		}
		pk.deactivate(index)
	}
	ps.state = pieceHave
	pk.have.Set(index)
	pk.verified++
}

// PieceFailed makes every block of a pending piece needed again.
func (pk *picker) PieceFailed(index int) {
	if index < 0 || index >= pk.l.NumPieces || pk.pieces[index].state != piecePending {
		return
	}
	pk.pieces[index].state = pieceNeeded
	if pk.wanted(index) {
		pk.unrequested += pk.l.NumBlocks(index)
	}
}

// Discard makes the received blocks of an active piece needed again.
func (pk *picker) Discard(index int) {
	if index < 0 || index >= pk.l.NumPieces || pk.pieces[index].state != pieceActive {
		return
	}
	ps := &pk.pieces[index]
	for j, st := range ps.blocks {
		if st == blockReceived {
			ps.blocks[j] = blockNeeded
			ps.received--
			if pk.wanted(index) {
				pk.unrequested++
			}
		}
	}
	pk.settle(index)
}

// SetPriority sets the priority class of piece index.
func (pk *picker) SetPriority(index int, prio Priority) {
	if index < 0 || index >= pk.l.NumPieces || prio < PrioritySkip || prio > PriorityHigh {
		return
	}
	ps := &pk.pieces[index]
	was, now := ps.prio > PrioritySkip, prio > PrioritySkip
	ps.prio = prio
	if was == now {
		return
	}
	d := 1
	if !now {
		d = -1
	}
	pk.unrequested += d * pk.remaining(index)
	if ps.state != pieceHave {
		pk.missing += d
	}
}

// Endgame reports whether every wanted block has been requested.
func (pk *picker) Endgame() bool { return pk.unrequested == 0 && pk.missing > 0 }

// Have returns a copy of the verified pieces.
func (pk *picker) Have() *Bitfield { return pk.have.Clone() }

// HaveCount returns the number of verified pieces.
func (pk *picker) HaveCount() int { return pk.verified }

// Done reports whether every wanted piece is have.
func (pk *picker) Done() bool { return pk.missing == 0 }
