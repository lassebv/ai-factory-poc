package m4

// HOLDOUT tests for milestone 4, written 2026-11-08 BEFORE the A and A+ runs
// and kept outside the repository until both finished. They are realistic
// M4-scope situations that the M4 acceptance suite does not cover.

import (
	"fmt"
	"net/netip"
	"path/filepath"
	"testing"
	"time"

	"torrentd.local/harness/acceptance/internal/fakes"
	"torrentd.local/harness/acceptance/internal/torrentgen"
)

// HM01: ten leechers at once; with 4+1 unchoke slots all of them finish eventually.
func TestHM01TenLeechersAllFinish(t *testing.T) {
	e := newEnv(t)
	tor := e.gen(t, "ten.bin", 65536, f(4<<20))
	e.seedFromDisk(t, tor)
	var ls []*fakes.Leecher
	for i := 0; i < 10; i++ {
		ls = append(ls, e.leech(t, tor, fakes.LeecherOptions{Pipeline: 8, Delay: 5 * time.Millisecond, PeerID: fmt.Sprintf("-FL0001-%012d", i)}))
	}
	start := time.Now()
	for i, l := range ls {
		if !l.WaitComplete(3*time.Minute - time.Since(start)) {
			t.Fatalf("leecher %d did not finish within 3 min: %+v", i, l.Stats())
		}
	}
	t.Logf("METRIC ten_leechers_s=%.1f", time.Since(start).Seconds())
}

// HM02: torrentd seeds to another torrentd over loopback (§11: >= 50 MB/s between two instances).
func TestHM02TorrentdToTorrentd(t *testing.T) {
	seeder := newEnv(t)
	tor := seeder.gen(t, "t2t.bin", 1<<20, f(256<<20))
	seeder.seedFromDisk(t, tor)
	seeder.tracker.SetPeers(netip.MustParseAddrPort(fmt.Sprintf("127.0.0.1:%d", seeder.peerPort)))
	leecher := newEnv(t)
	dir := filepath.Join(t.TempDir(), "d")
	start := time.Now()
	leecher.mustAdd(t, tor, dir)
	leecher.waitState(t, tor.ID(), "seeding", 5*time.Minute)
	mibs := 256 / time.Since(start).Seconds()
	t.Logf("METRIC torrentd_to_torrentd_mib_s=%.1f", mibs)
	if err := tor.Verify(dir); err != nil {
		t.Fatal(err)
	}
	if mibs < 50 {
		t.Errorf("%.1f MiB/s between two torrentd instances, §11 wants >= 50 MB/s", mibs)
	}
}

// HM03: a small swarm: torrentd downloads from two seeders while three
// leechers download from torrentd; everyone ends with correct data.
func TestHM03SmallSwarm(t *testing.T) {
	e := newEnv(t)
	tor := e.gen(t, "swarm.bin", 65536, f(6<<20))
	s1 := e.peer(t, tor, fakes.PeerOptions{Delay: 2 * time.Millisecond})
	s2 := e.peer(t, tor, fakes.PeerOptions{Delay: 2 * time.Millisecond})
	e.tracker.SetPeers(s1.Addr(), s2.Addr())
	dir := filepath.Join(t.TempDir(), "d")
	e.mustAdd(t, tor, dir)
	var ls []*fakes.Leecher
	for i := 0; i < 3; i++ {
		ls = append(ls, e.leech(t, tor, fakes.LeecherOptions{PeerID: fmt.Sprintf("-FL0001-%012d", i)}))
	}
	e.waitState(t, tor.ID(), "seeding", 2*time.Minute)
	for i, l := range ls {
		if !l.WaitComplete(2 * time.Minute) {
			t.Fatalf("leecher %d did not complete: %+v", i, l.Stats())
		}
	}
	if err := tor.Verify(dir); err != nil {
		t.Fatal(err)
	}
}

