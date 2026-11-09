package choke

// FROZEN CONTRACT TESTS (ADR-0017). Owned by the architect.

import (
	"math/rand/v2"
	"slices"
	"testing"
	"time"

	"torrentd/internal/bt/piece"
	"torrentd/internal/clock"
)

var t0 = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func leechers(n int) []Peer {
	var ps []Peer
	for i := 1; i <= n; i++ {
		ps = append(ps, Peer{Key: piece.PeerKey(i), Interested: true, ConnectedAt: t0})
	}
	return ps
}

func set(keys []piece.PeerKey) map[piece.PeerKey]bool {
	m := map[piece.PeerKey]bool{}
	for _, k := range keys {
		m[k] = true
	}
	return m
}

func check(t *testing.T, got []piece.PeerKey, peers []Peer) {
	t.Helper()
	s := set(got)
	if len(s) != len(got) {
		t.Fatalf("duplicate keys in %v", got)
	}
	if len(got) > RegularSlots+1 {
		t.Fatalf("%d unchoked, max %d", len(got), RegularSlots+1)
	}
	for _, k := range got {
		ok := false
		for _, p := range peers {
			if p.Key == k {
				ok = p.Interested
			}
		}
		if !ok {
			t.Fatalf("unchoked %d, which is absent or not interested", k)
		}
	}
}

func TestContractChokeSeedingSlotsAndRotation(t *testing.T) {
	c := NewChoker(rand.New(rand.NewPCG(1, 2)))
	peers := leechers(6)
	seen := map[piece.PeerKey]bool{}
	var lastOpt piece.PeerKey
	rotations := 0
	for tick := 0; tick <= 12; tick++ { // 120 s of rechokes
		now := t0.Add(time.Duration(tick) * RechokeInterval)
		got := c.Rechoke(now, peers, true)
		check(t, got, peers)
		if len(got) != RegularSlots+1 {
			t.Fatalf("tick %d: %d unchoked of 6 interested, want %d", tick, len(got), RegularSlots+1)
		}
		opt, ok := c.Optimistic()
		if !ok || !set(got)[opt] {
			t.Fatalf("tick %d: optimistic %d (%v) not in the unchoke set %v", tick, opt, ok, got)
		}
		if tick > 0 && opt != lastOpt {
			rotations++
			if tick%3 != 0 {
				t.Fatalf("optimistic rotated at %v, before OptimisticInterval", now.Sub(t0))
			}
		}
		lastOpt = opt
		for _, k := range got {
			seen[k] = true
		}
	}
	if rotations < 3 {
		t.Fatalf("optimistic slot rotated %d times in 120 s, want every 30 s", rotations)
	}
	if len(seen) != 6 {
		t.Fatalf("only %d distinct peers were ever unchoked", len(seen))
	}
}

func TestContractChokeByRate(t *testing.T) {
	c := NewChoker(rand.New(rand.NewPCG(3, 4)))
	peers := leechers(8)
	for i := range peers {
		peers[i].AmInterested = true
		peers[i].LastBlock = t0
		peers[i].DownloadRate = int64(i) * 1000 // 8 is fastest
		peers[i].UploadRate = int64(8-i) * 1000 // 1 is fastest
	}
	got := set(c.Rechoke(t0, peers, false))
	for _, k := range []piece.PeerKey{5, 6, 7, 8} {
		if !got[k] {
			t.Fatalf("downloading: the 4 fastest senders 5-8 must be unchoked, got %v", got)
		}
	}
	got = set(c.Rechoke(t0.Add(RechokeInterval), peers, true))
	for _, k := range []piece.PeerKey{1, 2, 3, 4} {
		if !got[k] {
			t.Fatalf("seeding: the 4 fastest receivers 1-4 must be unchoked, got %v", got)
		}
	}
}

func TestContractChokeAntiSnub(t *testing.T) {
	c := NewChoker(rand.New(rand.NewPCG(5, 6)))
	peers := leechers(6)
	for i := range peers {
		peers[i].AmInterested = true
		peers[i].LastBlock = t0
		peers[i].DownloadRate = 1000
	}
	peers[0].DownloadRate = 1 << 20 // fastest, but it stopped sending
	now := t0.Add(SnubTimeout)
	c.Rechoke(now, peers, false)
	for i := 0; i < 5; i++ {
		at := now.Add(time.Duration(i) * RechokeInterval)
		for j := 1; j < len(peers); j++ {
			peers[j].LastBlock = at // the others keep sending
		}
		got := c.Rechoke(at, peers, false)
		opt, _ := c.Optimistic()
		if set(got)[1] && opt != 1 {
			t.Fatalf("a snubbed peer (no block for %v) got a regular slot: %v", SnubTimeout, got)
		}
	}
	// Seeding never snubs.
	if got := c.Rechoke(now.Add(time.Hour), peers, true); len(got) != RegularSlots+1 {
		t.Fatalf("seeding: %v", got)
	}
}

func TestContractChokeFill(t *testing.T) {
	c := NewChoker(rand.New(rand.NewPCG(7, 8)))
	peers := leechers(2)
	got := c.Rechoke(t0, peers, true)
	if len(got) != 2 {
		t.Fatalf("2 interested peers, %v unchoked", got)
	}
	peers = append(peers, leechers(4)[2:]...) // peers 3, 4 arrive
	got2 := c.Fill(t0.Add(time.Second), peers, true)
	check(t, got2, peers)
	s := set(got2)
	if !s[1] || !s[2] || !s[3] || !s[4] {
		t.Fatalf("Fill must keep 1, 2 and give free slots to 3, 4: %v", got2)
	}
	peers[0].Interested = false
	got3 := c.Fill(t0.Add(2*time.Second), peers, true)
	check(t, got3, peers)
	if set(got3)[1] || !slices.Contains(got3, 2) {
		t.Fatalf("Fill after peer 1 lost interest: %v", got3)
	}
	if got := c.Rechoke(t0, nil, true); len(got) != 0 {
		t.Fatalf("no peers: %v", got)
	}
}

func TestContractMeter(t *testing.T) {
	clk := clock.NewFake(t0)
	m := NewMeter(clk)
	if m.Rate() != 0 {
		t.Fatal("new meter rate must be 0")
	}
	for i := 0; i < 10; i++ {
		m.Add(100_000)
		clk.Advance(500 * time.Millisecond)
	}
	if r := m.Rate(); r < 150_000 || r > 250_000 {
		t.Fatalf("200 kB/s for 5 s measured as %d", r)
	}
	clk.Advance(2 * MeterWindow)
	if r := m.Rate(); r != 0 {
		t.Fatalf("rate %d after %v of silence, want 0", r, 2*MeterWindow)
	}
	if m.Total() != 1_000_000 {
		t.Fatalf("Total = %d", m.Total())
	}
}
