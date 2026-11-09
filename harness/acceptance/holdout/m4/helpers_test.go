// Package m4: HOLDOUT tests for milestone 4.
// engine: seeding, choking, picker, banning, persistence, lifecycle), using
// fake peers and leechers (internal/fakes) and libtorrent (oracle).
package m4

import (
	"bytes"
	"encoding/json"
	"fmt"
	"mime/multipart"
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
	"torrentd.local/harness/acceptance/internal/oas"
	"torrentd.local/harness/acceptance/internal/torrentgen"
)

func TestMain(m *testing.M) { os.Exit(app.Main(m)) }

var spec = func() *oas.Spec {
	s, err := oas.LoadRepo(app.RepoRoot())
	if err != nil {
		panic(err)
	}
	return s
}()

// torrent is the subset of the Torrent schema the tests read.
type torrent struct {
	ID             string  `json:"id"`
	State          string  `json:"state"`
	Progress       float64 `json:"progress"`
	CompletedBytes *int64  `json:"completedBytes"`
	SizeBytes      *int64  `json:"sizeBytes"`
	WastedTotal    int64   `json:"wastedTotal"`
	UploadedTotal  int64   `json:"uploadedTotal"`
	DownloadedTot  int64   `json:"downloadedTotal"`
	StopReason     *string `json:"stopReason"`
	DownloadDir    string  `json:"downloadDir"`
	ErrorMessage   *string `json:"errorMessage"`
}

// env is one torrentd instance with a fake tracker.
type env struct {
	inst     *app.Instance
	tracker  *fakes.Tracker
	peerPort int
}

func newEnv(t *testing.T, args ...string) *env {
	t.Helper()
	tr := fakes.NewTracker()
	t.Cleanup(tr.Close)
	port := app.FreePort(t)
	inst := app.Start(t, app.Options{Args: append([]string{"--peer-port", strconv.Itoa(port)}, args...)})
	return &env{inst: inst, tracker: tr, peerPort: port}
}

// gen creates a deterministic torrent announcing to the env's tracker.
func (e *env) gen(t *testing.T, name string, pieceLength int64, files ...torrentgen.File) *torrentgen.Torrent {
	t.Helper()
	tor, err := torrentgen.Generate(t.TempDir(), torrentgen.Spec{
		Name: name, PieceLength: pieceLength, Files: files, Announce: e.tracker.URL(), Seed: uint64(len(name)) * 7919,
	})
	if err != nil {
		t.Fatal(err)
	}
	return tor
}

