package download

import (
	"bufio"
	"context"
	"fmt"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"torrentd.local/harness/acceptance/internal/app"
)

func repoRoot() string { return app.RepoRoot() }

const oracleImage = "torrentd-oracle:1"

func requireDocker(t *testing.T) {
	t.Helper()
	if err := exec.Command("docker", "info").Run(); err != nil {
		t.Fatalf("SP-01 needs Docker (the libtorrent oracle runs in a container): %v", err)
	}
	if exec.Command("docker", "image", "inspect", oracleImage).Run() != nil {
		out, err := exec.Command("docker", "build", "-q", "-t", oracleImage, filepath.Join(repoRoot(), "harness", "oracle")).CombinedOutput()
		if err != nil {
			t.Fatalf("building %s: %v\n%s", oracleImage, err, out)
		}
	}
}

// TestDownloadFromLibtorrent is the interop oracle: a real libtorrent seeder.
func TestDownloadFromLibtorrent(t *testing.T) {
	requireDocker(t)
	e := newEnv(t)
	tor := e.gen(t, "interop", 65536,
		f(2_000_000, "video.bin"), f(1_234_567, "extras", "commentary.bin"), f(777, "readme.txt"), f(1_500_000, "z.bin"))
	work := filepath.Dir(tor.ContentDir)
	torrentFile := filepath.Join(work, "interop.torrent")
	if err := os.WriteFile(torrentFile, tor.Bytes, 0o644); err != nil {
		t.Fatal(err)
	}
	port := app.FreePort(t)
	name := fmt.Sprintf("torrentd-sp01-%d", time.Now().UnixNano())
	out, err := exec.Command("docker", "run", "-d", "--rm", "--name", name,
		"-p", fmt.Sprintf("127.0.0.1:%d:%d", port, port),
		"-v", work+":/work:ro", "-v", tor.ContentDir+":/content:ro",
		"-v", filepath.Join(repoRoot(), "harness", "oracle")+":/oracle:ro",
		oracleImage, "python3", "-u", "/oracle/seed.py", "/work/interop.torrent", "/content", fmt.Sprint(port)).CombinedOutput()
	if err != nil {
		t.Fatalf("docker run: %v\n%s", err, out)
	}
	t.Cleanup(func() { exec.Command("docker", "rm", "-f", name).Run() })

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	logs := exec.CommandContext(ctx, "docker", "logs", "-f", name)
	pipe, _ := logs.StdoutPipe()
	logs.Start()
	ready := false
	sc := bufio.NewScanner(pipe)
	var seen []string
	for sc.Scan() {
		seen = append(seen, sc.Text())
		if strings.Contains(sc.Text(), "SEEDING") {
			ready = true
			break
		}
	}
	cancel()
	must(t, ready, "libtorrent did not start seeding; container output:\n%s", strings.Join(seen, "\n"))

	e.tracker.SetPeers(netip.MustParseAddrPort(fmt.Sprintf("127.0.0.1:%d", port)))
	final, _ := e.download(t, tor, 10*time.Minute)
	if final.State != "seeding" || final.Progress != 1 {
		t.Errorf("final state %s progress %v, want seeding and 1", final.State, final.Progress)
	}
}
