package m3

import (
	"fmt"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"torrentd.local/harness/acceptance/internal/app"
	"torrentd.local/harness/acceptance/internal/fakes"
	"torrentd.local/harness/acceptance/internal/torrentgen"
)

// Each holdout test is a realistic M3-scope situation that the acceptance
// suite does not cover. They are deliberately demanding but stay inside the
// M3 scope (§15.3, ADR-0011): no seeding behaviour, no persistence, no DHT.

// H01: work is spread over several good peers, and all of them are used.
func TestH01MultiplePeers(t *testing.T) {
	e := newEnv(t)
	tor := e.gen(t, "multi.bin", 262144, f(16<<20))
	var peers []*fakes.Peer
	var addrs []netip.AddrPort
	for i := 0; i < 3; i++ {
		p := e.peer(t, tor, fakes.PeerOptions{Delay: 2 * time.Millisecond})
		peers = append(peers, p)
		addrs = append(addrs, p.Addr())
	}
	e.tracker.SetPeers(addrs...)
	e.download(t, tor, 120*time.Second)
	used := 0
	for _, p := range peers {
		if p.Stats().BlocksSent > 0 {
			used++
		}
	}
	if used < 2 {
		t.Errorf("only %d of 3 peers delivered blocks; work should be spread over peers", used)
	}
}

// H02: the only fast peer disconnects mid-download; the download finishes from another.
func TestH02PeerDropsMidDownload(t *testing.T) {
	e := newEnv(t)
	tor := e.gen(t, "drop.bin", 65536, f(2<<20))
	flaky := e.peer(t, tor, fakes.PeerOptions{DropAfter: 20})
	backup := e.peer(t, tor, fakes.PeerOptions{Delay: 5 * time.Millisecond})
	e.tracker.SetPeers(flaky.Addr(), backup.Addr())
	e.download(t, tor, 120*time.Second)
	must(t, flaky.Stats().BlocksSent >= 20, "scenario did not happen: the flaky peer sent only %d blocks before the drop point", flaky.Stats().BlocksSent)
}

// H03: one peer corrupts every piece; with a good peer present the download still completes.
func TestH03AlwaysCorruptPeer(t *testing.T) {
	e := newEnv(t)
	tor := e.gen(t, "badpeer.bin", 32768, f(1<<20))
	bad := e.peer(t, tor, fakes.PeerOptions{CorruptAll: true})
	good := e.peer(t, tor, fakes.PeerOptions{Delay: 5 * time.Millisecond})
	e.tracker.SetPeers(bad.Addr(), good.Addr())
	final, _ := e.download(t, tor, 120*time.Second)
	must(t, bad.Stats().BlocksSent > 0, "scenario did not happen: torrentd never downloaded from the corrupt peer")
	if final.WastedTotal == 0 {
		t.Errorf("the corrupt peer sent %d blocks but wastedTotal is 0", bad.Stats().BlocksSent)
	}
}

// H04: BEP 12 tier failover: tier 0 is unreachable, tier 1 works.
func TestH04TrackerTierFailover(t *testing.T) {
	e := newEnv(t)
	dead := fmt.Sprintf("http://127.0.0.1:%d/announce", app.FreePort(t))
	tor, err := torrentgen.Generate(t.TempDir(), torrentgen.Spec{Name: "tiers.bin", PieceLength: 16384,
		Files: []torrentgen.File{f(100000)}, Announce: dead, Seed: 11,
		AnnounceList: [][]string{{dead}, {e.tracker.URL()}}})
	if err != nil {
		t.Fatal(err)
	}
	p := e.peer(t, tor, fakes.PeerOptions{})
	e.tracker.SetPeers(p.Addr())
	e.download(t, tor, 90*time.Second)
}

// H05: a tracker that answers with "failure reason" falls over to the next tier.
func TestH05TrackerFailureReason(t *testing.T) {
	e := newEnv(t)
	failing := fakes.NewTracker()
	defer failing.Close()
	failing.SetFailure("torrent not registered")
	tor, err := torrentgen.Generate(t.TempDir(), torrentgen.Spec{Name: "failure.bin", PieceLength: 16384,
		Files: []torrentgen.File{f(100000)}, Announce: failing.URL(), Seed: 12,
		AnnounceList: [][]string{{failing.URL()}, {e.tracker.URL()}}})
	if err != nil {
		t.Fatal(err)
	}
	p := e.peer(t, tor, fakes.PeerOptions{})
	e.tracker.SetPeers(p.Addr())
	e.download(t, tor, 90*time.Second)
	must(t, len(failing.Announces()) > 0, "the failing tracker was never contacted")
}

