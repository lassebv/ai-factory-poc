package piece

import (
	"math/rand/v2"
	"net/netip"
	"time"

	"torrentd/internal/clock"
)

// numClasses is the number of priority classes (skip, low, normal, high).
const numClasses = 4

// owner is one peer a block is outstanding for.
type owner struct {
	peer PeerKey
	at   time.Time
}

// blockInfo is the state of one block of a piece.
type blockInfo struct {
	received bool
	// owners are the peers the block is outstanding for (≤ EndgameMaxOwners).
	owners []owner
	// released are peers the block was taken back from (late answers are
	// still accepted from them).
	released []PeerKey
}

// pieceInfo is the state of one piece.
type pieceInfo struct {
	prio    Priority
	have    bool
	pending bool
	avail   int
	blocks  []blockInfo // nil until the piece is first started
	nBusy   int         // blocks with at least one owner and not received
	nRecv   int         // received blocks
	// exclusive pieces (after a hash failure) are handed to one peer only.
	exclusive bool
	exOwner   PeerKey
	exHeld    bool
	contrib   []netip.Addr
	// where records the piece's membership: in the fresh structures or in
	// the active set of its class.
	inFresh  bool
	inActive bool
}

// peerInfo is the state of one registered peer.
type peerInfo struct {
	origin      netip.Addr
	bf          *Bitfield
	interest    int // pieces the peer has that are wanted and not have
	outstanding map[Block]time.Time
	owned       idxSet // pieces the peer has outstanding or received blocks in
	cooldown    time.Time
}

// expiry is one request in the order it was made (for Expire).
type expiry struct {
	peer  PeerKey
	block Block
	at    time.Time
}

// picker implements Picker (see contract.go).
type picker struct {
	l       Layout
	rnd     *rand.Rand
	clk     clock.Clock
	timeout time.Duration

	pieces []pieceInfo
	peers  map[PeerKey]*peerInfo
	have   *Bitfield

	// freshAll[c] holds the fresh (not started, not have, not pending)
	// pieces of class c; buckets[c][a] those with availability a.
	freshAll [numClasses]idxSet
	buckets  [numClasses][]idxSet
	// active[c] holds the started, not pending, not have pieces of class c.
	active [numClasses]idxSet

	needed     int // needed blocks of wanted, not-have pieces
	haveN      int // verified pieces
	wantedLeft int // wanted pieces that are not have

	queue []expiry
	qhead int
}

func newPicker(l Layout, opts Options) Picker {
	if l.NumPieces < 0 {
		l.NumPieces = 0
	}
	pk := &picker{
		l:       l,
		rnd:     opts.Rand,
		clk:     opts.Clock,
		timeout: opts.RequestTimeout,
		pieces:  make([]pieceInfo, l.NumPieces),
		peers:   map[PeerKey]*peerInfo{},
		have:    NewBitfield(l.NumPieces),
	}
	if pk.rnd == nil {
		pk.rnd = rand.New(rand.NewPCG(1, 2))
	}
	if pk.clk == nil {
		pk.clk = clock.Real()
	}
	if pk.timeout <= 0 {
		pk.timeout = DefaultRequestTimeout
	}
	for i := range pk.pieces {
		pk.pieces[i].prio = PriorityNormal
		pk.needed += l.NumBlocks(i)
		pk.wantedLeft++
		pk.addFresh(i)
	}
	return pk
}

// wanted reports whether a piece's class is requested at all.
func wanted(p Priority) bool { return p != PrioritySkip }

// needOf returns the number of needed blocks of piece i.
func (pk *picker) needOf(i int) int {
	ps := &pk.pieces[i]
	if ps.have || ps.pending {
		return 0
	}
	return pk.l.NumBlocks(i) - ps.nBusy - ps.nRecv
}

// addNeeded adjusts the global needed counter for piece i by d.
func (pk *picker) addNeeded(i, d int) {
	if wanted(pk.pieces[i].prio) {
		pk.needed += d
	}
}

func (pk *picker) bucket(c Priority, a int) *idxSet {
	for len(pk.buckets[c]) <= a {
		pk.buckets[c] = append(pk.buckets[c], idxSet{})
	}
	return &pk.buckets[c][a]
}

func (pk *picker) addFresh(i int) {
	ps := &pk.pieces[i]
	if ps.inFresh {
		return
	}
	ps.inFresh = true
	pk.freshAll[ps.prio].add(i)
	pk.bucket(ps.prio, ps.avail).add(i)
}

