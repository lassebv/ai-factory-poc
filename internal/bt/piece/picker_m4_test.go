package piece

import (
	"math/rand/v2"
	"testing"
)

func TestPickerReleaseKeepsPartialPieceInProgress(t *testing.T) {
	l := Layout{NumPieces: 2, PieceLength: 2 * BlockSize, TotalLength: 4 * BlockSize}
	pk := NewPicker(l, nil)
	pk.PeerBitfield(1, full(2))
	pk.PeerBitfield(2, full(2))
	bs := pk.Pick(1, 2)
	if ok, _, _ := pk.Received(1, bs[0]); !ok {
		t.Fatal("first block rejected")
	}
	rel := pk.Release(1)
	if len(rel) != 1 || rel[0] != bs[1] {
		t.Fatalf("Release = %v, want %v", rel, bs[1:])
	}
	// Peer 2 finishes the partial piece 0 first (affinity: nobody else works on it).
	got := pk.Pick(2, 1)
	if len(got) != 1 || got[0] != bs[1] {
		t.Fatalf("Pick = %v, want the released block %v", got, bs[1])
	}
	if pk.Release(42) != nil {
		t.Fatal("unknown peer releases nothing")
	}
}

func TestPickerAffinityAvoidsMixingPieces(t *testing.T) {
	l := Layout{NumPieces: 4, PieceLength: 4 * BlockSize, TotalLength: 16 * BlockSize}
	pk := NewPicker(l, nil)
	pk.PeerBitfield(1, full(4))
	pk.PeerBitfield(2, full(4))
	a := pk.Pick(1, 2) // half of piece 0
	b := pk.Pick(2, 4) // must start a new piece, not finish piece 0
	if a[0].Index != 0 || b[0].Index == 0 {
		t.Fatalf("peer 2 joined peer 1's piece: %v / %v", a, b)
	}
	// With no new pieces left, foreign in-progress blocks are handed out.
	pk.Pick(2, 8)
	rest := pk.Pick(2, 8)
	if len(rest) == 0 || rest[0].Index != 0 {
		t.Fatalf("Pick = %v, want the rest of piece 0", rest)
	}
}

func TestPickerRandomFirstThenRarest(t *testing.T) {
	l := Layout{NumPieces: 64, PieceLength: BlockSize, TotalLength: 64 * BlockSize}
	pk := NewPicker(l, rand.New(rand.NewPCG(1, 2)))
	pk.PeerBitfield(1, full(64))
	rare := NewBitfield(64)
	for i := 32; i < 64; i++ {
		rare.Set(i)
	}
	// Pieces 0-31 are on 3 peers, 32-63 only on peer 1.
	common := NewBitfield(64)
	for i := 0; i < 32; i++ {
		common.Set(i)
	}
	pk.PeerBitfield(2, common)
	pk.PeerBitfield(3, common)
	first := pk.Pick(1, 4)
	if len(first) != 4 {
		t.Fatalf("Pick = %v", first)
	}
	inOrder := true
	for k, b := range first {
		inOrder = inOrder && int(b.Index) == k
	}
	if inOrder {
		t.Fatal("random-first should not pick pieces 0-3 in order")
	}
	for _, b := range first {
		if ok, done, _ := pk.Received(1, b); !ok || !done {
			t.Fatal("receive")
		}
		pk.PieceVerified(int(b.Index))
	}
	if pk.HaveCount() != 4 {
		t.Fatalf("HaveCount = %d", pk.HaveCount())
	}
	for _, b := range pk.Pick(1, 16) {
		if b.Index < 32 {
			t.Fatalf("after 4 pieces, rarest-first must pick rare pieces, got %d", b.Index)
		}
	}
}

func TestPickerPriorityClasses(t *testing.T) {
	l := Layout{NumPieces: 4, PieceLength: BlockSize, TotalLength: 4 * BlockSize}
	pk := NewPicker(l, nil)
	pk.PeerBitfield(1, full(4))
	pk.SetPriority(3, PriorityHigh)
	pk.SetPriority(0, PriorityLow)
	pk.SetPriority(1, PrioritySkip)
	pk.SetPriority(1, PrioritySkip) // no change
	pk.SetPriority(9, PriorityHigh) // out of range
	pk.SetPriority(2, Priority(42)) // invalid
	got := pk.Pick(1, 10)
	if len(got) != 3 || got[0].Index != 3 || got[1].Index != 2 || got[2].Index != 0 {
		t.Fatalf("Pick = %v, want 3 (high), 2 (normal), 0 (low); 1 skipped", got)
	}
	for _, b := range got {
		if _, done, _ := pk.Received(1, b); done {
			pk.PieceVerified(int(b.Index))
		}
	}
	if !pk.Done() || pk.Endgame() {
		t.Fatal("every wanted piece is have: Done, not endgame")
	}
	pk.SetPriority(1, PriorityNormal)
	if pk.Done() {
		t.Fatal("piece 1 is wanted again")
	}
	if bs := pk.Pick(1, 10); len(bs) != 1 || bs[0].Index != 1 {
		t.Fatalf("Pick = %v, want piece 1", bs)
	}
}