// H06: the peer chokes mid-transfer (dropping outstanding requests) and unchokes later.
func TestH06ChokeMidTransfer(t *testing.T) {
	e := newEnv(t)
	tor := e.gen(t, "choke.bin", 32768, f(1<<20))
	p := e.peer(t, tor, fakes.PeerOptions{ChokeAfter: 10, ChokeFor: 2 * time.Second})
	e.tracker.SetPeers(p.Addr())
	e.download(t, tor, 90*time.Second)
	must(t, p.Stats().BlocksSent > 10, "scenario did not happen: the choke point was never reached")
}

// H07: a peer that announces its pieces with `have` messages instead of a bitfield.
func TestH07HaveInsteadOfBitfield(t *testing.T) {
	e := newEnv(t)
	tor := e.gen(t, "haves.bin", 16384, f(300000))
	p := e.peer(t, tor, fakes.PeerOptions{HaveNoBitfield: true})
	e.tracker.SetPeers(p.Addr())
	e.download(t, tor, 60*time.Second)
}

// H08: unsolicited and malformed pieces are ignored or cause a disconnect,
// never corruption or a crash; the download completes from a clean peer.
func TestH08BogusPieces(t *testing.T) {
	e := newEnv(t)
	tor := e.gen(t, "bogus.bin", 32768, f(500000))
	bogus := e.peer(t, tor, fakes.PeerOptions{Bogus: true})
	good := e.peer(t, tor, fakes.PeerOptions{Delay: 2 * time.Millisecond})
	e.tracker.SetPeers(bogus.Addr(), good.Addr())
	e.download(t, tor, 60*time.Second)
	must(t, bogus.Stats().Connections > 0, "scenario did not happen: torrentd never connected to the bogus peer")
}

// H09: SIGTERM in the middle of a download exits cleanly and quickly, and announces stopped.
func TestH09ShutdownMidDownload(t *testing.T) {
	e := newEnv(t)
	tor := e.gen(t, "midstop.bin", 32768, f(1<<20))
	p := e.peer(t, tor, fakes.PeerOptions{StallFrom: 10})
	e.tracker.SetPeers(p.Addr())
	got := e.mustAdd(t, tor, filepath.Join(t.TempDir(), "d"))
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) && p.Stats().BlocksSent < 10 {
		time.Sleep(50 * time.Millisecond)
	}
	must(t, e.get(t, got.ID).State == "downloading", "expected a download in progress")
	sent := time.Now()
	e.inst.Signal(syscall.SIGTERM)
	code, ok := e.inst.Wait(5 * time.Second)
	must(t, ok, "still running 5 s after SIGTERM during an active download")
	if code != 0 {
		t.Errorf("exit code %d, want 0", code)
	}
	if _, ok := e.tracker.WaitFor(time.Second, func(a fakes.Announce) bool { return a.Event == "stopped" }); !ok {
		t.Errorf("no stopped announce within %s of SIGTERM", time.Since(sent).Round(time.Millisecond))
	}
}

// H10: a peer that dies right after the handshake (and refuses later
// connections) does not stall the torrent; another peer takes over.
func TestH10DeadPeerDoesNotStall(t *testing.T) {
	e := newEnv(t)
	tor := e.gen(t, "deadpeer.bin", 32768, f(400000))
	dead := e.peer(t, tor, fakes.PeerOptions{DropAfter: 1})
	dead.Close()
	good := e.peer(t, tor, fakes.PeerOptions{})
	e.tracker.SetPeers(dead.Addr(), good.Addr())
	e.download(t, tor, 60*time.Second)
}

// H11: thirty slow peers at once (connection handling, concurrency).
func TestH11ManyPeers(t *testing.T) {
	e := newEnv(t)
	tor := e.gen(t, "many-peers.bin", 65536, f(8<<20))
	var addrs []netip.AddrPort
	var peers []*fakes.Peer
	for i := 0; i < 30; i++ {
		p := e.peer(t, tor, fakes.PeerOptions{Delay: 5 * time.Millisecond})
		peers = append(peers, p)
		addrs = append(addrs, p.Addr())
	}
	e.tracker.SetPeers(addrs...)
	start := time.Now()
	e.download(t, tor, 180*time.Second)
	used := 0
	for _, p := range peers {
		if p.Stats().BlocksSent > 0 {
			used++
		}
	}
	t.Logf("METRIC many_peers_s=%.2f peers_used=%d", time.Since(start).Seconds(), used)
	must(t, used >= 10, "only %d of 30 peers delivered blocks", used)
}