func (pk *picker) removeFresh(i int) {
	ps := &pk.pieces[i]
	if !ps.inFresh {
		return
	}
	ps.inFresh = false
	pk.freshAll[ps.prio].remove(i)
	pk.bucket(ps.prio, ps.avail).remove(i)
}

func (pk *picker) addActive(i int) {
	ps := &pk.pieces[i]
	if !ps.inActive {
		ps.inActive = true
		pk.active[ps.prio].add(i)
	}
}

func (pk *picker) removeActive(i int) {
	ps := &pk.pieces[i]
	if ps.inActive {
		ps.inActive = false
		pk.active[ps.prio].remove(i)
	}
}

// settle moves piece i between the fresh and active structures after its
// block counts changed.
func (pk *picker) settle(i int) {
	ps := &pk.pieces[i]
	switch {
	case ps.have || ps.pending:
		pk.removeFresh(i)
		pk.removeActive(i)
	case ps.nBusy+ps.nRecv == 0:
		pk.removeActive(i)
		pk.addFresh(i)
	default:
		pk.removeFresh(i)
		pk.addActive(i)
	}
}

func (pk *picker) setAvail(i, a int) {
	ps := &pk.pieces[i]
	if ps.inFresh {
		pk.bucket(ps.prio, ps.avail).remove(i)
		pk.bucket(ps.prio, a).add(i)
	}
	ps.avail = a
}

// counts reports whether piece i counts towards a peer's interest.
func (pk *picker) counts(i int) bool {
	ps := &pk.pieces[i]
	return wanted(ps.prio) && !ps.have
}

// AddPeer registers p.
func (pk *picker) AddPeer(p PeerKey, origin netip.Addr) {
	if _, ok := pk.peers[p]; ok {
		return
	}
	pk.peers[p] = &peerInfo{
		origin:      origin.WithZone(""),
		bf:          NewBitfield(pk.l.NumPieces),
		outstanding: map[Block]time.Time{},
	}
}

// RemovePeer forgets p.
func (pk *picker) RemovePeer(p PeerKey) {
	pi := pk.peers[p]
	if pi == nil {
		return
	}
	pk.takeBack(p, pi, false)
	for i := 0; i < pk.l.NumPieces; i++ {
		if pi.bf.Has(i) {
			pk.setAvail(i, pk.pieces[i].avail-1)
		}
	}
	delete(pk.peers, p)
}

// PeerBitfield replaces p's availability.
func (pk *picker) PeerBitfield(p PeerKey, bf *Bitfield) {
	pi := pk.peers[p]
	if pi == nil || bf == nil || bf.Len() != pk.l.NumPieces {
		return
	}
	for i := 0; i < pk.l.NumPieces; i++ {
		had, has := pi.bf.Has(i), bf.Has(i)
		if had == has {
			continue
		}
		if has {
			pk.setAvail(i, pk.pieces[i].avail+1)
			if pk.counts(i) {
				pi.interest++
			}
		} else {
			pk.setAvail(i, pk.pieces[i].avail-1)
			if pk.counts(i) {
				pi.interest--
			}
		}
	}
	pi.bf = bf.Clone()
}

// PeerHave adds one piece to p's availability.
func (pk *picker) PeerHave(p PeerKey, index int) {
	pi := pk.peers[p]
	if pi == nil || index < 0 || index >= pk.l.NumPieces || pi.bf.Has(index) {
		return
	}
	pi.bf.Set(index)
	pk.setAvail(index, pk.pieces[index].avail+1)
	if pk.counts(index) {
		pi.interest++
	}
}

// Interesting reports whether p has a wanted piece we lack.
func (pk *picker) Interesting(p PeerKey) bool {
	pi := pk.peers[p]
	return pi != nil && pi.interest > 0
}

// IsSeed reports whether p has every piece.
func (pk *picker) IsSeed(p PeerKey) bool {
	pi := pk.peers[p]
	return pi != nil && pi.bf.Full()
}

