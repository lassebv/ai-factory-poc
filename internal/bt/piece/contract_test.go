package piece

// FROZEN CONTRACT TESTS (ADR-0017, ADR-0011). Owned by the architect. Every correct
// implementation of contract.go must pass these.

import (
	"bytes"
	"errors"
	"testing"
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

// drain picks and receives every block from p until nothing is left,
// verifying each completed piece, and returns the blocks in pick order.
func drain(t *testing.T, pk Picker, p PeerKey, batch int) []Block {
	t.Helper()
	var all []Block
	for i := 0; i < 100000; i++ {
		bs := pk.Pick(p, batch)
		if len(bs) == 0 {
			return all
		}
		if len(bs) > batch {
			t.Fatalf("Pick(%d) returned %d blocks", batch, len(bs))
		}
		for _, b := range bs {
			all = append(all, b)
			ok, done, _ := pk.Received(p, b)
			if !ok {
				t.Fatalf("Received(%+v) was not accepted", b)
			}
			if done {
				pk.PieceVerified(int(b.Index))
			}
		}
	}
	t.Fatal("picker never ran out of blocks")
	return nil
}

func TestContractPickerCoversEveryBlockExactlyOnce(t *testing.T) {
	l := Layout{NumPieces: 5, PieceLength: 65536, TotalLength: 65536*4 + 20000}
	pk := NewPicker(l, nil)
	pk.PeerBitfield(1, full(5))
	blocks := drain(t, pk, 1, 7)
	seen := map[Block]bool{}
	var total int64
	for _, b := range blocks {
		if seen[b] {
			t.Fatalf("block %+v picked twice", b)
		}
		seen[b] = true
		if b.Begin%BlockSize != 0 || int64(b.Begin)+int64(b.Length) > l.PieceSize(int(b.Index)) {
			t.Fatalf("block %+v crosses the piece end or is misaligned", b)
		}
		last := int64(b.Begin)+int64(b.Length) == l.PieceSize(int(b.Index))
		if b.Length != BlockSize && !last {
			t.Fatalf("block %+v: only the last block of a piece may be short", b)
		}
		total += int64(b.Length)
	}
	if total != l.TotalLength {
		t.Fatalf("picked %d bytes, want %d", total, l.TotalLength)
	}
	if !pk.Done() || !pk.Have().Full() {
		t.Fatal("picker should be done after every piece verified")
	}
	if pk.Pick(1, 10) != nil {
		t.Fatal("Pick after Done must return nil")
	}
}

func TestContractPickerInOrderForSinglePeer(t *testing.T) {
	// ADR-0012: in-progress pieces first, then lowest index among equally
	// rare pieces. TestPartFiles (STOR-02) depends on this.
	l := Layout{NumPieces: 6, PieceLength: 32768, TotalLength: 6 * 32768}
	pk := NewPicker(l, nil)
	pk.PeerBitfield(7, full(6))
	got := pk.Pick(7, 5)
	want := []Block{{0, 0, BlockSize}, {0, BlockSize, BlockSize}, {1, 0, BlockSize}, {1, BlockSize, BlockSize}, {2, 0, BlockSize}}
	if len(got) != len(want) {
		t.Fatalf("Pick = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Pick = %v, want %v", got, want)
		}
	}
	if pk.Outstanding(7) != 5 {
		t.Fatalf("Outstanding = %d, want 5", pk.Outstanding(7))
	}
	// The in-progress piece 2 is finished before piece 3 starts.
	if next := pk.Pick(7, 2); len(next) != 2 || next[0] != (Block{2, BlockSize, BlockSize}) || next[1].Index != 3 {
		t.Fatalf("second Pick = %v, want piece 2's second block then piece 3", next)
	}
}

func TestContractPickerRespectsAvailability(t *testing.T) {
	l := Layout{NumPieces: 4, PieceLength: 16384, TotalLength: 4 * 16384}
	pk := NewPicker(l, nil)
	pk.AddPeer(1)
	if bs := pk.Pick(1, 10); len(bs) != 0 {
		t.Fatalf("a peer with no pieces got %v", bs)
	}
	pk.PeerHave(1, 2)
	pk.PeerHave(1, 99) // ignored
	bs := pk.Pick(1, 10)
	if len(bs) != 1 || bs[0].Index != 2 {
		t.Fatalf("Pick = %v, want only piece 2", bs)
	}
	// A second peer gets every other block first; the outstanding block
	// only comes last, as an endgame duplicate (ADR-0017).
	pk.PeerBitfield(2, full(4))
	bs = pk.Pick(2, 10)
	if len(bs) != 4 || bs[3].Index != 2 {
		t.Fatalf("Pick = %v, want pieces 0, 1, 3, then the endgame duplicate of piece 2", bs)
	}
	for _, b := range bs[:3] {
		if b.Index == 2 {
			t.Fatalf("block %+v handed out twice before endgame", b)
		}
	}
	if !pk.Endgame() {
		t.Fatal("every block is requested: Endgame() should be true")
	}
	ok, _, cancel := pk.Received(2, bs[3])
	if !ok || len(cancel) != 1 || cancel[0] != 1 {
		t.Fatalf("endgame duplicate: accepted=%v cancel=%v, want true [1]", ok, cancel)
	}
	if pk.Outstanding(1) != 0 {
		t.Fatal("the duplicate's other owner must lose the block")
	}
}