// HM04: restart while seeding; a leecher that reconnects afterwards finishes.
func TestHM04RestartWhileSeeding(t *testing.T) {
	r := newRestartable(t)
	tor := r.gen(t, "reseed.bin", 65536, f(4<<20))
	r.seedFromDisk(t, tor)
	l := r.leech(t, tor, fakes.LeecherOptions{Pipeline: 2, Delay: 20 * time.Millisecond})
	time.Sleep(2 * time.Second)
	r.restart(t, false)
	r.waitAnyState(t, tor.ID(), 30*time.Second, "seeding")
	l.Close()
	l2 := r.leech(t, tor, fakes.LeecherOptions{})
	if !l2.WaitComplete(2 * time.Minute) {
		t.Fatalf("after a restart the seeder did not serve a new leecher: %+v", l2.Stats())
	}
}

// HM05: pause, restart, resume: the download finishes correctly.
func TestHM05PauseRestartResume(t *testing.T) {
	r := newRestartable(t)
	tor := r.gen(t, "prr.bin", 32768, f(64*32768))
	p := r.peer(t, tor, fakes.PeerOptions{Delay: 20 * time.Millisecond})
	r.tracker.SetPeers(p.Addr())
	dir := filepath.Join(r.home, "dl")
	r.mustAdd(t, tor, dir)
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline) && r.get(t, tor.ID()).Progress < 0.2; {
		time.Sleep(100 * time.Millisecond)
	}
	r.mustAction(t, tor.ID(), "pause")
	r.waitAnyState(t, tor.ID(), 10*time.Second, "paused")
	r.restart(t, false)
	r.waitListed(t, tor.ID(), 10*time.Second)
	r.waitAnyState(t, tor.ID(), 15*time.Second, "paused")
	r.mustAction(t, tor.ID(), "resume")
	r.waitState(t, tor.ID(), "seeding", 2*time.Minute)
	if err := tor.Verify(dir); err != nil {
		t.Fatal(err)
	}
}

// HM06: stop seeding, restart, resume seeding: back to seeding and serving.
func TestHM06StopRestartResume(t *testing.T) {
	r := newRestartable(t)
	tor := r.gen(t, "srr.bin", 65536, f(2<<20))
	r.seedFromDisk(t, tor)
	r.mustAction(t, tor.ID(), "stop-seeding")
	r.waitAnyState(t, tor.ID(), 10*time.Second, "stopped")
	r.restart(t, false)
	r.waitListed(t, tor.ID(), 10*time.Second)
	r.waitAnyState(t, tor.ID(), 15*time.Second, "stopped")
	r.mustAction(t, tor.ID(), "resume")
	r.waitAnyState(t, tor.ID(), 30*time.Second, "seeding")
	l := r.leech(t, tor, fakes.LeecherOptions{})
	if !l.WaitComplete(time.Minute) {
		t.Fatalf("after stop, restart and resume the seeder did not serve: %+v", l.Stats())
	}
}

// HM07: a hostile mix (always-corrupt, stalling and poisoning peers) next to
// one good IPv6 peer: the download still completes correctly.
func TestHM07HostileMix(t *testing.T) {
	e := newEnv(t)
	e.tracker.SetCompact(false)
	tor := e.gen(t, "hostile.bin", 32768, f(1<<20))
	corrupt := e.peer(t, tor, fakes.PeerOptions{CorruptAll: true})
	stall := e.peer(t, tor, fakes.PeerOptions{StallFrom: 1})
	poison := e.peer(t, tor, fakes.PeerOptions{Poison: true})
	good := e.peer(t, tor, fakes.PeerOptions{Delay: 5 * time.Millisecond, Listen: "[::1]:0"})
	e.tracker.SetPeers(corrupt.Addr(), stall.Addr(), poison.Addr(), good.Addr())
	start := time.Now()
	e.download(t, tor, 90*time.Second)
	t.Logf("METRIC hostile_mix_s=%.1f", time.Since(start).Seconds())
}