func (e *env) peer(t *testing.T, tor *torrentgen.Torrent, opts fakes.PeerOptions) *fakes.Peer {
	t.Helper()
	p, err := fakes.NewPeer(tor, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	return p
}

func multipartAdd(t *testing.T, torrentBytes []byte, downloadDir string) ([]byte, string) {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	part, _ := w.CreateFormFile("file", "test.torrent")
	part.Write(torrentBytes)
	if downloadDir != "" {
		w.WriteField("downloadDir", downloadDir)
	}
	w.Close()
	return buf.Bytes(), w.FormDataContentType()
}

// add posts a torrent and returns the raw response.
func (e *env) add(t *testing.T, tor *torrentgen.Torrent, downloadDir string) app.Response {
	t.Helper()
	body, ct := multipartAdd(t, tor.Bytes, downloadDir)
	return e.inst.Do(t, "POST", "/api/v1/torrents", body, map[string]string{"Content-Type": ct})
}

// mustAdd adds a torrent and checks the 201 response.
func (e *env) mustAdd(t *testing.T, tor *torrentgen.Torrent, downloadDir string) torrent {
	t.Helper()
	r := e.add(t, tor, downloadDir)
	if r.Status != 201 {
		t.Fatalf("POST /api/v1/torrents = %d, want 201; body: %.400s", r.Status, r.Body)
	}
	if err := spec.ValidateJSON("Torrent", r.Body); err != nil {
		t.Fatalf("POST /api/v1/torrents response: %v", err)
	}
	var got torrent
	json.Unmarshal(r.Body, &got)
	if got.ID != tor.ID() {
		t.Fatalf("id = %s, want the v1 info-hash %s", got.ID, tor.ID())
	}
	return got
}

func (e *env) get(t *testing.T, id string) torrent {
	t.Helper()
	r := e.inst.Do(t, "GET", "/api/v1/torrents/"+id, nil, nil)
	if r.Status != 200 {
		t.Fatalf("GET /api/v1/torrents/%s = %d; body: %.300s", id, r.Status, r.Body)
	}
	if err := spec.ValidateJSON("Torrent", r.Body); err != nil {
		t.Fatalf("GET /api/v1/torrents/%s: %v", id, err)
	}
	var got torrent
	json.Unmarshal(r.Body, &got)
	return got
}

// waitState polls until the torrent reaches state, failing on "error".
func (e *env) waitState(t *testing.T, id, state string, timeout time.Duration) torrent {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last torrent
	for time.Now().Before(deadline) {
		last = e.get(t, id)
		if last.State == state {
			return last
		}
		if last.State == "error" {
			msg := ""
			if last.ErrorMessage != nil {
				msg = *last.ErrorMessage
			}
			t.Fatalf("torrent went to state error: %s\nlogs:\n%s", msg, tail(e.inst.Logs()))
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("torrent did not reach %q within %s (last: state=%s progress=%.3f)\nlogs:\n%s",
		state, timeout, last.State, last.Progress, tail(e.inst.Logs()))
	return last
}

// download runs a complete download from the given peers and verifies it.
func (e *env) download(t *testing.T, tor *torrentgen.Torrent, timeout time.Duration) (torrent, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "downloads")
	got := e.mustAdd(t, tor, dir)
	final := e.waitState(t, got.ID, "seeding", timeout)
	if err := tor.Verify(dir); err != nil {
		t.Fatalf("downloaded data is wrong: %v", err)
	}
	if final.Progress != 1 || final.CompletedBytes == nil || *final.CompletedBytes != wanted(tor) {
		t.Errorf("final progress=%v completedBytes=%v, want 1 and %d", final.Progress, final.CompletedBytes, wanted(tor))
	}
	return final, dir
}

// wanted is the size of all non-pad files.
func wanted(tor *torrentgen.Torrent) int64 {
	var n int64
	for _, f := range tor.Files {
		if !f.Pad {
			n += f.Size
		}
	}
	return n
}

func tail(s string) string {
	if len(s) > 3000 {
		return "…" + s[len(s)-3000:]
	}
	return s
}

func files(dir string) ([]string, error) {
	var out []string
	err := filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			rel, _ := filepath.Rel(dir, p)
			out = append(out, rel)
		}
		return err
	})
	return out, err
}

func f(size int64, path ...string) torrentgen.File { return torrentgen.File{Path: path, Size: size} }

func must(t *testing.T, ok bool, format string, a ...any) {
	t.Helper()
	if !ok {
		t.Fatalf(format, a...)
	}
}

var _ = fmt.Sprint

// ---- M4 additions ---------------------------------------------------------

// restartable is an env whose torrentd can be restarted on the same home,
// data dir and peer port.
type restartable struct {
	*env
	home string
	args []string
}

func newRestartable(t *testing.T) *restartable {
	t.Helper()
	tr := fakes.NewTracker()
	t.Cleanup(tr.Close)
	port := app.FreePort(t)
	home := t.TempDir()
	args := []string{"--peer-port", strconv.Itoa(port)}
	inst := app.Start(t, app.Options{Home: home, Args: args})
	return &restartable{env: &env{inst: inst, tracker: tr, peerPort: port}, home: home, args: args}
}

// restart stops torrentd (SIGTERM, or SIGKILL when kill is set) and starts it
// again on the same home, data dir and peer port.
func (r *restartable) restart(t *testing.T, kill bool) {
	t.Helper()
	if kill {
		r.inst.Kill()
	} else {
		r.inst.Signal(syscall.SIGTERM)
		if _, ok := r.inst.Wait(15 * time.Second); !ok {
			t.Fatal("torrentd did not exit within 15 s of SIGTERM")
		}
	}
	r.inst = app.Start(t, app.Options{Home: r.home, Args: r.args})
}

// action posts a lifecycle action (pause, resume, stop-seeding, recheck).
func (e *env) action(t *testing.T, id, act string) app.Response {
	t.Helper()
	return e.inst.Do(t, "POST", "/api/v1/torrents/"+id+"/"+act, nil, map[string]string{"Content-Type": "application/json"})
}

