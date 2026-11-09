package piece

// FROZEN CONTRACT TESTS (ADR-0011, ADR-0018). Owned by the architect. Every correct
// implementation of contract.go must pass these.

import (
	"bytes"
	"errors"
	"math/rand/v2"
	"net/netip"
	"slices"
	"testing"
	"time"

	"torrentd/internal/clock"
)

func TestContractBitfieldBasics(t *testing.T) {
	b := NewBitfield(10)
	if b.Len() != 10 || b.Count() != 0 || b.Full() {
		t.Fatalf("new bitfield: len=%d count=%d full=%v", b.Len(), b.Count(), b.Full())
	}
	b.Set(0)
	b.Set(9)
	b.Set(9)
	b.Set(-1) // out of range: no-op, no panic
	b.Set(10)
	if !b.Has(0) || !b.Has(9) || b.Has(1) || b.Has(10) || b.Has(-1) || b.Count() != 2 {
		t.Fatalf("after Set: count=%d bytes=%08b", b.Count(), b.Bytes())
	}
	if got := b.Bytes(); !bytes.Equal(got, []byte{0x80, 0x40}) {
		t.Fatalf("Bytes = %08b, want [10000000 01000000]", got)
	}
	c := b.Clone()
	b.Clear(0)
	b.Clear(99)
	if b.Has(0) || !c.Has(0) {
		t.Fatal("Clone must be independent of the original")
	}
	out := b.Bytes()
	out[0] = 0xff
	if b.Has(0) {
		t.Fatal("Bytes must return a copy")
	}
	for i := 0; i < 10; i++ {
		b.Set(i)
	}
	if !b.Full() || b.Count() != 10 {
		t.Fatalf("all set: full=%v count=%d", b.Full(), b.Count())
	}
	if z := NewBitfield(0); !z.Full() || z.Len() != 0 || len(z.Bytes()) != 0 {
		t.Fatal("a zero-length bitfield is full and empty")
	}
}

func TestContractBitfieldFromBytes(t *testing.T) {
	b, err := FromBytes([]byte{0xa0, 0x80}, 9)
	if err != nil {
		t.Fatal(err)
	}
	if !b.Has(0) || b.Has(1) || !b.Has(2) || !b.Has(8) || b.Count() != 3 {
		t.Fatalf("FromBytes decoded wrongly: %08b", b.Bytes())
	}
	src := []byte{0xff}
	b2, _ := FromBytes(src, 8)
	src[0] = 0
	if !b2.Full() {
		t.Fatal("FromBytes must copy its input")
	}
	for _, tc := range []struct {
		b []byte
		n int
	}{
		{[]byte{0xff}, 9},       // too short
		{[]byte{0xff, 0}, 8},    // too long
		{[]byte{0xff, 0x40}, 9}, // spare bit set
		{[]byte{0x01}, 7},       // spare bit set
	} {
		if _, err := FromBytes(tc.b, tc.n); !errors.Is(err, ErrInvalidBitfield) {
			t.Errorf("FromBytes(%08b, %d) = %v, want ErrInvalidBitfield", tc.b, tc.n, err)
		}
	}
}

func TestContractLayout(t *testing.T) {
	l := Layout{NumPieces: 3, PieceLength: 32768, TotalLength: 32768*2 + 100}
	if l.PieceSize(0) != 32768 || l.PieceSize(2) != 100 || l.PieceSize(3) != 0 || l.PieceSize(-1) != 0 {
		t.Fatalf("PieceSize: %d %d %d", l.PieceSize(0), l.PieceSize(2), l.PieceSize(3))
	}
	if l.NumBlocks(0) != 2 || l.NumBlocks(2) != 1 || l.NumBlocks(5) != 0 {
		t.Fatalf("NumBlocks: %d %d", l.NumBlocks(0), l.NumBlocks(2))
	}
	odd := Layout{NumPieces: 1, PieceLength: 40000, TotalLength: 40000}
	if odd.NumBlocks(0) != 3 {
		t.Fatalf("NumBlocks for a 40000-byte piece = %d, want 3", odd.NumBlocks(0))
	}
}

func TestContractPipelineDepth(t *testing.T) {
	for _, tc := range []struct {
		rate int64
		want int
	}{
		{-5, 5}, {0, 5}, {16384, 5}, {16384 * 3, 6}, {16384 * 50, 100}, {1 << 30, 250},
	} {
		if got := PipelineDepth(tc.rate); got != tc.want {
			t.Errorf("PipelineDepth(%d) = %d, want %d", tc.rate, got, tc.want)
		}
	}
}

