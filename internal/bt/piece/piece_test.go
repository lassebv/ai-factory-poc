package piece

import (
	"bytes"
	"testing"
)

func TestBitfield(t *testing.T) {
	f := NewBitfield(10)
	if f.Len() != 10 || f.Count() != 0 || f.Full() {
		t.Fatalf("new bitfield: len %d count %d", f.Len(), f.Count())
	}
	f.Set(0)
	f.Set(9)
	f.Set(10) // ignored
	f.Set(-1) // ignored
	if !f.Has(0) || !f.Has(9) || f.Has(1) || f.Has(10) || f.Has(-1) {
		t.Fatal("Has after Set")
	}
	if got := f.Bytes(); !bytes.Equal(got, []byte{0x80, 0x40}) {
		t.Fatalf("Bytes = %x", got)
	}
	f.Clear(0)
	f.Clear(99)
	if f.Has(0) || f.Count() != 1 {
		t.Fatal("Clear")
	}
	for i := 0; i < 10; i++ {
		f.Set(i)
	}
	if !f.Full() {
		t.Fatal("Full")
	}
}

func TestFromBytes(t *testing.T) {
	if _, err := FromBytes([]byte{0xff}, 10); err != ErrBadBitfield {
		t.Errorf("short: %v", err)
	}
	if _, err := FromBytes([]byte{0xff, 0xff}, 10); err != ErrBadBitfield {
		t.Errorf("spare bits: %v", err)
	}
	f, err := FromBytes([]byte{0xff, 0xc0}, 10)
	if err != nil || !f.Full() {
		t.Fatalf("full: %v", err)
	}
	g, err := FromBytes([]byte{0x01}, 8)
	if err != nil || !g.Has(7) || g.Count() != 1 {
		t.Fatalf("exact byte: %v", err)
	}
}

func full(n int) *Bitfield {
	f := NewBitfield(n)
	for i := 0; i < n; i++ {
		f.Set(i)
	}
	return f
}

func none(Block) bool { return false }

func TestPickerGeometry(t *testing.T) {
	p := NewPicker(3*BlockSize*2+100, 2*BlockSize) // 4 pieces, last is 100 bytes
	if p.NumPieces() != 4 || p.PieceSize(3) != 100 || p.NumBlocks(0) != 2 || p.NumBlocks(3) != 1 {
		t.Fatalf("pieces %d size %d blocks %d/%d", p.NumPieces(), p.PieceSize(3), p.NumBlocks(0), p.NumBlocks(3))
	}
	for _, tc := range []struct {
		b  Block
		ok bool
	}{
		{Block{0, 0, BlockSize}, true},
		{Block{0, BlockSize, BlockSize}, true},
		{Block{3, 0, 100}, true},
		{Block{3, 0, BlockSize}, false},
		{Block{0, 1, BlockSize}, false},
		{Block{0, 2 * BlockSize, BlockSize}, false},
		{Block{4, 0, BlockSize}, false},
		{Block{-1, 0, BlockSize}, false},
	} {
		if p.Valid(tc.b) != tc.ok {
			t.Errorf("Valid(%+v) = %v", tc.b, !tc.ok)
		}
	}
}

