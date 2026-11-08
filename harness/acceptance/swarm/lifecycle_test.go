package swarm

import (
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"torrentd.local/harness/acceptance/internal/fakes"
)

// STOP-01 (§5.2, AC-06): stop seeding closes peer connections, uploads stop,
// a stopped announce is sent, files stay; resume seeding brings it back.
func TestStopAndResumeSeeding(t *testing.T) {
	e := newEnv(t)
	tor := e.gen(t, "stop-seeding.bin", 65536, f(8<<20))
	e.seedFromDisk(t, tor)
	l := e.leech(t, tor, fakes.LeecherOptions{Pipeline: 2, Delay: 50 * time.Millisecond, StayAfter: true})
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline) && l.Stats().Blocks < 5; {
		time.Sleep(100 * time.Millisecond)
	}
	must(t, l.Stats().Blocks >= 5, "setup: the leecher received no data")
	e.mustAction(t, tor.ID(), "stop-seeding")
	got := e.waitAnyState(t, tor.ID(), 10*time.Second, "stopped")
	if got.StopReason == nil || *got.StopReason != "user" {
		t.Errorf("stopReason = %v, want user", got.StopReason)
	}
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline) && !l.Stats().Closed; {
		time.Sleep(100 * time.Millisecond)
	}
	must(t, l.Stats().ClosedByRemote, "the leecher's connection was not closed after stop seeding")
	_, ok := e.tracker.WaitFor(5*time.Second, func(a fakes.Announce) bool { return a.Event == "stopped" })
	must(t, ok, "no stopped announce after stop seeding")
	if err := tor.Verify(tor.ContentDir); err != nil {
		t.Errorf("files changed after stop seeding: %v", err)
	}
	up := e.get(t, tor.ID()).UploadedTotal
	if l2, err := fakes.DialLeecher(fmt.Sprintf("127.0.0.1:%d", e.peerPort), tor, fakes.LeecherOptions{}); err == nil {
		time.Sleep(3 * time.Second)
		if l2.Stats().Blocks > 0 {
			t.Errorf("a stopped torrent still uploaded %d blocks", l2.Stats().Blocks)
		}
		l2.Close()
	}
	if after := e.get(t, tor.ID()).UploadedTotal; after != up {
		t.Errorf("uploadedTotal grew from %d to %d while stopped", up, after)
	}

	e.mustAction(t, tor.ID(), "resume")
	e.waitAnyState(t, tor.ID(), 15*time.Second, "seeding")
	l3 := e.leech(t, tor, fakes.LeecherOptions{})
	must(t, l3.WaitComplete(60*time.Second), "after resume seeding a new leecher could not complete: %+v", l3.Stats())
}

// PAUSE-01 (§5.1): pausing a download stops requesting data; resume continues.
func TestPauseAndResumeDownload(t *testing.T) {
	e := newEnv(t)
	tor := e.gen(t, "pause.bin", 32768, f(64*32768))
	p := e.peer(t, tor, fakes.PeerOptions{Delay: 30 * time.Millisecond})
	e.tracker.SetPeers(p.Addr())
	dir := filepath.Join(t.TempDir(), "d")
	e.mustAdd(t, tor, dir)
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline) && p.Stats().BlocksSent < 10; {
		time.Sleep(50 * time.Millisecond)
	}
	e.mustAction(t, tor.ID(), "pause")
	e.waitAnyState(t, tor.ID(), 10*time.Second, "paused")
	time.Sleep(1500 * time.Millisecond)
	n := len(p.Stats().Requests)
	time.Sleep(3 * time.Second)
	if m := len(p.Stats().Requests); m != n {
		t.Errorf("%d new requests while paused", m-n)
	}
	e.mustAction(t, tor.ID(), "resume")
	e.waitState(t, tor.ID(), "seeding", 90*time.Second)
	if err := tor.Verify(dir); err != nil {
		t.Fatal(err)
	}
}