func TestContractPickerRarestFirst(t *testing.T) {
	l := Layout{NumPieces: 4, PieceLength: 16384, TotalLength: 4 * 16384}
	pk := NewPicker(l, nil)
	pk.PeerBitfield(1, full(4))
	pk.PeerBitfield(2, full(4))
	only3 := NewBitfield(4)
	only3.Set(3)
	pk.PeerBitfield(3, only3)
	pk.PeerHave(2, 0)
	// Availability: piece 0,1,2 = 2 peers, piece 3 = 3 peers... make piece 1
	// rarest by removing peer 2 and re-adding it without piece 1.
	pk.RemovePeer(2)
	no1 := full(4)
	no1.Clear(1)
	pk.PeerBitfield(2, no1)
	// Now: piece 0: 2, piece 1: 1, piece 2: 2, piece 3: 3.
	bs := pk.Pick(1, 1)
	if len(bs) != 1 || bs[0].Index != 1 {
		t.Fatalf("Pick = %v, want the rarest piece 1", bs)
	}
	bs = pk.Pick(1, 3)
	if len(bs) != 3 || bs[0].Index != 0 || bs[1].Index != 2 || bs[2].Index != 3 {
		t.Fatalf("Pick = %v, want pieces 0, 2 (ties by index), then 3", bs)
	}
}

func TestContractPickerReleaseAndRemove(t *testing.T) {
	l := Layout{NumPieces: 2, PieceLength: 32768, TotalLength: 65536}
	pk := NewPicker(l, nil)
	pk.PeerBitfield(1, full(2))
	pk.PeerBitfield(2, full(2))
	a := pk.Pick(1, 4)
	if len(a) != 4 || pk.Endgame() == false {
		t.Fatalf("peer 1 should hold all 4 blocks, got %v", a)
	}
	pk.Release(1)
	if pk.Outstanding(1) != 0 {
		t.Fatal("Release must clear outstanding")
	}
	b := pk.Pick(2, 4)
	if len(b) != 4 {
		t.Fatalf("released blocks should go to peer 2, got %v", b)
	}
	pk.RemovePeer(2)
	if pk.Outstanding(2) != 0 {
		t.Fatal("RemovePeer must clear outstanding")
	}
	c := pk.Pick(1, 4) // availability of peer 1 survived Release
	if len(c) != 4 {
		t.Fatalf("after RemovePeer(2), peer 1 should get the blocks back, got %v", c)
	}
	pk.RemovePeer(42) // unknown: no panic
}

func TestContractPickerReceivedRules(t *testing.T) {
	l := Layout{NumPieces: 2, PieceLength: 32768, TotalLength: 32768 + 100}
	pk := NewPicker(l, nil)
	pk.PeerBitfield(1, full(2))
	bs := pk.Pick(1, 3)
	if len(bs) != 3 || bs[2] != (Block{1, 0, 100}) {
		t.Fatalf("Pick = %v, want two blocks of piece 0 and the 100-byte block of piece 1", bs)
	}
	for _, bad := range []Block{{0, 1, BlockSize}, {0, 0, 100}, {5, 0, BlockSize}, {1, 0, BlockSize}} {
		if ok, _, _ := pk.Received(1, bad); ok {
			t.Errorf("Received(%+v) accepted a block that is not in the layout", bad)
		}
	}
	if ok, _, _ := pk.Received(2, bs[0]); ok {
		t.Fatal("a block from a peer it was not requested from must be rejected (POISON-01)")
	}
	if ok, done, _ := pk.Received(1, bs[0]); !ok || done {
		t.Fatalf("first block: accepted=%v done=%v, want true false", ok, done)
	}
	if ok, _, _ := pk.Received(1, bs[0]); ok {
		t.Fatal("a duplicate block must not be accepted")
	}
	if ok, done, _ := pk.Received(1, bs[1]); !ok || !done {
		t.Fatalf("second block: accepted=%v done=%v, want true true", ok, done)
	}
	if pk.Outstanding(1) != 1 {
		t.Fatalf("Outstanding = %d, want 1 (only piece 1's block)", pk.Outstanding(1))
	}
	// Pending piece 0 is not handed out again.
	for _, b := range pk.Pick(1, 10) {
		if b.Index == 0 {
			t.Fatalf("pending piece picked again: %+v", b)
		}
	}
	if ok, _, _ := pk.Received(1, bs[0]); ok {
		t.Fatal("a block of a pending piece must not be accepted")
	}
	pk.PieceFailed(0)
	again := pk.Pick(1, 10)
	if len(again) != 2 || again[0].Index != 0 || again[1].Index != 0 {
		t.Fatalf("after PieceFailed, piece 0's blocks should be needed again, got %v", again)
	}
	for _, b := range again {
		pk.Received(1, b)
	}
	pk.PieceVerified(0)
	if !pk.Have().Has(0) || pk.Done() {
		t.Fatal("piece 0 verified, piece 1 not yet")
	}
	if ok, _, _ := pk.Received(1, Block{0, 0, BlockSize}); ok {
		t.Fatal("a block of a verified piece must not be accepted")
	}
	if ok, done, _ := pk.Received(1, bs[2]); !ok || !done {
		t.Fatal("piece 1's single block should complete it")
	}
	pk.PieceVerified(1)
	if !pk.Done() {
		t.Fatal("all pieces verified")
	}
}

func TestContractPickerPieceVerifiedUpfront(t *testing.T) {
	l := Layout{NumPieces: 3, PieceLength: 16384, TotalLength: 3 * 16384}
	pk := NewPicker(l, nil)
	pk.PieceVerified(0) // e.g. found during checking
	pk.PieceVerified(7) // out of range: ignored
	pk.PeerBitfield(1, full(3))
	bs := pk.Pick(1, 10)
	if len(bs) != 2 || bs[0].Index != 1 || bs[1].Index != 2 {
		t.Fatalf("Pick = %v, want pieces 1 and 2", bs)
	}
	pk.PeerBitfield(9, NewBitfield(5)) // wrong length: ignored, no panic
	if n := pk.Have().Count(); n != 1 {
		t.Fatalf("Have().Count() = %d, want 1", n)
	}
}