func full(n int) *Bitfield {
	b := NewBitfield(n)
	for i := 0; i < n; i++ {
		b.Set(i)
	}
	return b
}

func ip(n byte) netip.Addr { return netip.AddrFrom4([4]byte{10, 0, 0, n}) }

var t0 = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// newPK returns a deterministic picker on a fake clock.
func newPK(l Layout, seed uint64) (Picker, *clock.Fake) {
	clk := clock.NewFake(t0)
	return New(l, Options{Rand: rand.New(rand.NewPCG(seed, 7)), Clock: clk}), clk
}

// lay returns a layout of n pieces of blocks*BlockSize bytes.
func lay(n, blocks int) Layout {
	return Layout{NumPieces: n, PieceLength: int64(blocks * BlockSize), TotalLength: int64(n * blocks * BlockSize)}
}

// drain picks and receives every block from p until nothing is left,
// verifying each completed piece, and returns the blocks in pick order.
func drain(t *testing.T, pk Picker, p PeerKey, batch int) []Block {
	t.Helper()
	var all []Block
	for i := 0; i < 1<<20; i++ {
		bs := pk.Pick(p, batch)
		if len(bs) == 0 {
			return all
		}
		if len(bs) > batch {
			t.Fatalf("Pick(%d) returned %d blocks", batch, len(bs))
		}
		for _, b := range bs {
			all = append(all, b)
			r := pk.Received(p, b)
			if !r.Accepted {
				t.Fatalf("Received(%+v) from the peer it was requested from was not accepted", b)
			}
			if r.PieceDone {
				pk.PieceVerified(int(b.Index))
			}
		}
	}
	t.Fatal("picker never ran out of blocks")
	return nil
}

func TestContractPickerCoversEveryBlockExactlyOnce(t *testing.T) {
	// 10 pieces of 2.5 blocks, the last one short.
	l := Layout{NumPieces: 10, PieceLength: 40960, TotalLength: 9*40960 + 20000}
	pk, _ := newPK(l, 1)
	pk.AddPeer(1, ip(1))
	pk.PeerBitfield(1, full(10))
	seen := map[Block]bool{}
	for _, b := range drain(t, pk, 1, 7) {
		if seen[b] {
			t.Fatalf("block %+v handed out twice to the same peer", b)
		}
		seen[b] = true
		if b.Begin%BlockSize != 0 || int64(b.Begin)+int64(b.Length) > l.PieceSize(int(b.Index)) {
			t.Fatalf("block %+v is not a layout block", b)
		}
	}
	want := 0
	for i := 0; i < 10; i++ {
		want += l.NumBlocks(i)
	}
	if len(seen) != want || !pk.Done() || pk.Have().Count() != 10 {
		t.Fatalf("got %d blocks (want %d), done=%v", len(seen), want, pk.Done())
	}
}

func TestContractPickerRespectsAvailability(t *testing.T) {
	pk, _ := newPK(lay(8, 2), 1)
	pk.AddPeer(1, ip(1))
	if pk.Pick(1, 10) != nil || pk.Interesting(1) {
		t.Fatal("a peer with no pieces must get nothing and not be interesting")
	}
	pk.PeerHave(1, 5)
	pk.PeerHave(1, 99) // ignored
	if !pk.Interesting(1) {
		t.Fatal("peer with piece 5 must be interesting")
	}
	for _, b := range pk.Pick(1, 10) {
		if b.Index != 5 {
			t.Fatalf("picked %+v from a peer that only has piece 5", b)
		}
	}
	pk.PeerBitfield(1, NewBitfield(7)) // wrong length: ignored
	if !pk.Interesting(1) {
		t.Fatal("a bitfield of the wrong length must be ignored")
	}
	pk.PeerBitfield(2, full(8)) // unregistered: ignored
	if pk.Pick(2, 10) != nil {
		t.Fatal("unregistered peers get nothing")
	}
	pk.AddPeer(2, ip(2))
	pk.PeerBitfield(2, full(8))
	if !pk.IsSeed(2) || pk.IsSeed(1) {
		t.Fatal("IsSeed wrong")
	}
	for i := 0; i < 8; i++ {
		pk.PieceVerified(i)
	}
	if pk.Interesting(2) || !pk.Done() {
		t.Fatal("nothing is interesting once every piece is have")
	}
}