// LIFE-04 (G18): invalid actions are refused with the specified codes.
func TestActionValidity(t *testing.T) {
	e := newEnv(t)
	done := e.gen(t, "complete.bin", 32768, f(100_000))
	e.seedFromDisk(t, done)
	busy := e.gen(t, "incomplete.bin", 32768, f(1<<20))
	p := e.peer(t, busy, fakes.PeerOptions{StallFrom: 1})
	e.tracker.SetPeers(p.Addr())
	e.mustAdd(t, busy, filepath.Join(t.TempDir(), "d"))
	e.waitAnyState(t, busy.ID(), 15*time.Second, "downloading")
	check := func(id, act string, status int, code string) {
		t.Helper()
		r := e.action(t, id, act)
		if r.Status != status || (code != "" && !strings.Contains(string(r.Body), code)) {
			t.Errorf("%s on %s = %d %.200s, want %d %s", act, id[:8], r.Status, r.Body, status, code)
		}
	}
	check(done.ID(), "pause", 409, "invalid_state")
	check(busy.ID(), "stop-seeding", 409, "not_complete")
	check(done.ID(), "resume", 204, "")
	check(strings.Repeat("ab", 20), "pause", 404, "not_found")
}

// SHUT-01 (§16.11, from M3 holdout H12): the stopped announce does not wait
// for an HTTP request still in flight at SIGTERM.
func TestStoppedAnnounceWithOpenHTTPRequest(t *testing.T) {
	e := newEnv(t)
	tor := e.gen(t, "inflight.bin", 16384, f(50000))
	p := e.peer(t, tor, fakes.PeerOptions{})
	e.tracker.SetPeers(p.Addr())
	e.download(t, tor, 60*time.Second)
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
	if d := a.Time.Sub(sent); d > 3500*time.Millisecond {
		t.Errorf("stopped announce came %s after SIGTERM: it waited for the HTTP drain (§16.11: 3 s budget from the signal)", d.Round(time.Millisecond))
	}
}

// MEM-01 (from M3 holdout P03): peak RSS stays bounded with large pieces and
// many peers working different pieces.
func TestMemoryLargePieces(t *testing.T) {
	e := newEnv(t)
	tor := e.gen(t, "bigpieces.bin", 8<<20, f(128<<20))
	var addrs []string
	for i := 0; i < 16; i++ {
		addrs = append(addrs, e.peer(t, tor, fakes.PeerOptions{Delay: 2 * time.Millisecond}).Addr().String())
	}
	setPeers(e, addrs)
	dir := filepath.Join(t.TempDir(), "d")
	got := e.mustAdd(t, tor, dir)
	peak := 0
	for start := time.Now(); e.get(t, got.ID).State != "seeding"; {
		peak = max(peak, rss(e.inst.PID()))
		must(t, time.Since(start) < 5*time.Minute, "did not finish in 5 min")
		time.Sleep(50 * time.Millisecond)
	}
	t.Logf("METRIC peak_rss_mib=%d", peak/1024)
	if err := tor.Verify(dir); err != nil {
		t.Fatal(err)
	}
	if peak > 150<<10 {
		t.Errorf("peak RSS %d MiB downloading 8 MiB pieces from 16 peers (bound 150 MiB)", peak/1024)
	}
}

// FD-01 (§4.5, resource bounds): after 60 short-lived peer connections the
// number of open TCP sockets returns to its baseline (no leaked connections).
func TestNoLeakedConnections(t *testing.T) {
	e := newEnv(t)
	tor := e.gen(t, "churn.bin", 65536, f(1<<20))
	e.seedFromDisk(t, tor)
	time.Sleep(time.Second)
	base := tcpSockets(e.inst.PID())
	for i := 0; i < 60; i++ {
		c, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", e.peerPort))
		if err != nil {
			continue
		}
		if i%2 == 0 {
			c.Write([]byte{19, 'B', 'i', 't'}) // half a handshake
		}
		c.Close()
		l, err := fakes.DialLeecher(fmt.Sprintf("127.0.0.1:%d", e.peerPort), tor, fakes.LeecherOptions{Want: []int{0}})
		if err == nil {
			l.WaitComplete(5 * time.Second)
			l.Close()
		}
	}
	var now int
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); {
		if now = tcpSockets(e.inst.PID()); now <= base+3 {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Errorf("open TCP sockets: %d before, %d 15 s after 120 closed peer connections", base, now)
}

func setPeers(e *env, addrs []string) {
	var ps []netipAddrPort
	for _, a := range addrs {
		ps = append(ps, mustAddrPort(a))
	}
	e.tracker.SetPeers(ps...)
}
