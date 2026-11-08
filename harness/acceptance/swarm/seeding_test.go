package swarm

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"torrentd.local/harness/acceptance/internal/app"
	"torrentd.local/harness/acceptance/internal/fakes"
)

// SEED-01: a complete torrent on disk is seeded to a leecher, byte-correct,
// and the upload is counted.
func TestSeedToLeecher(t *testing.T) {
	e := newEnv(t)
	tor := e.gen(t, "seed-me", 65536, f(1_500_000, "a.bin"), f(333_333, "dir", "b.bin"))
	e.seedFromDisk(t, tor)
	l := e.leech(t, tor, fakes.LeecherOptions{})
	must(t, l.WaitComplete(60*time.Second), "leecher did not get every piece from torrentd: %+v", l.Stats())
	must(t, l.Stats().BadPieces == 0, "torrentd served corrupt data: %+v", l.Stats())
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && e.get(t, tor.ID()).UploadedTotal < tor.Total {
		time.Sleep(200 * time.Millisecond)
	}
	if up := e.get(t, tor.ID()).UploadedTotal; up < tor.Total {
		t.Errorf("uploadedTotal = %d after serving %d bytes", up, tor.Total)
	}
}

// SEED-02: a real libtorrent leecher downloads from torrentd (interop, §14.10).
func TestSeedToLibtorrent(t *testing.T) {
	requireDocker(t)
	e := newEnv(t)
	tor := e.gen(t, "interop-seed", 65536, f(2_500_000, "x.bin"), f(4321, "y.txt"))
	e.seedFromDisk(t, tor)
	out := t.TempDir()
	work := t.TempDir()
	os.WriteFile(filepath.Join(work, "t.torrent"), tor.Bytes, 0o644)
	name := fmt.Sprintf("torrentd-seed02-%d", time.Now().UnixNano())
	if b, err := exec.Command("docker", "run", "-d", "--rm", "--name", name, "--add-host=host.docker.internal:host-gateway",
		"-v", work+":/work:ro", "-v", out+":/out", "-v", filepath.Join(app.RepoRoot(), "harness", "oracle")+":/oracle:ro",
		oracleImage, "python3", "-u", "/oracle/leech.py", "/work/t.torrent", "/out", "host.docker.internal", fmt.Sprint(e.peerPort)).CombinedOutput(); err != nil {
		t.Fatalf("docker run: %v\n%s", err, b)
	}
	t.Cleanup(func() { exec.Command("docker", "rm", "-f", name).Run() })
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", "logs", "-f", name)
	pipe, _ := cmd.StdoutPipe()
	cmd.Start()
	var seen []string
	ok := false
	for sc := bufio.NewScanner(pipe); sc.Scan(); {
		seen = append(seen, sc.Text())
		if strings.HasPrefix(sc.Text(), "COMPLETE") {
			ok = true
			break
		}
		if strings.HasPrefix(sc.Text(), "TIMEOUT") {
			break
		}
	}
	cancel()
	must(t, ok, "libtorrent did not complete downloading from torrentd:\n%s", strings.Join(seen, "\n"))
	if err := tor.Verify(out); err != nil {
		t.Fatalf("libtorrent's copy differs: %v", err)
	}
}

// SWARM-01: while torrentd downloads from a seeder, it announces verified
// pieces (HAVE) and serves them to a leecher, which completes from torrentd alone.
func TestPassThrough(t *testing.T) {
	e := newEnv(t)
	tor := e.gen(t, "relay.bin", 32768, f(1_000_000))
	src := e.peer(t, tor, fakes.PeerOptions{Delay: 3 * time.Millisecond})
	e.tracker.SetPeers(src.Addr())
	dir := filepath.Join(t.TempDir(), "d")
	e.mustAdd(t, tor, dir)
	l := e.leech(t, tor, fakes.LeecherOptions{})
	must(t, l.WaitComplete(90*time.Second), "the leecher did not complete from torrentd while it was downloading: %+v", l.Stats())
	st := l.Stats()
	must(t, st.HaveMsgs > 0, "torrentd never sent HAVE for pieces it verified")
	must(t, st.BadPieces == 0, "torrentd relayed corrupt data")
	e.waitState(t, tor.ID(), "seeding", 60*time.Second)
}

// CHOKE-01 (§4.1 Choking): with 6 interested leechers, at most 4 regular plus
// 1 optimistic are unchoked at once, and the optimistic slot rotates (§4.1:
// every 30 s), so over 40 s at least 5 different leechers get unchoked.
func TestChokeSlots(t *testing.T) {
	e := newEnv(t)
	tor := e.gen(t, "choke.bin", 262144, f(64<<20))
	e.seedFromDisk(t, tor)
	var ls []*fakes.Leecher
	for i := 0; i < 6; i++ {
		ls = append(ls, e.leech(t, tor, fakes.LeecherOptions{Pipeline: 4, Delay: 40 * time.Millisecond, StayAfter: true,
			PeerID: fmt.Sprintf("-FL0001-%012d", i)}))
	}
	maxNow, distinct := fakes.WatchUnchokes(ls, 40*time.Second)
	t.Logf("max unchoked at once = %d, distinct unchoked over 40 s = %d", maxNow, distinct)
	if maxNow > 5 {
		t.Errorf("%d leechers unchoked at the same time; at most 4 regular + 1 optimistic", maxNow)
	}
	if maxNow < 4 {
		t.Errorf("only %d leechers unchoked at once while 6 wanted data and slots were free", maxNow)
	}
	if distinct < 5 {
		t.Errorf("only %d distinct leechers unchoked in 40 s; the optimistic unchoke should rotate every 30 s", distinct)
	}
}

// WIRE-03 (T6): a request for more than 16 KiB gets reject_request or a disconnect.
func TestOversizeRequest(t *testing.T) {
	e := newEnv(t)
	tor := e.gen(t, "oversize-req.bin", 65536, f(500_000))
	e.seedFromDisk(t, tor)
	l := e.leech(t, tor, fakes.LeecherOptions{Oversize: true, StayAfter: true})
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if st := l.Stats(); st.Rejects > 0 || st.ClosedByRemote {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("a 32 KiB request was neither rejected nor answered with a disconnect within 15 s: %+v", l.Stats())
}
