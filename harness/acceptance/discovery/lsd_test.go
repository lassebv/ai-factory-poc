package discovery

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"torrentd.local/harness/acceptance/internal/app"
	"torrentd.local/harness/acceptance/internal/fakes"
	"torrentd.local/harness/acceptance/internal/torrentgen"
)

// LSD-01: BEP 14 announces. With --lsd-addr and --lsd-interface (flags,
// ADR-0020), torrentd multicasts a well-formed BT-SEARCH for an active
// torrent within 30 s of adding it, from 127.0.0.1, with Host = the group,
// Port = the peer port and the info-hash in hex; it does not repeat it more
// than once in the following 45 s (every 5 min per torrent, D6).
func TestLSDAnnounce(t *testing.T) {
	t.Parallel()
	group := fmt.Sprintf("239.192.152.143:%d", app.FreeUDPPort())
	listener := lsd(t, group)
	e := newEnv(t, opts{Args: []string{"--lsd-addr", group, "--lsd-interface", app.LoopbackInterface()}})
	tor := gen(t, "lsd-announce", torrentgen.Spec{})
	e.add(t, tor, "")
	a, ok := listener.WaitFor(30*time.Second, func(a fakes.LSDAnnounce) bool { return lsdHas(a, tor) })
	if !ok {
		fatalf(t, e, "no LSD announce for the torrent on %s within 30 s; seen: %+v", group, listener.Announces())
	}
	if a.Err != "" {
		t.Errorf("malformed BT-SEARCH: %s\n%q", a.Err, a.Raw)
	}
	if a.Host != group {
		t.Errorf("Host: %q, want %q", a.Host, group)
	}
	if a.Port != e.peerPort {
		t.Errorf("Port: %d, want the peer port %d", a.Port, e.peerPort)
	}
	if a.From.Addr().String() != "127.0.0.1" {
		t.Errorf("announce came from %v: --lsd-interface restricts LSD to loopback", a.From)
	}
	time.Sleep(45 * time.Second)
	n := 0
	for _, x := range listener.Announces() {
		if lsdHas(x, tor) && x.Time.After(a.Time) {
			n++
		}
	}
	if n > 1 {
		t.Errorf("%d more LSD announces for the torrent within 45 s of the first: the interval is 5 min (D6)", n)
	}
}

// LSD-02: torrentd uses peers announced over LSD: a BT-SEARCH on its group
// (TORRENTD_LSD_ADDR) naming the torrent and a seeder's port makes torrentd
// connect to that seeder at the sender's address and download from it.
func TestLSDReceive(t *testing.T) {
	t.Parallel()
	e := newEnv(t, opts{})
	sender := lsd(t, e.inst.LSDGroup)
	tor := gen(t, "lsd-receive", torrentgen.Spec{})
	seeder := extPeer(t, tor, fakes.ExtPeerOptions{})
	dir := filepath.Join(t.TempDir(), "dl")
	e.add(t, tor, dir)
	if !eventually(30*time.Second, func() bool {
		sender.Send(int(seeder.Addr().Port()), "harness-lsd", tor.ID())
		return seeder.WaitFor(3*time.Second, func(s fakes.ExtStats) bool { return connected(s, tor) })
	}) {
		fatalf(t, e, "torrentd did not connect to the LSD-announced peer within 30 s (announcing every 3 s on %s)", e.inst.LSDGroup)
	}
	e.waitSeeding(t, tor.ID(), 60*time.Second)
	if err := tor.Verify(dir); err != nil {
		t.Fatal(err)
	}
}
