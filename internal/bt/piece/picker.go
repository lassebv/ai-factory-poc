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

// pieceInfo is the per-piece bookkeeping. blocks and owner are allocated
// only while the piece is active or pending.
type pieceInfo struct {
	state       pieceState
	avail       int // number of registered peers having the piece
	outstanding int
	received    int
	blocks      []blockState
	owner       []PeerKey // owner[j] is valid while blocks[j] is outstanding
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
	rnd    *rand.Rand // reserved for M4 random-first; unused at M3
}

func newPicker(l Layout, rnd *rand.Rand) Picker {
	n := max(l.NumPieces, 0)
	l.NumPieces = n
	return &picker{
		l:      l,
		pieces: make([]pieceInfo, n),
		peers:  make(map[PeerKey]*peerInfo),
		have:   NewBitfield(n),
		rnd:    rnd,
	}
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

// Pick returns up to max needed blocks for p (see the Picker doc for the
// order) and marks them outstanding for p.
func (pk *picker) Pick(p PeerKey, max int) []Block {
	pi := pk.peers[p]
	if pi == nil || max <= 0 {
		return nil
	}
	var out []Block
	// 1. In-progress pieces, lowest index first.
	for i := range pk.pieces {
		if len(out) >= max {
			break
		}
		if pk.pieces[i].state == pieceActive && pi.have.Has(i) {
			out = pk.take(p, pi, i, out, max)
		}
	}
	if len(out) >= max {
		return out
	}
	// 2. New pieces, rarest first, ties by lowest index.
	var cand []int
	for i := range pk.pieces {
		if pk.pieces[i].state == pieceNeeded && pi.have.Has(i) && pk.l.NumBlocks(i) > 0 {
			cand = append(cand, i)
		}
	}
	slices.SortFunc(cand, func(a, b int) int {
		if d := pk.pieces[a].avail - pk.pieces[b].avail; d != 0 {
			return d
		}
		return a - b
	})
	for _, i := range cand {
		if len(out) >= max {
			break
		}
		out = pk.take(p, pi, i, out, max)
	}
	return out
}

// take appends needed blocks of piece i to out (up to max), marking them
// outstanding for p.
func (pk *picker) take(p PeerKey, pi *peerInfo, i int, out []Block, max int) []Block {
	ps := &pk.pieces[i]
	if ps.state == pieceNeeded {
		pk.activate(i)
	}
	if ps.outstanding+ps.received >= len(ps.blocks) {
		return out
	}
	for j := range ps.blocks {
		if len(out) >= max {
			break
		}
		if ps.blocks[j] != blockNeeded {
			continue
		}
		b := Block{Index: uint32(i), Begin: uint32(j * BlockSize), Length: pk.l.blockLen(i, j)}
		ps.blocks[j] = blockOutstanding
		ps.owner[j] = p
		ps.outstanding++
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
	ps.owner = make([]PeerKey, nb)
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

// unown removes block j of piece i from its owner's outstanding set.
func (pk *picker) unown(i, j int) {
	ps := &pk.pieces[i]
	if ps.blocks[j] != blockOutstanding {
		return
	}
	if op := pk.peers[ps.owner[j]]; op != nil {
		delete(op.outstanding, Block{Index: uint32(i), Begin: uint32(j * BlockSize), Length: pk.l.blockLen(i, j)})
	}
	ps.blocks[j] = blockNeeded
	ps.outstanding--
}

// Received records a block from p.
func (pk *picker) Received(_ PeerKey, b Block) (accepted, pieceDone bool) {
	j, ok := pk.blockIndex(b)
	if !ok {
		return false, false
	}
	i := int(b.Index)
	ps := &pk.pieces[i]
	switch ps.state {
	case pieceHave, piecePending:
		return false, false
	case pieceNeeded:
		pk.activate(i)
	}
	if ps.blocks[j] == blockReceived {
		return false, false
	}
	pk.unown(i, j)
	ps.blocks[j] = blockReceived
	ps.received++
	if ps.received == len(ps.blocks) {
		ps.state = piecePending
		return true, true
	}
	return true, false
}

// Release makes p's outstanding blocks needed again.
func (pk *picker) Release(p PeerKey) {
	pi := pk.peers[p]
	if pi == nil {
		return
	}
	for b := range pi.outstanding {
		i, j := int(b.Index), int(b.Begin/BlockSize)
		pk.unown(i, j)
		pk.settle(i)
	}
	clear(pi.outstanding)
}

// settle returns an active piece with nothing outstanding or received to
// the needed state, so that it is ranked as a new piece again.
func (pk *picker) settle(i int) {
	ps := &pk.pieces[i]
	if ps.state == pieceActive && ps.outstanding == 0 && ps.received == 0 {
		ps.state = pieceNeeded
		ps.blocks, ps.owner = nil, nil
	}
}

// Outstanding returns the number of blocks outstanding for p.
func (pk *picker) Outstanding(p PeerKey) int {
	if pi := pk.peers[p]; pi != nil {
		return len(pi.outstanding)
	}
	return 0
}

// PieceVerified marks piece index as have, whatever its state.
func (pk *picker) PieceVerified(index int) {
	if index < 0 || index >= pk.l.NumPieces {
		return
	}
	ps := &pk.pieces[index]
	for j := range ps.blocks {
		pk.unown(index, j)
	}
	ps.state = pieceHave
	ps.blocks, ps.owner = nil, nil
	ps.outstanding, ps.received = 0, 0
	pk.have.Set(index)
}

// PieceFailed makes every block of a pending piece needed again.
func (pk *picker) PieceFailed(index int) {
	if index < 0 || index >= pk.l.NumPieces || pk.pieces[index].state != piecePending {
		return
	}
	ps := &pk.pieces[index]
	ps.state = pieceNeeded
	ps.blocks, ps.owner = nil, nil
	ps.outstanding, ps.received = 0, 0
}

// Have returns a copy of the verified pieces.
func (pk *picker) Have() *Bitfield { return pk.have.Clone() }

// Done reports whether every piece is have.
func (pk *picker) Done() bool { return pk.have.Full() }
