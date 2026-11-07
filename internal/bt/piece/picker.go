package piece

// BlockSize is the request unit: 16 KiB (BEP 3). Only the final block of
// the final piece may be shorter.
const BlockSize = 16 * 1024

// Block identifies one request-sized range of a piece.
type Block struct {
	Piece  int
	Begin  uint32
	Length uint32
}

const (
	blockFree uint8 = iota
	blockRequested
	blockReceived
)

// pieceState tracks the blocks of a piece that is being downloaded.
type pieceState struct {
	state    []uint8
	reqs     []uint8 // outstanding requests per block (endgame may duplicate)
	received int
}

// Picker decides which blocks to request. It is not safe for concurrent
// use: the torrent actor owns it.
//
// Selection order: blocks of pieces already in progress (lowest index
// first, so pieces complete quickly), then new pieces rarest-first with
// ties broken by the lowest index, then — once every missing block has been
// requested at least once — endgame duplicates of outstanding blocks.
type Picker struct {
	numPieces int
	pieceLen  int64
	total     int64

	have   *Bitfield
	avail  []int
	active map[int]*pieceState
	free   int64 // blocks not requested by anyone, over all missing pieces
}

// NewPicker returns a picker for a torrent of total bytes split into pieces
// of pieceLen bytes. Nothing is marked as had.
func NewPicker(total, pieceLen int64) *Picker {
	n := int((total + pieceLen - 1) / pieceLen)
	p := &Picker{
		numPieces: n,
		pieceLen:  pieceLen,
		total:     total,
		have:      NewBitfield(n),
		avail:     make([]int, n),
		active:    map[int]*pieceState{},
	}
	for i := 0; i < n; i++ {
		p.free += int64(p.NumBlocks(i))
	}
	return p
}

// NumPieces returns the number of pieces.
func (p *Picker) NumPieces() int { return p.numPieces }

// PieceSize returns the size of piece i in bytes.
func (p *Picker) PieceSize(i int) int64 {
	if i == p.numPieces-1 {
		return p.total - int64(i)*p.pieceLen
	}
	return p.pieceLen
}

// NumBlocks returns the number of blocks in piece i.
func (p *Picker) NumBlocks(i int) int {
	return int((p.PieceSize(i) + BlockSize - 1) / BlockSize)
}

// block returns block j of piece i.
func (p *Picker) block(i, j int) Block {
	begin := int64(j) * BlockSize
	return Block{Piece: i, Begin: uint32(begin), Length: uint32(min(BlockSize, p.PieceSize(i)-begin))}
}

// Valid reports whether b is exactly one of the torrent's blocks.
func (p *Picker) Valid(b Block) bool {
	if b.Piece < 0 || b.Piece >= p.numPieces || b.Begin%BlockSize != 0 {
		return false
	}
	j := int(b.Begin / BlockSize)
	return j < p.NumBlocks(b.Piece) && p.block(b.Piece, j) == b
}

// Have returns the bitfield of verified pieces. The caller must not modify it.
func (p *Picker) Have() *Bitfield { return p.have }

// Complete reports whether every piece is verified.
func (p *Picker) Complete() bool { return p.have.Full() }

// AddPeer counts a peer's pieces towards availability.
func (p *Picker) AddPeer(bf *Bitfield) { p.adjust(bf, 1) }

// RemovePeer removes a disconnected peer's pieces from availability.
func (p *Picker) RemovePeer(bf *Bitfield) { p.adjust(bf, -1) }

func (p *Picker) adjust(bf *Bitfield, d int) {
	for i := 0; i < p.numPieces; i++ {
		if bf.Has(i) {
			p.avail[i] += d
		}
	}
}

// PeerHave records that a peer announced piece i.
func (p *Picker) PeerHave(i int) {
	if i >= 0 && i < p.numPieces {
		p.avail[i]++
	}
}

// Interesting reports whether a peer with bf has a piece we lack.
func (p *Picker) Interesting(bf *Bitfield) bool {
	for i := 0; i < p.numPieces; i++ {
		if bf.Has(i) && !p.have.Has(i) {
			return true
		}
	}
	return false
}