func (e *env) mustAction(t *testing.T, id, act string) {
	t.Helper()
	if r := e.action(t, id, act); r.Status != 204 {
		t.Fatalf("POST %s = %d, want 204; body %.300s", act, r.Status, r.Body)
	}
}

// seedFromDisk adds a torrent whose complete data already sits in its
// content dir, and waits until torrentd seeds it.
func (e *env) seedFromDisk(t *testing.T, tor *torrentgen.Torrent) torrent {
	t.Helper()
	e.mustAdd(t, tor, tor.ContentDir)
	return e.waitState(t, tor.ID(), "seeding", 60*time.Second)
}

// leech connects a fake leecher to torrentd's peer port.
func (e *env) leech(t *testing.T, tor *torrentgen.Torrent, opts fakes.LeecherOptions) *fakes.Leecher {
	t.Helper()
	l, err := fakes.DialLeecher(fmt.Sprintf("127.0.0.1:%d", e.peerPort), tor, opts)
	if err != nil {
		t.Fatalf("connecting a leecher to torrentd's peer port %d: %v", e.peerPort, err)
	}
	t.Cleanup(l.Close)
	return l
}

func rss(pid int) int {
	out, err := exec.Command("ps", "-o", "rss=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return 0
	}
	v, _ := strconv.Atoi(strings.TrimSpace(string(out)))
	return v
}

func cpuSeconds(pid int) float64 {
	out, err := exec.Command("ps", "-o", "time=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return 0
	}
	// [[hh:]mm:]ss.ss
	parts := strings.Split(strings.TrimSpace(string(out)), ":")
	total := 0.0
	for _, p := range parts {
		v, _ := strconv.ParseFloat(p, 64)
		total = total*60 + v
	}
	return total
}

func tcpSockets(pid int) int {
	out, _ := exec.Command("lsof", "-n", "-P", "-a", "-p", strconv.Itoa(pid), "-i", "TCP").Output()
	n := 0
	for _, l := range strings.Split(string(out), "\n") {
		if strings.Contains(l, "TCP") {
			n++
		}
	}
	return n
}

const oracleImage = "torrentd-oracle:1"

func requireDocker(t *testing.T) {
	t.Helper()
	if err := exec.Command("docker", "info").Run(); err != nil {
		t.Fatalf("this check needs Docker (the libtorrent oracle runs in a container): %v", err)
	}
	if exec.Command("docker", "image", "inspect", oracleImage).Run() != nil {
		out, err := exec.Command("docker", "build", "-q", "-t", oracleImage, filepath.Join(app.RepoRoot(), "harness", "oracle")).CombinedOutput()
		if err != nil {
			t.Fatalf("building %s: %v\n%s", oracleImage, err, out)
		}
	}
}

func fixturesRoot() string { return filepath.Join(app.RepoRoot(), "harness", "fixtures") }

const syscallTERM = syscall.SIGTERM

// startAgain starts torrentd on the same home after the test stopped it.
func (r *restartable) startAgain(t *testing.T) {
	t.Helper()
	r.inst = app.Start(t, app.Options{Home: r.home, Args: r.args})
}

type netipAddrPort = netip.AddrPort

func mustAddrPort(s string) netip.AddrPort { return netip.MustParseAddrPort(s) }

// waitListed waits until the torrent is listed after a restart.
func (e *env) waitListed(t *testing.T, id string, timeout time.Duration) torrent {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		r := e.inst.Do(t, "GET", "/api/v1/torrents/"+id, nil, nil)
		if r.Status == 200 {
			var got torrent
			json.Unmarshal(r.Body, &got)
			return got
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("torrent %s is not listed %s after restart", id, timeout)
	return torrent{}
}

// waitAnyState polls until the torrent is in one of states (error allowed).
func (e *env) waitAnyState(t *testing.T, id string, timeout time.Duration, states ...string) torrent {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last torrent
	for time.Now().Before(deadline) {
		last = e.get(t, id)
		for _, s := range states {
			if last.State == s {
				return last
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("torrent did not reach %v within %s (last %s, progress %.3f)", states, timeout, last.State, last.Progress)
	return last
}
