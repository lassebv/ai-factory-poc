package piece

import (
	"errors"
	"testing"
)

func TestBitfieldNilAndZero(t *testing.T) {
	var nb *Bitfield
	if nb.Len() != 0 || nb.Has(0) || nb.Count() != 0 || !nb.Full() || len(nb.Bytes()) != 0 || nb.Clone().Len() != 0 {
		t.Fatal("nil bitfield should behave as empty")
	}
	nb.Set(0)
	nb.Clear(0)
	var z Bitfield
	z.Set(0)
	if z.Has(0) || !z.Full() {
		t.Fatal("zero-value bitfield should be empty and full")
	}
	if NewBitfield(-3).Len() != 0 {
		t.Fatal("negative length should give 0")
	}
	if _, err := FromBytes(nil, -1); !errors.Is(err, ErrInvalidBitfield) {
		t.Fatal("negative n must be rejected")
	}
	if b, err := FromBytes(nil, 0); err != nil || b.Len() != 0 {
		t.Fatal("empty bitfield of 0 pieces is valid")
	}
}

func TestBitfieldWireOrder(t *testing.T) {
	b := NewBitfield(16)
	for _, i := range []int{0, 7, 8, 15} {
		b.Set(i)
	}
	got := b.Bytes()
	if got[0] != 0x81 || got[1] != 0x81 {
		t.Fatalf("Bytes = %08b", got)
	}
	r, err := FromBytes(got, 16)
	if err != nil || r.Count() != 4 || !r.Has(15) {
		t.Fatalf("round trip: %v %v", r, err)
	}
}

func TestLayoutEdges(t *testing.T) {
	bad := Layout{NumPieces: 2, PieceLength: 100, TotalLength: 50}
	if bad.PieceSize(1) != 0 || bad.NumBlocks(1) != 0 {
		t.Fatal("inconsistent layout must not yield a negative size")
	}
	l := Layout{NumPieces: 1, PieceLength: 40000, TotalLength: 40000}
	if l.blockLen(0, 2) != 40000-2*BlockSize || l.blockLen(0, 0) != BlockSize {
		t.Fatal("blockLen")
	}
	if pipelineDepth(16384*125) != 250 || pipelineDepth(16384*125+8191) != 250 || pipelineDepth(16384*4+8192) != 9 {
		t.Fatal("pipelineDepth rounding")
	}
	if pipelineDepth(1<<62) != MaxPipelineDepth {
		t.Fatal("pipelineDepth must not overflow")
	}
}

func TestPickerZeroPiecesAndBadArgs(t *testing.T) {
	pk := NewPicker(Layout{}, nil)
	if !pk.Done() || pk.Pick(1, 5) != nil {
		t.Fatal("empty torrent is done")
	}
	pk = NewPicker(Layout{NumPieces: 2, PieceLength: BlockSize, TotalLength: 2 * BlockSize}, nil)
	pk.PeerBitfield(1, nil)
	pk.PeerBitfield(1, full(2))
	if pk.Pick(1, 0) != nil || pk.Pick(1, -1) != nil || pk.Pick(99, 5) != nil {
		t.Fatal("max <= 0 or unknown peer must give nil")
	}
	pk.Release(99)
	if pk.Outstanding(99) != 0 {
		t.Fatal("unknown peer has nothing outstanding")
	}
	pk.PieceFailed(-1)
	pk.PieceFailed(0) // not pending: ignored
	pk.PieceVerified(-1)
	pk.PeerHave(1, -1)
	pk.PeerHave(1, 0) // already has: no availability change
	if got := pk.(*picker).pieces[0].avail; got != 1 {
		t.Fatalf("avail = %d, want 1", got)
	}
}

func TestPickerBitfieldReplaceUpdatesAvailability(t *testing.T) {
	l := Layout{NumPieces: 3, PieceLength: BlockSize, TotalLength: 3 * BlockSize}
	pk := NewPicker(l, nil).(*picker)
	pk.PeerBitfield(1, full(3))
	only2 := NewBitfield(3)
	only2.Set(2)
	pk.PeerBitfield(1, only2)
	if pk.pieces[0].avail != 0 || pk.pieces[2].avail != 1 {
		t.Fatalf("avail = %d %d", pk.pieces[0].avail, pk.pieces[2].avail)
	}
	only2.Set(0) // the picker holds its own copy
	if bs := pk.Pick(1, 5); len(bs) != 1 || bs[0].Index != 2 {
		t.Fatalf("Pick = %v", bs)
	}
	pk.RemovePeer(1)
	for i := range pk.pieces {
		if pk.pieces[i].avail != 0 {
			t.Fatalf("avail[%d] = %d after RemovePeer", i, pk.pieces[i].avail)
		}
	}
}