func TestPickerSequentialDownload(t *testing.T) {
	p := NewPicker(5*BlockSize, 2*BlockSize) // pieces of 2, 2, 1 blocks
	peer := full(3)
	p.AddPeer(peer)
	if !p.Interesting(peer) {
		t.Fatal("peer should be interesting")
	}
	got := p.Pick(peer, 3, none)
	want := []Block{{0, 0, BlockSize}, {0, BlockSize, BlockSize}, {1, 0, BlockSize}}
	if len(got) != 3 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Fatalf("Pick = %+v", got)
	}
	if !p.Requested(got[0]) {
		t.Fatal("Requested")
	}
	// The in-progress piece 1 is finished before piece 2 starts.
	if more := p.Pick(peer, 1, none); len(more) != 1 || more[0] != (Block{1, BlockSize, BlockSize}) {
		t.Fatalf("second Pick = %+v", more)
	}
	if fresh, done := p.Received(got[0]); !fresh || done {
		t.Fatal("first block of piece 0")
	}
	if fresh, _ := p.Received(got[0]); fresh {
		t.Fatal("duplicate must not be fresh")
	}
	if _, done := p.Received(got[1]); !done {
		t.Fatal("piece 0 should be done")
	}
	p.Verified(0)
	p.Verified(0) // idempotent
	if !p.Have().Has(0) || p.Complete() {
		t.Fatal("after Verified(0)")
	}
	// A hash failure makes the piece pickable again.
	p.Received(Block{1, 0, BlockSize})
	p.Received(Block{1, BlockSize, BlockSize})
	p.Failed(1)
	again := p.Pick(peer, 10, none)
	if len(again) != 3 || again[0].Piece != 1 || again[2].Piece != 2 {
		t.Fatalf("after Failed: %+v", again)
	}
	for _, b := range again {
		p.Received(b)
	}
	p.Verified(1)
	p.Verified(2)
	if !p.Complete() || p.Interesting(peer) {
		t.Fatal("should be complete")
	}
	if b := p.Pick(peer, 5, none); len(b) != 0 {
		t.Fatalf("Pick when complete = %+v", b)
	}
}

func TestPickerRarestFirstAndAvailability(t *testing.T) {
	p := NewPicker(4*BlockSize, BlockSize)
	a, b := full(4), NewBitfield(4)
	b.Set(0)
	b.Set(1)
	p.AddPeer(a)
	p.AddPeer(b)
	// Pieces 2 and 3 are rarer than 0 and 1.
	if got := p.Pick(a, 1, none); got[0].Piece != 2 {
		t.Fatalf("rarest first: %+v", got)
	}
	p.RemovePeer(b)
	p.PeerHave(1)
	p.PeerHave(99) // ignored
	if got := p.Pick(a, 1, none); got[0].Piece != 0 {
		t.Fatalf("after RemovePeer: %+v", got)
	}
	// A peer without the piece is never asked for it.
	if got := p.Pick(NewBitfield(4), 5, none); len(got) != 0 {
		t.Fatalf("empty peer: %+v", got)
	}
}

func TestPickerUnrequestAndEndgame(t *testing.T) {
	p := NewPicker(2*BlockSize, BlockSize)
	a, b := full(2), full(2)
	got := p.Pick(a, 5, none)
	if len(got) != 2 {
		t.Fatalf("Pick = %+v", got)
	}
	out := map[Block]bool{got[0]: true, got[1]: true}
	// Endgame: every block is requested, so b gets duplicates, a doesn't.
	if dup := p.Pick(a, 5, func(x Block) bool { return out[x] }); len(dup) != 0 {
		t.Fatalf("a got duplicates of its own requests: %+v", dup)
	}
	dup := p.Pick(b, 5, none)
	if len(dup) != 2 {
		t.Fatalf("endgame Pick = %+v", dup)
	}
	// Dropping one of the two requests keeps the block requested.
	p.Unrequest(got[0])
	if !p.Requested(got[0]) {
		t.Fatal("block with a remaining request must stay requested")
	}
	p.Unrequest(dup[0])
	if p.Requested(got[0]) {
		t.Fatal("block with no requests must be free")
	}
	p.Unrequest(got[0]) // no-op on a free block
	p.Unrequest(Block{9, 0, 1})
	if again := p.Pick(a, 5, func(x Block) bool { return x == got[1] }); len(again) != 1 || again[0] != got[0] {
		t.Fatalf("re-pick = %+v", again)
	}
	// Receiving a free (never requested) block of an active piece still counts.
	p.Unrequest(got[0])
	if fresh, done := p.Received(got[0]); !fresh || !done {
		t.Fatal("received free block")
	}
	if fresh, _ := p.Received(Block{5, 0, BlockSize}); fresh {
		t.Fatal("invalid block")
	}
	p.Verified(1) // a piece verified while not active
	p.Verified(-1)
	if p.Have().Count() != 1 {
		t.Fatal("Verified(1)")
	}
}
