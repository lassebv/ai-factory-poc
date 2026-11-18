package oraclecheck

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"torrentd.local/harness/acceptance/internal/app"
)

// The M5 discovery fakes are validated against real libtorrent. The fakes run
// inside the oracle container next to libtorrent (fakesrv), so every packet
// stays on the container's loopback.

func runFakesrv(t *testing.T, scenario string) {
	t.Helper()
	if err := exec.Command("docker", "image", "inspect", "torrentd-oracle:1").Run(); err != nil {
		out, err := exec.Command("docker", "build", "-q", "-t", "torrentd-oracle:1", filepath.Join(app.RepoRoot(), "harness", "oracle")).CombinedOutput()
		if err != nil {
			t.Fatalf("building the oracle image: %v\n%s", err, out)
		}
	}
	bin := filepath.Join(t.TempDir(), "fakesrv")
	build := exec.Command("go", "build", "-o", bin, "./acceptance/oraclecheck/fakesrv")
	build.Dir = filepath.Join(app.RepoRoot(), "harness")
	build.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+runtime.GOARCH)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building fakesrv: %v\n%s", err, out)
	}
	cmd := exec.Command("docker", "run", "--rm",
		"-v", bin+":/fakesrv:ro",
		"-v", filepath.Join(app.RepoRoot(), "harness", "oracle")+":/oracle:ro",
		"torrentd-oracle:1", "/fakesrv", scenario)
	out, err := cmd.CombinedOutput()
	t.Logf("fakesrv %s:\n%s", scenario, out)
	if err != nil || !strings.Contains(string(out), "\nPASS") {
		t.Fatalf("scenario %s failed: %v", scenario, err)
	}
}

// TestUDPTrackerAgainstLibtorrent: libtorrent announces to and scrapes the
// fake UDP tracker (BEP 15) and ignores its wrong-transaction-id decoys.
func TestUDPTrackerAgainstLibtorrent(t *testing.T) { runFakesrv(t, "udp") }

// TestDHTAgainstLibtorrent: libtorrent's DHT walks the fake KRPC network to
// the nodes holding peers, connects to the peer, announces with a valid
// token; libtorrent's DHT answers the probe queries DHT-07 sends to torrentd.
func TestDHTAgainstLibtorrent(t *testing.T) { runFakesrv(t, "dht") }

// TestMetadataPeerAgainstLibtorrent: libtorrent resolves a magnet from the
// fake ut_metadata peer (3 metadata pieces) and downloads; it rejects the
// fake's corrupted metadata.
func TestMetadataPeerAgainstLibtorrent(t *testing.T) { runFakesrv(t, "magnet") }

// TestPEXAgainstLibtorrent: libtorrent connects to a peer introduced by the
// fake's ut_pex, and the fake decodes libtorrent's own ut_pex messages.
func TestPEXAgainstLibtorrent(t *testing.T) { runFakesrv(t, "pex") }