func TestPickerReleaseKeepsPartialPieceInProgress(t *testing.T) {
	l := Layout{NumPieces: 3, PieceLength: 2 * BlockSize, TotalLength: 6 * BlockSize}
	pk := NewPicker(l, nil)
	only2 := NewBitfield(3)
	only2.Set(2)
	pk.PeerBitfield(1, full(3))
	pk.PeerBitfield(2, full(3))
	pk.PeerBitfield(3, only2)
	// Piece 2 is the most common; peer 1 starts it by being given only it.
	bs := pk.Pick(3, 2)
	if len(bs) != 2 || bs[0].Index != 2 {
		t.Fatalf("Pick = %v", bs)
	}
	if ok, _ := recvM3(pk, 3, bs[0]); !ok {
		t.Fatal("not accepted")
	}
	pk.Release(3)
	// Piece 2 has a received block, so it is in progress and goes first even
	// though it is the most available.
	got := pk.Pick(1, 1)
	if len(got) != 1 || got[0] != bs[1] {
		t.Fatalf("Pick = %v, want %v", got, bs[1])
	}
	// After another release, piece 2 still has a received block and stays
	// first for any peer.
	pk.Release(1)
	if x := pk.Pick(2, 1); len(x) != 1 || x[0] != bs[1] {
		t.Fatalf("in-progress piece 2 first, got %v", x)
	}
	pk.Release(2)
	first := pk.Pick(1, 1)
	pk.Release(1)
	if first[0] != bs[1] {
		t.Fatalf("got %v", first)
	}
	// A piece that was only outstanding and then released counts as new
	// again and is ranked by availability.
	p0 := NewPicker(l, nil)
	p0.PeerBitfield(1, full(3))
	p0.PeerBitfield(2, only2)
	p0.Pick(2, 2) // piece 2 outstanding only
	p0.Release(2)
	if got := p0.Pick(1, 1); got[0].Index != 0 {
		t.Fatalf("released piece 2 (avail 2) must rank as new, got %v", got)
	}
}

func TestPickerVerifiedClearsOutstanding(t *testing.T) {
	l := Layout{NumPieces: 2, PieceLength: 2 * BlockSize, TotalLength: 4 * BlockSize}
	pk := NewPicker(l, nil)
	pk.PeerBitfield(1, full(2))
	pk.Pick(1, 4)
	pk.PieceVerified(0)
	if pk.Outstanding(1) != 2 {
		t.Fatalf("Outstanding = %d, want 2", pk.Outstanding(1))
	}
	pk.PieceVerified(0) // idempotent
	if !pk.Have().Has(0) || pk.Have().Count() != 1 {
		t.Fatal("Have")
	}
}

func TestPickerReceivedUnrequestedStartsPiece(t *testing.T) {
	l := Layout{NumPieces: 3, PieceLength: 2 * BlockSize, TotalLength: 6 * BlockSize}
	pk := NewPicker(l, nil)
	pk.PeerBitfield(1, full(3))
	if ok, done := recvM3(pk, 1, Block{2, BlockSize, BlockSize}); !ok || done {
		t.Fatal("unrequested block of a needed piece is accepted")
	}
	// Piece 2 is now in progress, so its remaining block comes first.
	if bs := pk.Pick(1, 1); len(bs) != 1 || bs[0] != (Block{2, 0, BlockSize}) {
		t.Fatalf("Pick = %v", bs)
	}
	// A block owned by another peer is accepted from anyone and leaves the
	// owner's outstanding set.
	if ok, done := recvM3(pk, 5, Block{2, 0, BlockSize}); !ok || !done {
		t.Fatal("completing block")
	}
	if pk.Outstanding(1) != 0 {
		t.Fatalf("Outstanding = %d", pk.Outstanding(1))
	}
	// Owner already gone when its block arrives from elsewhere.
	bs := pk.Pick(1, 1)
	delete(pk.(*picker).peers, 1)
	if ok, _ := recvM3(pk, 2, bs[0]); !ok {
		t.Fatal("accepted")
	}
}

func TestPickerDrainManyPeers(t *testing.T) {
	l := Layout{NumPieces: 9, PieceLength: 3*BlockSize + 7, TotalLength: 8*(3*BlockSize+7) + 1}
	pk := NewPicker(l, nil)
	for p := PeerKey(1); p <= 3; p++ {
		pk.PeerBitfield(p, full(9))
	}
	var total int64
	seen := map[Block]bool{}
	for round := 0; !pk.Done(); round++ {
		if round > 10000 {
			t.Fatal("no progress")
		}
		for p := PeerKey(1); p <= 3; p++ {
			for _, b := range pk.Pick(p, 2) {
				if seen[b] {
					t.Fatalf("duplicate %v", b)
				}
				ok, done := recvM3(pk, p, b)
				if !ok {
					t.Fatalf("rejected %v", b)
				}
				seen[b] = true
				total += int64(b.Length)
				if done {
					if b.Index == 4 && !seen[Block{99, 0, 0}] {
						// Fail piece 4 once; its blocks must come back.
						seen[Block{99, 0, 0}] = true
						for k := range seen {
							if k.Index == 4 {
								delete(seen, k)
								total -= int64(k.Length)
							}
						}
						pk.PieceFailed(4)
						continue
					}
					pk.PieceVerified(int(b.Index))
				}
			}
		}
	}
	if total != l.TotalLength {
		t.Fatalf("total %d, want %d", total, l.TotalLength)
	}
}

// BenchmarkPickFull250 models the gate's 4 MiB / 256 KiB torrent with a
// full pipeline being topped up one block at a time.
func BenchmarkPickFull250(b *testing.B) {
	l := Layout{NumPieces: 16, PieceLength: 256 << 10, TotalLength: 4 << 20}
	for b.Loop() {
		pk := NewPicker(l, nil)
		pk.PeerBitfield(1, full(16))
		out := pk.Pick(1, 250)
		for len(out) > 0 {
			blk := out[0]
			out = out[1:]
			if _, done := recvM3(pk, 1, blk); done {
				pk.PieceVerified(int(blk.Index))
			}
			out = append(out, pk.Pick(1, 1)...)
		}
	}
}

// recvM3 adapts the M3 two-value Received to the M4 Receipt (mechanical).
func recvM3(pk Picker, p PeerKey, b Block) (bool, bool) {
	r := pk.Received(p, b)
	return r.Accepted, r.PieceDone
}
