package swarm

import (
	"net/netip"
	"testing"
	"time"

	"torrentd.local/harness/acceptance/internal/fakes"
)

func firstDistinct(reqs []fakes.Request, n int) []int {
	seen := map[uint32]bool{}
	var out []int
	for _, r := range reqs {
		if !seen[r.Index] {
			seen[r.Index] = true
			out = append(out, int(r.Index))
			if len(out) == n {
				break
			}
		}
	}
	return out
}

// PICK-01 (§4.1, A5): rarest-first. Pieces 24-47 exist only on peer A;
// pieces 0-23 also exist on B and C. A rarest-first picker asks A mostly for
// the rare pieces (random-first only covers the first 4 completed pieces).
func TestRarestFirst(t *testing.T) {
	e := newEnv(t)
	tor := e.gen(t, "rarest.bin", 32768, f(48*32768))
	var common []int
	for i := 0; i < 24; i++ {
		common = append(common, i)
	}
	a := e.peer(t, tor, fakes.PeerOptions{Delay: 20 * time.Millisecond})
	b := e.peer(t, tor, fakes.PeerOptions{Delay: 20 * time.Millisecond, Has: common})
	c := e.peer(t, tor, fakes.PeerOptions{Delay: 20 * time.Millisecond, Has: common})
	e.tracker.SetPeers(a.Addr(), b.Addr(), c.Addr())
	e.download(t, tor, 120*time.Second)
	for _, p := range []*fakes.Peer{a, b, c} {
		must(t, len(p.Stats().BadRequests) == 0, "requests for pieces a peer does not have: %v", p.Stats().BadRequests)
	}
	first := firstDistinct(a.Stats().Requests, 16)
	rare := 0
	for _, i := range first {
		if i >= 24 {
			rare++
		}
	}
	t.Logf("first pieces requested from the only holder of pieces 24-47: %v (%d rare)", first, rare)
	if rare < 8 {
		t.Errorf("only %d of the first %d pieces requested from peer A were rare; want rarest-first", rare, len(first))
	}
}

// PICK-02 (§4.1 endgame): a connected peer that never answers requests for
// the last pieces must not hang the download, and duplicate requests are
// cancelled once the block arrives elsewhere.
func TestEndgame(t *testing.T) {
	e := newEnv(t)
	tor := e.gen(t, "endgame.bin", 32768, f(32*32768))
	staller := e.peer(t, tor, fakes.PeerOptions{StallFrom: 1}) // never released
	good := e.peer(t, tor, fakes.PeerOptions{Delay: 20 * time.Millisecond})
	e.tracker.SetPeers(staller.Addr(), good.Addr())
	e.download(t, tor, 45*time.Second)
	if staller.Stats().Cancels == 0 {
		t.Errorf("no cancel was sent for blocks that arrived from another peer (endgame duplicate cancellation)")
	}
}

// PIPE-01 (D1): request pipelining adapts to the rate: a fast peer drives
// the queue well above the minimum, never past 250.
func TestPipelineAdapts(t *testing.T) {
	e := newEnv(t)
	tor := e.gen(t, "pipeline.bin", 1<<20, f(64<<20))
	p := e.peer(t, tor, fakes.PeerOptions{})
	e.tracker.SetPeers(p.Addr())
	e.download(t, tor, 120*time.Second)
	got := p.Stats().MaxOutstanding
	t.Logf("max outstanding requests to a fast peer = %d", got)
	if got < 50 || got > 250+pipelineSlack {
		t.Errorf("max outstanding = %d, want 50..250 for a fast peer (clamp(rate*2s/16KiB, 5, 250))", got)
	}
}

// BAN-01 (§4.1 hash verification, G10): a peer that corrupts every piece is
// struck and disconnected; the download completes from a good peer every
// time. The good peer is on IPv6 loopback so a per-IP ban of 127.0.0.1
// cannot hit it. Three rounds catch intermittent livelocks.
func TestCorruptPeerBanned(t *testing.T) {
	for round := 1; round <= 3; round++ {
		e := newEnv(t)
		e.tracker.SetCompact(false)
		tor := e.gen(t, "ban.bin", 32768, f(1<<20))
		bad := e.peer(t, tor, fakes.PeerOptions{CorruptAll: true})
		good := e.peer(t, tor, fakes.PeerOptions{Delay: 3 * time.Millisecond, Listen: "[::1]:0"})
		e.tracker.SetPeers(bad.Addr(), good.Addr())
		start := time.Now()
		final, _ := e.download(t, tor, 30*time.Second)
		st := bad.Stats()
		t.Logf("round %d: %s, bad peer sent %d blocks, connections %d, closed by torrentd %d, wasted %d",
			round, time.Since(start).Round(time.Millisecond), st.BlocksSent, st.Connections, st.ClosedByRemote, final.WastedTotal)
		must(t, st.BlocksSent > 0, "scenario did not happen: the corrupt peer was never used")
		if st.ClosedByRemote == 0 {
			t.Errorf("round %d: torrentd never disconnected the peer whose pieces always fail", round)
		}
		if st.Connections > 2 {
			t.Errorf("round %d: torrentd reconnected %d times to a peer it should have banned", round, st.Connections)
		}
	}
}

// POISON-01: unrequested blocks are discarded. A peer flooding unrequested
// garbage must not cause hash failures and re-downloads from the good peer.
func TestUnrequestedBlocksIgnored(t *testing.T) {
	e := newEnv(t)
	tor := e.gen(t, "poison.bin", 32768, f(1<<20))
	bad := e.peer(t, tor, fakes.PeerOptions{Poison: true})
	good := e.peer(t, tor, fakes.PeerOptions{Delay: 2 * time.Millisecond, Listen: "[::1]:0"})
	e.tracker.SetCompact(false)
	e.tracker.SetPeers(bad.Addr(), good.Addr())
	e.download(t, tor, 60*time.Second)
	blocks := int(tor.Total / 16384)
	gs := good.Stats()
	must(t, bad.Stats().Connections > 0, "scenario did not happen: the poisoning peer was never connected")
	if gs.BlocksSent > blocks+blocks/10 {
		t.Errorf("the good peer served %d blocks for a %d-block torrent: unrequested data was accepted and pieces had to be re-downloaded", gs.BlocksSent, blocks)
	}
}

// CPU-01: picker efficiency with many pieces. 64 MiB in 16 KiB pieces (4096
// pieces) from one fast peer must cost little CPU (§11: idle/efficiency).
func TestPickerCPU(t *testing.T) {
	e := newEnv(t)
	tor := e.gen(t, "cpu.bin", 16384, f(64<<20))
	p := e.peer(t, tor, fakes.PeerOptions{})
	e.tracker.SetPeers(p.Addr())
	before := cpuSeconds(e.inst.PID())
	e.download(t, tor, 120*time.Second)
	used := cpuSeconds(e.inst.PID()) - before
	t.Logf("METRIC cpu_seconds_64MiB_4096_pieces=%.2f", used)
	if used > cpuBudget64MiB {
		t.Errorf("downloading 64 MiB in 4096 pieces used %.2f s of CPU (budget %.1f s)", used, cpuBudget64MiB)
	}
}

var _ = netip.Addr{}