func TestContractPickerRarestFirstAfterRandomFirst(t *testing.T) {
	// 48 one-block pieces: A has all, B and C only 0-23 (PICK-01 shape).
	l := lay(48, 1)
	pk, _ := newPK(l, 3)
	common := NewBitfield(48)
	for i := 0; i < 24; i++ {
		common.Set(i)
	}
	pk.AddPeer(1, ip(1))
	pk.PeerBitfield(1, full(48))
	for _, p := range []PeerKey{2, 3} {
		pk.AddPeer(p, ip(byte(p)))
		pk.PeerBitfield(p, common)
	}
	for i := 0; i < RandomFirstPieces; i++ {
		pk.PieceVerified(i)
	}
	got := pk.Pick(1, 16)
	if len(got) != 16 {
		t.Fatalf("picked %d blocks, want 16", len(got))
	}
	for _, b := range got {
		if b.Index < 24 {
			t.Fatalf("picked common piece %d from the only holder of the rare pieces: %v", b.Index, got)
		}
	}
	for _, b := range pk.Pick(2, 50) {
		if b.Index >= 24 {
			t.Fatalf("peer 2 asked for piece %d it does not have", b.Index)
		}
	}
}

func TestContractPickerRandomFirstUsesRand(t *testing.T) {
	first := map[uint32]bool{}
	for seed := uint64(0); seed < 16; seed++ {
		pk, _ := newPK(lay(64, 1), seed)
		pk.AddPeer(1, ip(1))
		pk.PeerBitfield(1, full(64))
		first[pk.Pick(1, 1)[0].Index] = true
	}
	if len(first) < 4 {
		t.Fatalf("with no piece verified the first piece must be random; 16 seeds gave only %v", first)
	}
	// Same seed, same choices (T2).
	a, _ := newPK(lay(64, 1), 9)
	b, _ := newPK(lay(64, 1), 9)
	for _, pk := range []Picker{a, b} {
		pk.AddPeer(1, ip(1))
		pk.PeerBitfield(1, full(64))
	}
	if !slices.Equal(a.Pick(1, 8), b.Pick(1, 8)) {
		t.Fatal("the picker must be deterministic for a given Rand")
	}
}

func TestContractPickerPriorities(t *testing.T) {
	pk, _ := newPK(lay(10, 1), 1)
	pk.AddPeer(1, ip(1))
	pk.PeerBitfield(1, full(10))
	pk.SetPriority(7, PriorityHigh)
	pk.SetPriority(3, PrioritySkip)
	pk.SetPriority(0, PriorityLow)
	pk.SetPriority(99, PriorityHigh) // ignored
	got := pk.Pick(1, 10)
	if len(got) != 9 || got[0].Index != 7 || got[8].Index != 0 {
		t.Fatalf("pick order %v: want high piece 7 first, low piece 0 last, skip piece 3 absent", got)
	}
	for _, b := range got {
		if b.Index == 3 {
			t.Fatal("a skip piece was requested")
		}
		pk.Received(1, b)
		pk.PieceVerified(int(b.Index))
	}
	if !pk.Done() || pk.Have().Has(3) {
		t.Fatal("Done must ignore skip pieces")
	}
}

func TestContractPickerOwnership(t *testing.T) {
	pk, _ := newPK(lay(4, 2), 1)
	for _, p := range []PeerKey{1, 2} {
		pk.AddPeer(p, ip(byte(p)))
		pk.PeerBitfield(p, full(4))
	}
	bs := pk.Pick(1, 1)
	if len(bs) != 1 {
		t.Fatal("no block picked")
	}
	b := bs[0]
	other := Block{Index: (b.Index + 1) % 4, Begin: 0, Length: BlockSize}
	if r := pk.Received(1, other); r.Accepted {
		t.Fatal("an unrequested block was accepted (POISON-01)")
	}
	if r := pk.Received(2, b); r.Accepted {
		t.Fatal("a block requested from peer 1 was accepted from peer 2")
	}
	if r := pk.Received(1, Block{Index: b.Index, Begin: b.Begin, Length: b.Length - 1}); r.Accepted {
		t.Fatal("a block of the wrong length was accepted")
	}
	if r := pk.Received(1, b); !r.Accepted || r.PieceDone {
		t.Fatalf("requested block: %+v", r)
	}
	if r := pk.Received(1, b); r.Accepted {
		t.Fatal("a duplicate block was accepted")
	}
	// Late answer after Release is still accepted.
	b2 := pk.Pick(1, 1)
	pk.Release(1)
	if pk.Outstanding(1) != 0 {
		t.Fatal("Release must take back every outstanding block")
	}
	if r := pk.Received(1, b2[0]); !r.Accepted {
		t.Fatal("a late block after Release, still needed, must be accepted")
	}
}

