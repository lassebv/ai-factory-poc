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