// Pick returns up to n blocks to request from a peer that has the pieces in
// bf. outstanding reports whether the peer already has a request out for a
// block; such blocks are never returned. The returned blocks are marked as
// requested.
func (p *Picker) Pick(bf *Bitfield, n int, outstanding func(Block) bool) []Block {
	var out []Block
	take := func(i int, ps *pieceState, endgame bool) {
		for j := range ps.state {
			if len(out) >= n {
				return
			}
			s := ps.state[j]
			if s == blockFree || (endgame && s == blockRequested && !outstanding(p.block(i, j))) {
				if s == blockFree {
					p.free--
				}
				ps.state[j] = blockRequested
				if ps.reqs[j] < 255 {
					ps.reqs[j]++
				}
				out = append(out, p.block(i, j))
			}
		}
	}
	// 1. Pieces in progress, lowest index first.
	for i := 0; i < p.numPieces && len(out) < n && len(p.active) > 0; i++ {
		if ps := p.active[i]; ps != nil && bf.Has(i) {
			take(i, ps, false)
		}
	}
	// 2. New pieces, rarest first.
	for len(out) < n {
		best := -1
		for i := 0; i < p.numPieces; i++ {
			if bf.Has(i) && !p.have.Has(i) && p.active[i] == nil && (best < 0 || p.avail[i] < p.avail[best]) {
				best = i
			}
		}
		if best < 0 {
			break
		}
		k := p.NumBlocks(best)
		ps := &pieceState{state: make([]uint8, k), reqs: make([]uint8, k)}
		p.active[best] = ps
		take(best, ps, false)
	}
	// 3. Endgame: every missing block has been requested at least once.
	if len(out) < n && p.free == 0 {
		for i := 0; i < p.numPieces && len(out) < n; i++ {
			if ps := p.active[i]; ps != nil && bf.Has(i) {
				take(i, ps, true)
			}
		}
	}
	return out
}

// Unrequest returns a block whose request was dropped (choke, disconnect,
// timeout) to the pool, unless another request for it is still out.
func (p *Picker) Unrequest(b Block) {
	ps, j := p.lookup(b)
	if ps == nil || ps.state[j] != blockRequested {
		return
	}
	if ps.reqs[j] > 0 {
		ps.reqs[j]--
	}
	if ps.reqs[j] == 0 {
		ps.state[j] = blockFree
		p.free++
	}
}

// Received marks a block as received. fresh is false when the block was
// invalid, not in progress, or already received (a duplicate). done is true
// when this block completed its piece.
func (p *Picker) Received(b Block) (fresh, done bool) {
	ps, j := p.lookup(b)
	if ps == nil || ps.state[j] == blockReceived {
		return false, false
	}
	if ps.state[j] == blockFree {
		p.free--
	}
	ps.state[j] = blockReceived
	ps.reqs[j] = 0
	ps.received++
	return true, ps.received == len(ps.state)
}

// Requested reports whether b is currently in the requested state.
func (p *Picker) Requested(b Block) bool {
	ps, j := p.lookup(b)
	return ps != nil && ps.state[j] == blockRequested
}

func (p *Picker) lookup(b Block) (*pieceState, int) {
	if !p.Valid(b) {
		return nil, 0
	}
	ps := p.active[b.Piece]
	if ps == nil {
		return nil, 0
	}
	return ps, int(b.Begin / BlockSize)
}

// Failed resets a piece that failed its hash check, so all of its blocks
// are requested again.
func (p *Picker) Failed(i int) {
	if ps := p.active[i]; ps != nil {
		for _, s := range ps.state {
			if s != blockFree {
				p.free++
			}
		}
		delete(p.active, i)
	}
}

// Verified marks piece i as verified and stops tracking its blocks.
func (p *Picker) Verified(i int) {
	if i < 0 || i >= p.numPieces || p.have.Has(i) {
		return
	}
	if ps := p.active[i]; ps != nil {
		for _, s := range ps.state {
			if s == blockFree {
				p.free--
			}
		}
		delete(p.active, i)
	} else {
		p.free -= int64(p.NumBlocks(i))
	}
	p.have.Set(i)
}