func TestContractPickerPieceAffinity(t *testing.T) {
	pk, _ := newPK(lay(8, 4), 1)
	for _, p := range []PeerKey{1, 2} {
		pk.AddPeer(p, ip(byte(p)))
		pk.PeerBitfield(p, full(8))
	}
	a := pk.Pick(1, 2)
	bs := pk.Pick(2, 4)
	for _, b := range bs {
		if b.Index == a[0].Index {
			t.Fatalf("peer 2 got blocks of piece %d that peer 1 started while fresh pieces exist", b.Index)
		}
	}
	more := pk.Pick(1, 2)
	if len(more) != 2 || more[0].Index != a[0].Index {
		t.Fatalf("peer 1 must finish its own piece %d first, got %v", a[0].Index, more)
	}
}

func TestContractPickerEndgame(t *testing.T) {
	pk, _ := newPK(lay(4, 1), 1)
	for _, p := range []PeerKey{1, 2, 3, 4, 5} {
		pk.AddPeer(p, ip(byte(p)))
		pk.PeerBitfield(p, full(4))
	}
	if pk.Endgame() {
		t.Fatal("endgame before anything was requested")
	}
	held := pk.Pick(1, 10)
	if len(held) != 4 || !pk.Endgame() {
		t.Fatalf("peer 1 holds %d blocks, endgame=%v; want 4, true", len(held), pk.Endgame())
	}
	dup := pk.Pick(2, 10)
	if len(dup) != 4 {
		t.Fatalf("endgame must hand the remaining blocks to another peer, got %v", dup)
	}
	if again := pk.Pick(2, 10); len(again) != 0 {
		t.Fatalf("a block must never be outstanding twice for the same peer: %v", again)
	}
	pk.Pick(3, 10)
	if extra := pk.Pick(4, 10); len(extra) != 0 {
		t.Fatalf("blocks outstanding for more than EndgameMaxOwners=%d peers: %v", EndgameMaxOwners, extra)
	}
	r := pk.Received(2, dup[0])
	if !r.Accepted || !r.PieceDone {
		t.Fatalf("receipt %+v", r)
	}
	slices.Sort(r.Cancel)
	if !slices.Equal(r.Cancel, []PeerKey{1, 3}) {
		t.Fatalf("Cancel = %v, want the other owners [1 3]", r.Cancel)
	}
	if pk.Outstanding(1) != 3 || pk.Outstanding(3) != 3 {
		t.Fatal("the cancelled block must leave the other peers' outstanding sets")
	}
	if r := pk.Received(1, dup[0]); r.Accepted {
		t.Fatal("the losing endgame duplicate must be rejected (wasted)")
	}
}

func TestContractPickerExpire(t *testing.T) {
	pk, clk := newPK(lay(8, 1), 1)
	for _, p := range []PeerKey{1, 2} {
		pk.AddPeer(p, ip(byte(p)))
		pk.PeerBitfield(p, full(8))
	}
	held := pk.Pick(1, 4)
	clk.Advance(DefaultRequestTimeout - time.Second)
	if e := pk.Expire(); len(e) != 0 {
		t.Fatalf("expired too early: %v", e)
	}
	clk.Advance(time.Second)
	e := pk.Expire()
	if len(e) != 4 || pk.Outstanding(1) != 0 {
		t.Fatalf("Expire returned %v, outstanding %d; want the 4 blocks of peer 1", e, pk.Outstanding(1))
	}
	for _, x := range e {
		if x.Peer != 1 {
			t.Fatalf("expired %+v", x)
		}
	}
	if bs := pk.Pick(1, 10); len(bs) != 0 {
		t.Fatalf("a peer that just stalled must not get new blocks: %v", bs)
	}
	got := pk.Pick(2, 10)
	if len(got) != 8 {
		t.Fatalf("the expired blocks must be requestable from another peer: %v", got)
	}
	// Peer 1 answers late: accepted, and peer 2 is told to cancel.
	r := pk.Received(1, held[0])
	if !r.Accepted || !slices.Equal(r.Cancel, []PeerKey{2}) {
		t.Fatalf("late answer: %+v", r)
	}
}