func TestPickerDiscardAndFailed(t *testing.T) {
	l := Layout{NumPieces: 1, PieceLength: 3 * BlockSize, TotalLength: 3 * BlockSize}
	pk := NewPicker(l, nil)
	pk.PeerBitfield(1, full(1))
	bs := pk.Pick(1, 2)
	pk.Received(1, bs[0])
	pk.Received(1, bs[1])
	pk.Discard(0)
	pk.Discard(-1)
	again := pk.Pick(1, 3)
	if len(again) != 3 {
		t.Fatalf("after Discard every block is needed again, got %v", again)
	}
	var done bool
	for _, b := range again {
		_, done, _ = pk.Received(1, b)
	}
	if !done {
		t.Fatal("piece should be pending")
	}
	pk.Discard(0) // pending: ignored
	pk.PieceFailed(0)
	if bs := pk.Pick(1, 3); len(bs) != 3 {
		t.Fatalf("after PieceFailed, Pick = %v", bs)
	}
	pk.PieceVerified(0)
	pk.PieceVerified(0) // idempotent
	if !pk.Done() || pk.HaveCount() != 1 || pk.Outstanding(1) != 0 {
		t.Fatal("verified while outstanding must clear ownership")
	}
}

func TestPickerEndgameMultipleOwners(t *testing.T) {
	l := Layout{NumPieces: 1, PieceLength: BlockSize, TotalLength: BlockSize}
	pk := NewPicker(l, nil)
	for p := PeerKey(1); p <= 3; p++ {
		pk.PeerBitfield(p, full(1))
	}
	pk.Pick(1, 1)
	if !pk.Endgame() {
		t.Fatal("endgame")
	}
	if len(pk.Pick(2, 1)) != 1 || len(pk.Pick(3, 1)) != 1 || len(pk.Pick(3, 1)) != 0 {
		t.Fatal("each peer gets the duplicate once")
	}
	if rel := pk.Release(2); len(rel) != 1 {
		t.Fatalf("Release = %v", rel)
	}
	if pk.Outstanding(1) != 1 || pk.Endgame() == false {
		t.Fatal("block is still outstanding for peers 1 and 3")
	}
	ok, done, cancel := pk.Received(3, Block{0, 0, BlockSize})
	if !ok || !done || len(cancel) != 1 || cancel[0] != 1 {
		t.Fatalf("Received = %v %v %v", ok, done, cancel)
	}
	if ok, _, _ := pk.Received(1, Block{0, 0, BlockSize}); ok {
		t.Fatal("late duplicate must be rejected")
	}
	pk.RemovePeer(1)
}

func TestPickerManySmallPiecesSinglePeer(t *testing.T) {
	const n = 4096
	l := Layout{NumPieces: n, PieceLength: BlockSize, TotalLength: n * BlockSize}
	pk := NewPicker(l, rand.New(rand.NewPCG(3, 4)))
	pk.PeerBitfield(1, full(n))
	seen := map[uint32]bool{}
	for {
		bs := pk.Pick(1, 250)
		if len(bs) == 0 {
			break
		}
		for _, b := range bs {
			if seen[b.Index] {
				t.Fatalf("piece %d picked twice", b.Index)
			}
			seen[b.Index] = true
			if _, done, _ := pk.Received(1, b); done {
				pk.PieceVerified(int(b.Index))
			}
		}
	}
	if len(seen) != n || !pk.Done() {
		t.Fatalf("picked %d pieces", len(seen))
	}
}

func BenchmarkPickManyPieces(b *testing.B) {
	const n = 4096
	l := Layout{NumPieces: n, PieceLength: BlockSize, TotalLength: n * BlockSize}
	for b.Loop() {
		pk := NewPicker(l, rand.New(rand.NewPCG(1, 1)))
		pk.PeerBitfield(1, full(n))
		out := pk.Pick(1, 250)
		for len(out) > 0 {
			blk := out[0]
			out = out[1:]
			if _, done, _ := pk.Received(1, blk); done {
				pk.PieceVerified(int(blk.Index))
			}
			if len(out) < 200 {
				out = append(out, pk.Pick(1, 50)...)
			}
		}
	}
}
