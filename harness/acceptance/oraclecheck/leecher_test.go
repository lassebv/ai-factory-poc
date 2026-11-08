// Package oraclecheck validates harness fakes against real libtorrent, so the
// M4 seeding/choking checks are known to observe a correct client correctly.
// It needs Docker; run it when the fakes change:  go test ./acceptance/oraclecheck/
package oraclecheck

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"torrentd.local/harness/acceptance/internal/app"
	"torrentd.local/harness/acceptance/internal/fakes"
	"torrentd.local/harness/acceptance/internal/torrentgen"
)

// ltMax is the largest UNCHOKED count libtorrent itself reported.
var ltMax struct {
	sync.Mutex
	n int
}

func seed(t *testing.T, tor *torrentgen.Torrent, extra ...string) int {
	t.Helper()
	work := filepath.Dir(tor.ContentDir)
	os.WriteFile(filepath.Join(work, "x.torrent"), tor.Bytes, 0o644)
	port := app.FreePort(t)
	name := fmt.Sprintf("oraclecheck-%d", time.Now().UnixNano())
	args := []string{"run", "-d", "--rm", "--name", name, "-p", fmt.Sprintf("127.0.0.1:%d:%d", port, port),
		"-v", work + ":/work:ro", "-v", tor.ContentDir + ":/content:ro",
		"-v", filepath.Join(app.RepoRoot(), "harness", "oracle") + ":/oracle:ro",
		"torrentd-oracle:1", "python3", "-u", "/oracle/seed.py", "/work/x.torrent", "/content", fmt.Sprint(port)}
	if out, err := exec.Command("docker", append(args, extra...)...).CombinedOutput(); err != nil {
		t.Fatalf("docker run: %v\n%s", err, out)
	}
	t.Cleanup(func() { exec.Command("docker", "rm", "-f", name).Run() })
	cmd := exec.Command("docker", "logs", "-f", name)
	pipe, _ := cmd.StdoutPipe()
	cmd.Start()
	t.Cleanup(func() { cmd.Process.Kill() })
	ready := make(chan bool, 1)
	go func() {
		sc := bufio.NewScanner(pipe)
		for sc.Scan() {
			f := strings.Fields(sc.Text())
			switch {
			case len(f) > 0 && f[0] == "SEEDING":
				ready <- true
			case len(f) == 3 && f[0] == "UNCHOKED":
				n, _ := strconv.Atoi(f[1])
				ltMax.Lock()
				ltMax.n = max(ltMax.n, n)
				ltMax.Unlock()
			}
		}
	}()
	select {
	case <-ready:
		return port
	case <-time.After(60 * time.Second):
		t.Fatal("libtorrent did not start seeding")
		return 0
	}
}

func TestLeecherDownloadsFromLibtorrent(t *testing.T) {
	tor, err := torrentgen.Generate(t.TempDir(), torrentgen.Spec{Name: "lt", PieceLength: 65536, Seed: 3,
		Announce: "http://127.0.0.1:1/announce", Files: []torrentgen.File{{Path: []string{"a"}, Size: 3_000_000}, {Path: []string{"b"}, Size: 777}}})
	if err != nil {
		t.Fatal(err)
	}
	port := seed(t, tor)
	l, err := fakes.DialLeecher(fmt.Sprintf("127.0.0.1:%d", port), tor, fakes.LeecherOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if !l.WaitComplete(60 * time.Second) {
		t.Fatalf("fake leecher did not complete from libtorrent: %+v", l.Stats())
	}
	st := l.Stats()
	if st.Verified != tor.NumPieces() || st.BadPieces != 0 || !st.Bitfield || st.Unchokes == 0 {
		t.Fatalf("stats %+v", st)
	}
}

// TestChokeObservationAgainstLibtorrent runs the exact observation logic of
// CHOKE-01 against libtorrent configured with 4 regular + 1 optimistic slots.
func TestChokeObservationAgainstLibtorrent(t *testing.T) {
	tor, err := torrentgen.Generate(t.TempDir(), torrentgen.Spec{Name: "choke", PieceLength: 262144, Seed: 4,
		Announce: "http://127.0.0.1:1/announce", Files: []torrentgen.File{{Size: 64 << 20}}})
	if err != nil {
		t.Fatal(err)
	}
	port := seed(t, tor, "4")
	var ls []*fakes.Leecher
	for i := 0; i < 6; i++ {
		l, err := fakes.DialLeecher(fmt.Sprintf("127.0.0.1:%d", port), tor, fakes.LeecherOptions{
			Pipeline: 4, Delay: 40 * time.Millisecond, StayAfter: true, PeerID: fmt.Sprintf("-FL0001-%012d", i)})
		if err != nil {
			t.Fatal(err)
		}
		defer l.Close()
		ls = append(ls, l)
	}
	maxNow, ever := fakes.WatchUnchokes(ls, 40*time.Second)
	ltMax.Lock()
	lt := ltMax.n
	ltMax.Unlock()
	t.Logf("observed: max unchoked at once = %d, distinct ever unchoked = %d; libtorrent's own max = %d", maxNow, ever, lt)
	if lt == 0 || maxNow < lt-1 || maxNow > lt+1 {
		t.Errorf("observation (%d) disagrees with libtorrent's own unchoke count (%d)", maxNow, lt)
	}
}
