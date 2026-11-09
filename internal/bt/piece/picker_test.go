package piece

import (
	"net/netip"
	"testing"
	"time"
)

func TestPickerNilOptionsAndBadArgs(t *testing.T) {
	pk := New(Layout{NumPieces: -1}, Options{})
	if !pk.Done() || pk.Endgame() || pk.Have().Len() != 0 {
		t.Fatal("empty picker")
	}
	pk = New(lay(2, 1), Options{})
	pk.AddPeer(1, netip.MustParseAddr("fe80::1%eth0"))
	pk.AddPeer(1, ip(9)) // no-op
	pk.PeerBitfield(1, nil)
	pk.PeerHave(1, -1)
	pk.PeerHave(1, 0)
	pk.PeerHave(1, 0)
	pk.SetPriority(0, Priority(9))
	pk.SetPriority(0, PriorityNormal)
	if pk.Pick(1, 0) != nil || pk.Pick(9, 1) != nil {
		t.Fatal("max 0 or unknown peer must give nothing")
	}
	if r := pk.Received(9, Block{}); r.Accepted {
		t.Fatal("unknown peer")
	}
	if r := pk.Received(1, Block{Index: 5}); r.Accepted {
		t.Fatal("out of range")
	}
	if r := pk.Received(1, Block{Index: 0, Begin: 1}); r.Accepted {
		t.Fatal("unaligned")
	}
	if r := pk.Received(1, Block{Index: 0, Begin: BlockSize, Length: 1}); r.Accepted {
		t.Fatal("beyond the piece")
	}
	if r := pk.Received(1, Block{Index: 1, Length: BlockSize}); r.Accepted {
		t.Fatal("never started")
	}
	pk.Release(9)
	pk.RemovePeer(9)
	pk.PieceVerified(-1)
	pk.PieceVerified(0)
	pk.PieceVerified(0)
	if pk.Outstanding(9) != 0 || pk.PieceFailed(-1) != nil || pk.Interesting(1) {
		t.Fatal("bad args")
	}
	got := pk.Pick(1, 4)
	if len(got) != 0 {
		t.Fatalf("peer only has a verified piece: %v", got)
	}
}

func TestPickerBitfieldReplaceAndRemoveAvailability(t *testing.T) {
	pk, _ := newPK(lay(4, 1), 1)
	pk.AddPeer(1, ip(1))
	pk.PeerBitfield(1, full(4))
	b := NewBitfield(4)
	b.Set(2)
	pk.PeerBitfield(1, b)
	for _, x := range pk.Pick(1, 10) {
		if x.Index != 2 {
			t.Fatalf("picked %v after the bitfield shrank", x)
		}
	}
	pk.AddPeer(2, ip(2))
	pk.PeerBitfield(2, full(4))
	pk.RemovePeer(2)
	if pk.Pick(2, 1) != nil {
		t.Fatal("removed")
	}
}

func TestPickerSkipThenWanted(t *testing.T) {
	pk, _ := newPK(lay(3, 2), 1)
	pk.AddPeer(1, ip(1))
	pk.PeerBitfield(1, full(3))
	bs := pk.Pick(1, 1) // piece becomes active
	i := int(bs[0].Index)
	pk.SetPriority(i, PrioritySkip)
	if pk.Interesting(1) != true {
		t.Fatal("two other pieces are still wanted")
	}
	for k := 0; k < 3; k++ {
		if k != i {
			pk.SetPriority(k, PrioritySkip)
		}
	}
	if pk.Interesting(1) || !pk.Done() || pk.Pick(1, 10) != nil {
		t.Fatal("everything skipped")
	}
	pk.SetPriority(i, PriorityLow)
	got := pk.Pick(1, 10)
	if len(got) != 1 || int(got[0].Index) != i || got[0].Begin != BlockSize {
		t.Fatalf("resumed skip piece: %v", got)
	}
	pk.Received(1, bs[0])
	if r := pk.Received(1, got[0]); !r.PieceDone {
		t.Fatal("not done")
	}
	pk.PieceVerified(i)
	pk.SetPriority(i, PrioritySkip) // have pieces may change class
	if !pk.Done() {
		t.Fatal("done")
	}
}

func TestPickerExpireCooldownEndsAndQueueCompacts(t *testing.T) {
	pk, clk := newPK(lay(3000, 1), 1)
	pk.AddPeer(1, ip(1))
	pk.PeerBitfield(1, full(3000))
	for k := 0; k < 3; k++ {
		pk.Pick(1, 1000)
		clk.Advance(DefaultRequestTimeout)
		if n := len(pk.Expire()); n != 1000 {
			t.Fatalf("expired %d", n)
		}
		if pk.Pick(1, 1) != nil {
			t.Fatal("cooldown")
		}
		clk.Advance(DefaultRequestTimeout)
		pk.Expire()
	}
	got := pk.Pick(1, 1)
	if len(got) != 1 {
		t.Fatal("cooldown must end after RequestTimeout")
	}
	pk.RemovePeer(1)
	clk.Advance(time.Hour)
	if len(pk.Expire()) != 0 {
		t.Fatal("removed peer's requests must not expire")
	}
}

func TestPickerReleasedExclusiveGoesToNextPeer(t *testing.T) {
	pk, _ := newPK(lay(1, 2), 1)
	for _, p := range []PeerKey{1, 2} {
		pk.AddPeer(p, ip(byte(p)))
		pk.PeerBitfield(p, full(1))
	}
	for _, b := range pk.Pick(1, 2) {
		pk.Received(1, b)
	}
	if got := pk.PieceFailed(0); len(got) != 1 {
		t.Fatalf("contributors %v", got)
	}
	if len(pk.Pick(1, 1)) != 1 || pk.Pick(2, 2) != nil {
		t.Fatal("exclusive piece must stay with peer 1")
	}
	pk.Release(1)
	if got := pk.Pick(2, 2); len(got) != 2 {
		t.Fatalf("after release the exclusive piece is free: %v", got)
	}
}

func TestPickerVerifiedWhileOutstanding(t *testing.T) {
	pk, _ := newPK(lay(2, 2), 1)
	pk.AddPeer(1, ip(1))
	pk.PeerBitfield(1, full(2))
	bs := pk.Pick(1, 4)
	pk.PieceVerified(int(bs[0].Index))
	if pk.Outstanding(1) != 2 {
		t.Fatalf("outstanding %d, want 2", pk.Outstanding(1))
	}
	if r := pk.Received(1, bs[0]); r.Accepted {
		t.Fatal("block of a have piece accepted")
	}
}

func TestPickerManyPeersDrain(t *testing.T) {
	l := Layout{NumPieces: 37, PieceLength: 3 * BlockSize, TotalLength: 36*3*BlockSize + 100}
	pk, _ := newPK(l, 5)
	for p := PeerKey(1); p <= 6; p++ {
		pk.AddPeer(p, ip(byte(p)))
		bf := NewBitfield(37)
		for i := int(p) % 2; i < 37; i++ {
			bf.Set(i)
		}
		pk.PeerBitfield(p, bf)
	}
	for round := 0; round < 1000 && !pk.Done(); round++ {
		for p := PeerKey(1); p <= 6; p++ {
			for _, b := range pk.Pick(p, 5) {
				if r := pk.Received(p, b); r.PieceDone {
					pk.PieceVerified(int(b.Index))
				}
			}
		}
	}
	if !pk.Done() || pk.Endgame() {
		t.Fatal("swarm never finished")
	}
}