// P02: one very slow peer next to a fast one: a good picker does not let the
// slow peer hold the download hostage. Reports the time; fails above 30 s.
func TestP02SlowPeerNextToFastPeer(t *testing.T) {
	e := newEnv(t)
	tor := e.gen(t, "slowfast.bin", 65536, f(4<<20))
	slow := e.peer(t, tor, fakes.PeerOptions{Delay: 300 * time.Millisecond})
	fast := e.peer(t, tor, fakes.PeerOptions{})
	e.tracker.SetPeers(slow.Addr(), fast.Addr())
	start := time.Now()
	e.download(t, tor, 120*time.Second)
	d := time.Since(start)
	t.Logf("METRIC slow_fast_s=%.2f slow_blocks=%d fast_blocks=%d", d.Seconds(), slow.Stats().BlocksSent, fast.Stats().BlocksSent)
	if d > 30*time.Second {
		t.Errorf("4 MiB took %s with a fast peer available", d.Round(time.Millisecond))
	}
}

// H12: an HTTP request still in flight at SIGTERM (a browser mid-upload; at
// M6 an SSE stream) must not push the stopped announce past its 3 s budget
// (§6.2 + TRK-01): announces may not wait for the HTTP drain.
func TestH12StoppedAnnounceWithOpenHTTPRequest(t *testing.T) {
	e := newEnv(t)
	tor := e.gen(t, "inflight.bin", 16384, f(50000))
	p := e.peer(t, tor, fakes.PeerOptions{})
	e.tracker.SetPeers(p.Addr())
	e.download(t, tor, 60*time.Second)

	// Start an upload whose body never finishes.
	conn, err := net.Dial("tcp", e.inst.Addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprintf(conn, "POST /api/v1/torrents HTTP/1.1\r\nHost: %s\r\nContent-Type: multipart/form-data; boundary=x\r\nContent-Length: 1000000\r\n\r\n--x\r\n", e.inst.Addr)
	time.Sleep(300 * time.Millisecond)

	sent := time.Now()
	e.inst.Signal(syscall.SIGTERM)
	a, ok := e.tracker.WaitFor(8*time.Second, func(a fakes.Announce) bool { return a.Event == "stopped" })
	must(t, ok, "no stopped announce within 8 s of SIGTERM")
	d := a.Time.Sub(sent)
	t.Logf("METRIC stopped_after_s=%.2f", d.Seconds())
	if d > 3500*time.Millisecond {
		t.Errorf("stopped announce came %s after SIGTERM: it waited for the HTTP drain (budget 3 s)", d.Round(time.Millisecond))
	}
	if code, ok := e.inst.Wait(10 * time.Second); !ok || code != 0 {
		t.Errorf("exit: code %d, exited %v", code, ok)
	}
}

// P01: loopback throughput from a fast fake peer (§11: ≥ 50 MB/s between two
// instances). Reports MB/s, peak RSS and CPU time; fails below 25 MB/s.
func TestP01Throughput(t *testing.T) {
	e := newEnv(t)
	tor := e.gen(t, "throughput.bin", 1<<20, f(256<<20))
	var addrs []netip.AddrPort
	for i := 0; i < 2; i++ {
		addrs = append(addrs, e.peer(t, tor, fakes.PeerOptions{}).Addr())
	}
	e.tracker.SetPeers(addrs...)
	dir := filepath.Join(t.TempDir(), "d")
	start := time.Now()
	got := e.mustAdd(t, tor, dir)
	peakRSS := 0
	for {
		st := e.get(t, got.ID)
		if out, err := exec.Command("ps", "-o", "rss=", "-p", strconv.Itoa(e.inst.PID())).Output(); err == nil {
			if v, err := strconv.Atoi(strings.TrimSpace(string(out))); err == nil && v > peakRSS {
				peakRSS = v
			}
		}
		if st.State == "seeding" {
			break
		}
		if time.Since(start) > 5*time.Minute {
			t.Fatalf("256 MiB did not finish in 5 min (progress %.2f)", st.Progress)
		}
		time.Sleep(100 * time.Millisecond)
	}
	elapsed := time.Since(start)
	cpu, _ := exec.Command("ps", "-o", "time=", "-p", strconv.Itoa(e.inst.PID())).Output()
	mbps := 256 / elapsed.Seconds()
	t.Logf("METRIC throughput_mib_s=%.1f elapsed_s=%.2f peak_rss_mib=%d cpu_time=%s", mbps, elapsed.Seconds(), peakRSS/1024, strings.TrimSpace(string(cpu)))
	if err := tor.Verify(dir); err != nil {
		t.Fatal(err)
	}
	if mbps < 25 {
		t.Errorf("throughput %.1f MiB/s is below 25", mbps)
	}
}

var _ = os.Getpid