func TestContractPickerRemovePeerFreesBlocks(t *testing.T) {
	pk, _ := newPK(lay(2, 2), 1)
	for _, p := range []PeerKey{1, 2} {
		pk.AddPeer(p, ip(byte(p)))
		pk.PeerBitfield(p, full(2))
	}
	pk.Pick(1, 4)
	pk.RemovePeer(1)
	if got := pk.Pick(2, 10); len(got) != 4 {
		t.Fatalf("blocks of a removed peer must be needed again, got %v", got)
	}
	if pk.Pick(1, 1) != nil || pk.Outstanding(1) != 0 {
		t.Fatal("a removed peer gets nothing")
	}
}

func TestContractPickerFailureContributors(t *testing.T) {
	pk, _ := newPK(lay(1, 2), 1)
	pk.AddPeer(1, ip(1))
	pk.AddPeer(2, ip(2))
	pk.AddPeer(3, ip(1)) // second connection from the same IP
	for _, p := range []PeerKey{1, 2, 3} {
		pk.PeerBitfield(p, full(1))
	}
	a := pk.Pick(1, 1)
	b := pk.Pick(2, 1) // rule 4: the only piece is started by peer 1
	if len(a) != 1 || len(b) != 1 || a[0] == b[0] {
		t.Fatalf("picks %v %v", a, b)
	}
	pk.Received(1, a[0])
	pk.RemovePeer(1) // contributions survive disconnect
	if r := pk.Received(2, b[0]); !r.PieceDone {
		t.Fatal("piece not done")
	}
	got := pk.PieceFailed(0)
	slices.SortFunc(got, func(x, y netip.Addr) int { return x.Compare(y) })
	if !slices.Equal(got, []netip.Addr{ip(1), ip(2)}) {
		t.Fatalf("contributors = %v, want [10.0.0.1 10.0.0.2]", got)
	}
	// Single-source retry: the first picker takes the whole piece.
	if bs := pk.Pick(3, 10); len(bs) != 2 {
		t.Fatalf("retry pick %v, want both blocks", bs)
	}
	if bs := pk.Pick(2, 10); len(bs) != 0 {
		t.Fatalf("an exclusive (failed) piece was shared with a second peer: %v", bs)
	}
	if again := pk.PieceFailed(0); again != nil {
		t.Fatal("PieceFailed on a piece that is not pending must be ignored")
	}
}

func TestContractPickerPieceVerifiedUpfront(t *testing.T) {
	pk, _ := newPK(lay(4, 2), 1)
	pk.PieceVerified(1)
	pk.PieceVerified(3)
	pk.AddPeer(1, ip(1))
	pk.PeerBitfield(1, full(4))
	for _, b := range drain(t, pk, 1, 3) {
		if b.Index == 1 || b.Index == 3 {
			t.Fatalf("requested verified piece %d", b.Index)
		}
	}
	if !pk.Done() {
		t.Fatal("not done")
	}
}

// TestContractPickerCost guards against per-call scans of every piece
// (CPU-01): downloading 8x more pieces must cost well under 64x more.
func TestContractPickerCost(t *testing.T) {
	run := func(n int) time.Duration {
		best := time.Duration(1 << 62)
		for rep := 0; rep < 3; rep++ {
			pk, _ := newPK(lay(n, 1), 1)
			for p := PeerKey(1); p <= 4; p++ {
				pk.AddPeer(p, ip(byte(p)))
				pk.PeerBitfield(p, full(n))
			}
			start := time.Now()
			for left := n; left > 0; {
				for p := PeerKey(1); p <= 4; p++ {
					for _, b := range pk.Pick(p, 16) {
						if pk.Received(p, b).PieceDone {
							pk.PieceVerified(int(b.Index))
							left--
						}
					}
				}
			}
			best = min(best, time.Since(start))
		}
		return best
	}
	small, large := run(2048), run(16384)
	t.Logf("2048 pieces: %v, 16384 pieces: %v", small, large)
	if large > 24*small+50*time.Millisecond {
		t.Errorf("8x the pieces took %.1fx the time; the picker scans every piece per call", float64(large)/float64(small))
	}
}