// HM08: twenty torrents seeding at once, each leeched once; all complete and
// memory stays reasonable.
func TestHM08TwentyTorrents(t *testing.T) {
	e := newEnv(t)
	var tors []*torrentgen.Torrent
	for i := 0; i < 20; i++ {
		tor := e.gen(t, fmt.Sprintf("many-%02d.bin", i), 65536, f(int64(500_000+i*1000)))
		e.mustAdd(t, tor, tor.ContentDir)
		tors = append(tors, tor)
	}
	for _, tor := range tors {
		e.waitState(t, tor.ID(), "seeding", time.Minute)
	}
	var ls []*fakes.Leecher
	for _, tor := range tors {
		ls = append(ls, e.leech(t, tor, fakes.LeecherOptions{}))
	}
	peak := 0
	for i, l := range ls {
		peak = max(peak, rss(e.inst.PID()))
		if !l.WaitComplete(time.Minute) {
			t.Fatalf("leecher for torrent %d did not complete: %+v", i, l.Stats())
		}
	}
	t.Logf("METRIC twenty_torrents_peak_rss_mib=%d", peak/1024)
	if peak > 200<<10 {
		t.Errorf("peak RSS %d MiB seeding 20 small torrents", peak/1024)
	}
}

// HM09: kill -9 while seeding: after restart it seeds again and the data is intact.
func TestHM09Kill9WhileSeeding(t *testing.T) {
	r := newRestartable(t)
	tor := r.gen(t, "k9seed.bin", 65536, f(4<<20))
	r.seedFromDisk(t, tor)
	l := r.leech(t, tor, fakes.LeecherOptions{Pipeline: 2, Delay: 20 * time.Millisecond})
	time.Sleep(3 * time.Second)
	r.restart(t, true)
	l.Close()
	r.waitListed(t, tor.ID(), 10*time.Second)
	r.waitAnyState(t, tor.ID(), time.Minute, "seeding")
	if err := tor.Verify(tor.ContentDir); err != nil {
		t.Fatalf("data changed after kill -9 while seeding: %v", err)
	}
	l2 := r.leech(t, tor, fakes.LeecherOptions{})
	if !l2.WaitComplete(time.Minute) {
		t.Fatalf("did not serve after kill -9 and restart: %+v", l2.Stats())
	}
}

// HM10: seeding throughput to one fast leecher (MiB/s, CPU).
func TestHM10SeedThroughput(t *testing.T) {
	e := newEnv(t)
	tor := e.gen(t, "seedfast.bin", 1<<20, f(256<<20))
	e.seedFromDisk(t, tor)
	cpu0 := cpuSeconds(e.inst.PID())
	start := time.Now()
	l := e.leech(t, tor, fakes.LeecherOptions{Pipeline: 64})
	must(t, l.WaitComplete(5*time.Minute), "leecher did not finish 256 MiB in 5 min: %+v", l.Stats())
	d := time.Since(start).Seconds()
	t.Logf("METRIC seed_mib_s=%.1f cpu_s=%.2f", 256/d, cpuSeconds(e.inst.PID())-cpu0)
}

// HM11 (added after blind review; claimed against both): pause and resume
// while the initial check is still running must neither leak storage nor get
// stuck: the torrent ends up seeding and serves a leecher.
func TestHM11PauseResumeDuringCheck(t *testing.T) {
	e := newEnv(t)
	tor := e.gen(t, "bigcheck.bin", 1<<20, f(768<<20))
	e.mustAdd(t, tor, tor.ContentDir)
	sawChecking := false
	for i := 0; i < 50; i++ {
		if e.get(t, tor.ID()).State == "checking" {
			sawChecking = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	must(t, sawChecking, "scenario did not happen: the check finished before it could be interrupted")
	for i := 0; i < 3; i++ {
		e.action(t, tor.ID(), "pause")
		time.Sleep(100 * time.Millisecond)
		e.action(t, tor.ID(), "resume")
		time.Sleep(100 * time.Millisecond)
	}
	got := e.waitAnyState(t, tor.ID(), 90*time.Second, "seeding", "error")
	must(t, got.State == "seeding", "after pause/resume during the check the torrent is %s, want seeding", got.State)
	l := e.leech(t, tor, fakes.LeecherOptions{Want: []int{0, 100, 700}})
	must(t, l.WaitComplete(30*time.Second), "did not serve after pause/resume during check: %+v", l.Stats())
}