// Outstanding returns the number of blocks outstanding for p.
func (pk *picker) Outstanding(p PeerKey) int {
	if pi := pk.peers[p]; pi != nil {
		return len(pi.outstanding)
	}
	return 0
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

func (pk *picker) block(i, j int) Block {
	return Block{Index: uint32(i), Begin: uint32(j * BlockSize), Length: pk.l.blockLen(i, j)}
}

// dropOwner removes p from the owners of block j of piece i. If remember
// is set, p is recorded as released (late answers stay acceptable). It
// keeps the counters consistent.
func (pk *picker) dropOwner(i, j int, p PeerKey, remember bool) {
	ps := &pk.pieces[i]
	bi := &ps.blocks[j]
	for k, o := range bi.owners {
		if o.peer == p {
			bi.owners = append(bi.owners[:k], bi.owners[k+1:]...)
			break
		}
	}
	if remember && !containsKey(bi.released, p) {
		bi.released = append(bi.released, p)
	}
	if len(bi.owners) == 0 && !bi.received {
		ps.nBusy--
		pk.addNeeded(i, 1)
	}
}

// takeBack releases every block outstanding for p.
func (pk *picker) takeBack(p PeerKey, pi *peerInfo, remember bool) {
	touched := map[int]bool{}
	for b := range pi.outstanding {
		j, _ := pk.blockIndex(b)
		i := int(b.Index)
		pk.dropOwner(i, j, p, remember)
		touched[i] = true
	}
	clear(pi.outstanding)
	for i := range touched {
		pk.settle(i)
	}
	for i := range pk.exclusiveOf(p) {
		pk.pieces[i].exHeld = false
	}
}

// exclusiveOf returns the exclusive pieces held by p (from its owned set).
func (pk *picker) exclusiveOf(p PeerKey) map[int]bool {
	out := map[int]bool{}
	if pi := pk.peers[p]; pi != nil {
		for _, i := range pi.owned.items {
			ps := &pk.pieces[i]
			if ps.exclusive && ps.exHeld && ps.exOwner == p {
				out[i] = true
			}
		}
	}
	return out
}

// Release takes back every block outstanding for p.
func (pk *picker) Release(p PeerKey) {
	if pi := pk.peers[p]; pi != nil {
		pk.takeBack(p, pi, true)
	}
}

// Expire takes back blocks outstanding for longer than the timeout.
func (pk *picker) Expire() []Expired {
	now := pk.clk.Now()
	var out []Expired
	touched := map[int]bool{}
	for pk.qhead < len(pk.queue) {
		e := pk.queue[pk.qhead]
		if now.Sub(e.at) < pk.timeout {
			break
		}
		pk.queue[pk.qhead] = expiry{}
		pk.qhead++
		pi := pk.peers[e.peer]
		if pi == nil {
			continue
		}
		at, ok := pi.outstanding[e.block]
		if !ok || !at.Equal(e.at) {
			continue
		}
		delete(pi.outstanding, e.block)
		j, _ := pk.blockIndex(e.block)
		pk.dropOwner(int(e.block.Index), j, e.peer, true)
		touched[int(e.block.Index)] = true
		pi.cooldown = now.Add(pk.timeout)
		for i := range pk.exclusiveOf(e.peer) {
			pk.pieces[i].exHeld = false
		}
		out = append(out, Expired{Peer: e.peer, Block: e.block})
	}
	for i := range touched {
		pk.settle(i)
	}
	if pk.qhead > 1024 && pk.qhead*2 > len(pk.queue) {
		pk.queue = append(pk.queue[:0], pk.queue[pk.qhead:]...)
		pk.qhead = 0
	}
	return out
}

// Received records a block from p.
func (pk *picker) Received(p PeerKey, b Block) Receipt {
	pi := pk.peers[p]
	j, ok := pk.blockIndex(b)
	if pi == nil || !ok {
		return Receipt{}
	}
	i := int(b.Index)
	ps := &pk.pieces[i]
	if ps.have || ps.pending || ps.blocks == nil {
		return Receipt{}
	}
	bi := &ps.blocks[j]
	if bi.received {
		return Receipt{}
	}
	isOwner := false
	for _, o := range bi.owners {
		if o.peer == p {
			isOwner = true
		}
	}
	if !isOwner && !containsKey(bi.released, p) {
		return Receipt{}
	}
	var r Receipt
	r.Accepted = true
	if len(bi.owners) == 0 {
		pk.addNeeded(i, -1) // it was needed again; now it is received
	} else {
		ps.nBusy--
	}
	for _, o := range bi.owners {
		if o.peer != p {
			r.Cancel = append(r.Cancel, o.peer)
		}
		if op := pk.peers[o.peer]; op != nil {
			delete(op.outstanding, b)
		}
	}
	bi.owners = nil
	bi.released = nil
	bi.received = true
	ps.nRecv++
	if !containsAddr(ps.contrib, pi.origin) {
		ps.contrib = append(ps.contrib, pi.origin)
	}
	pi.owned.add(i)
	pi.cooldown = time.Time{}
	if ps.nRecv == pk.l.NumBlocks(i) {
		ps.pending = true
		r.PieceDone = true
	}
	pk.settle(i)
	return r
}

// SetPriority sets the priority class of a piece.
func (pk *picker) SetPriority(index int, prio Priority) {
	if index < 0 || index >= pk.l.NumPieces || prio < PrioritySkip || prio > PriorityHigh {
		return
	}
	ps := &pk.pieces[index]
	if ps.prio == prio {
		return
	}
	wasFresh, wasActive := ps.inFresh, ps.inActive
	pk.removeFresh(index)
	pk.removeActive(index)
	before := pk.counts(index)
	pk.addNeeded(index, -pk.needOf(index))
	if wanted(ps.prio) && !ps.have {
		pk.wantedLeft--
	}
	ps.prio = prio
	pk.addNeeded(index, pk.needOf(index))
	if wanted(ps.prio) && !ps.have {
		pk.wantedLeft++
	}
	if after := pk.counts(index); after != before {
		d := 1
		if !after {
			d = -1
		}
		for _, pi := range pk.peers {
			if pi.bf.Has(index) {
				pi.interest += d
			}
		}
	}
	if wasFresh {
		pk.addFresh(index)
	}
	if wasActive {
		pk.addActive(index)
	}
}

// resetBlocks forgets every block of piece i (outstanding ones are removed
// from their peers' sets) and clears its counters.
func (pk *picker) resetBlocks(i int) {
	ps := &pk.pieces[i]
	for j := range ps.blocks {
		for _, o := range ps.blocks[j].owners {
			if op := pk.peers[o.peer]; op != nil {
				delete(op.outstanding, pk.block(i, j))
			}
		}
	}
	ps.blocks = nil
	ps.nBusy, ps.nRecv = 0, 0
	ps.exclusive, ps.exHeld = false, false
	ps.contrib = nil
}

// PieceVerified marks a piece as have.
func (pk *picker) PieceVerified(index int) {
	if index < 0 || index >= pk.l.NumPieces {
		return
	}
	ps := &pk.pieces[index]
	if ps.have {
		return
	}
	pk.addNeeded(index, -pk.needOf(index))
	if wanted(ps.prio) {
		pk.wantedLeft--
		for _, pi := range pk.peers {
			if pi.bf.Has(index) {
				pi.interest--
			}
		}
	}
	pk.resetBlocks(index)
	ps.have, ps.pending = true, false
	pk.have.Set(index)
	pk.haveN++
	pk.settle(index)
}

// PieceFailed makes a pending piece needed again and returns its
// contributors.
func (pk *picker) PieceFailed(index int) []netip.Addr {
	if index < 0 || index >= pk.l.NumPieces || !pk.pieces[index].pending {
		return nil
	}
	ps := &pk.pieces[index]
	contrib := ps.contrib
	pk.resetBlocks(index)
	ps.pending = false
	ps.exclusive = true
	pk.addNeeded(index, pk.needOf(index))
	for _, pi := range pk.peers {
		pi.owned.remove(index)
	}
	pk.settle(index)
	return contrib
}

// Have returns a copy of the verified pieces.
func (pk *picker) Have() *Bitfield { return pk.have.Clone() }

// Done reports whether every wanted piece is have.
func (pk *picker) Done() bool { return pk.wantedLeft == 0 }

// Endgame reports whether every remaining block has been requested.
func (pk *picker) Endgame() bool { return pk.needed == 0 && pk.wantedLeft > 0 }

func containsKey(s []PeerKey, p PeerKey) bool {
	for _, x := range s {
		if x == p {
			return true
		}
	}
	return false
}

func containsAddr(s []netip.Addr, a netip.Addr) bool {
	for _, x := range s {
		if x == a {
			return true
		}
	}
	return false
}

// idxSet is a set of ints with O(1) add, remove and random access.
type idxSet struct {
	items []int
	pos   map[int]int
}

func (s *idxSet) add(i int) {
	if s.pos == nil {
		s.pos = map[int]int{}
	}
	if _, ok := s.pos[i]; ok {
		return
	}
	s.pos[i] = len(s.items)
	s.items = append(s.items, i)
}

func (s *idxSet) remove(i int) {
	k, ok := s.pos[i]
	if !ok {
		return
	}
	last := len(s.items) - 1
	s.items[k] = s.items[last]
	s.pos[s.items[k]] = k
	s.items = s.items[:last]
	delete(s.pos, i)
}

func (s *idxSet) has(i int) bool {
	_, ok := s.pos[i]
	return ok
}
