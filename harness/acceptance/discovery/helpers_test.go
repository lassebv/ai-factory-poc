// Package discovery holds black-box acceptance tests for milestone 5
// (discovery: UDP trackers, multi-tracker, scrape, DHT, PEX, LSD, magnets,
// private torrents). Everything runs on loopback against harness fakes
// (internal/fakes) that are validated against libtorrent (oraclecheck).
//
// Every negative check ("no DHT traffic for a private torrent") has a
// positive control in the same instance, so a binary without discovery
// fails the check instead of passing it vacuously.
package discovery

import (
	"bytes"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net"
	"net/netip"
	"os"
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

// deadTracker refuses connections immediately: torrents that must find
// peers some other way announce here.
const deadTracker = "http://127.0.0.1:1/announce"

type torrent struct {
	ID            string  `json:"id"`
	Name          string  `json:"name"`
	State         string  `json:"state"`
	Private       bool    `json:"private"`
	HasMetadata   bool    `json:"hasMetadata"`
	Progress      float64 `json:"progress"`
	SizeBytes     *int64  `json:"sizeBytes"`
	SwarmSeeders  *int    `json:"swarmSeeders"`
	SwarmLeechers *int    `json:"swarmLeechers"`
	ErrorMessage  *string `json:"errorMessage"`
	Files         []struct {
		Path      string `json:"path"`
		SizeBytes int64  `json:"sizeBytes"`
	} `json:"files"`
	Peers []struct {
		Addr   string `json:"addr"`
		Source string `json:"source"`
	} `json:"peers"`
}

// env is one torrentd instance with a fixed peer port.
type env struct {
	inst     *app.Instance
	peerPort int
	home     string
	args     []string
	envs     []string
}

// opts configures newEnv.
type opts struct {
	Bootstrap string   // TORRENTD_DHT_BOOTSTRAP (default: the harness discard socket)
	Args      []string // extra flags
	Env       []string // extra environment
	Home      string
}

// freePeerPort returns a port free for both TCP and UDP on all interfaces.
func freePeerPort(t *testing.T) int {
	t.Helper()
	for i := 0; i < 50; i++ {
		l, err := net.Listen("tcp", ":0")
		if err != nil {
			t.Fatal(err)
		}
		port := l.Addr().(*net.TCPAddr).Port
		u, err := net.ListenUDP("udp", &net.UDPAddr{Port: port})
		l.Close()
		if err == nil {
			u.Close()
			return port
		}
	}
	t.Fatal("no port free for both TCP and UDP")
	return 0
}

func newEnv(t *testing.T, o opts) *env {
	t.Helper()
	e := &env{peerPort: freePeerPort(t), home: o.Home}
	if e.home == "" {
		e.home = t.TempDir()
	}
	e.args = append([]string{"--peer-port", strconv.Itoa(e.peerPort)}, o.Args...)
	if o.Bootstrap != "" {
		e.envs = append(e.envs, "TORRENTD_DHT_BOOTSTRAP="+o.Bootstrap)
	}
	e.envs = append(e.envs, o.Env...)
	e.inst = app.Start(t, app.Options{Home: e.home, Args: e.args, Env: e.envs})
	return e
}

// restart stops torrentd gracefully and starts it again on the same home and
// peer port, with the environment changed by envs (appended, so they win).
func (e *env) restart(t *testing.T, envs ...string) {
	t.Helper()
	e.stop(t)
	e.start(t, envs...)
}

// start starts torrentd again (after stop) on the same home and peer port.
func (e *env) start(t *testing.T, envs ...string) {
	t.Helper()
	e.inst = app.Start(t, app.Options{Home: e.home, Args: e.args, Env: append(append([]string{}, e.envs...), envs...)})
}

func (e *env) stop(t *testing.T) {
	t.Helper()
	e.inst.Signal(syscall.SIGTERM)
	if _, ok := e.inst.Wait(15 * time.Second); !ok {
		t.Fatal("torrentd did not exit within 15 s of SIGTERM")
	}
}

func (e *env) peerAddr() netip.AddrPort {
	return netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), uint16(e.peerPort))
}

func (e *env) dbPath() string { return filepath.Join(e.home, "data", "torrentd.db") }

// gen builds a deterministic torrent unique to the test.
func gen(t *testing.T, name string, s torrentgen.Spec) *torrentgen.Torrent {
	t.Helper()
	s.Name = name
	if s.PieceLength == 0 {
		s.PieceLength = 32768
	}
	if s.Files == nil {
		s.Files = []torrentgen.File{{Size: 600_000}}
	}
	if s.Announce == "" {
		s.Announce = deadTracker
	}
	if s.Seed == 0 {
		for _, c := range t.Name() + name {
			s.Seed = s.Seed*131 + uint64(c)
		}
	}
	tor, err := torrentgen.Generate(t.TempDir(), s)
	if err != nil {
		t.Fatal(err)
	}
	return tor
}

func (e *env) post(t *testing.T, field string, value []byte, downloadDir string) app.Response {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	if field == "file" {
		part, _ := w.CreateFormFile("file", "x.torrent")
		part.Write(value)
	} else {
		w.WriteField(field, string(value))
	}
	if downloadDir != "" {
		w.WriteField("downloadDir", downloadDir)
	}
	w.Close()
	return e.inst.Do(t, "POST", "/api/v1/torrents", buf.Bytes(), map[string]string{"Content-Type": w.FormDataContentType()})
}

func decode(t *testing.T, body []byte) torrent {
	t.Helper()
	var got torrent
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decoding %.300s: %v", body, err)
	}
	return got
}

// add posts a .torrent and requires 201.
func (e *env) add(t *testing.T, tor *torrentgen.Torrent, downloadDir string) torrent {
	t.Helper()
	if downloadDir == "" {
		downloadDir = filepath.Join(t.TempDir(), "dl")
	}
	r := e.post(t, "file", tor.Bytes, downloadDir)
	if r.Status != 201 {
		t.Fatalf("POST /api/v1/torrents (file) = %d, want 201; body %.400s", r.Status, r.Body)
	}
	return decode(t, r.Body)
}

// addMagnet posts a magnet URI and requires 201 with the ADR-0020 shape.
func (e *env) addMagnet(t *testing.T, uri, downloadDir string) torrent {
	t.Helper()
	r := e.post(t, "magnet", []byte(uri), downloadDir)
	if r.Status != 201 {
		t.Fatalf("POST /api/v1/torrents with a magnet = %d, want 201 (ADR-0020 §3); body %.400s", r.Status, r.Body)
	}
	if err := spec.ValidateJSON("Torrent", r.Body); err != nil {
		t.Fatalf("POST /api/v1/torrents (magnet) response: %v", err)
	}
	return decode(t, r.Body)
}

func (e *env) get(t *testing.T, id string) torrent {
	t.Helper()
	r := e.inst.Do(t, "GET", "/api/v1/torrents/"+id, nil, nil)
	if r.Status != 200 {
		t.Fatalf("GET /api/v1/torrents/%s = %d; body %.300s", id, r.Status, r.Body)
	}
	if err := spec.ValidateJSON("Torrent", r.Body); err != nil {
		t.Fatalf("GET /api/v1/torrents/%s: %v", id, err)
	}
	return decode(t, r.Body)
}

// waitFor polls the torrent until pred holds; it fails on state error.
func (e *env) waitFor(t *testing.T, id string, timeout time.Duration, what string, pred func(torrent) bool) torrent {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last torrent
	for time.Now().Before(deadline) {
		last = e.get(t, id)
		if pred(last) {
			return last
		}
		if last.State == "error" {
			msg := ""
			if last.ErrorMessage != nil {
				msg = *last.ErrorMessage
			}
			t.Fatalf("torrent went to state error (%s) while waiting for %s\nlogs:\n%s", msg, what, tail(e.inst.Logs()))
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s (last: state=%s hasMetadata=%v progress=%.3f)\nlogs:\n%s",
		timeout, what, last.State, last.HasMetadata, last.Progress, tail(e.inst.Logs()))
	return last
}

func (e *env) waitSeeding(t *testing.T, id string, timeout time.Duration) torrent {
	t.Helper()
	return e.waitFor(t, id, timeout, "state seeding", func(x torrent) bool { return x.State == "seeding" })
}

func tail(s string) string {
	if len(s) > 3000 {
		return "…" + s[len(s)-3000:]
	}
	return s
}

func peer(t *testing.T, tor *torrentgen.Torrent, o fakes.PeerOptions) *fakes.Peer {
	t.Helper()
	p, err := fakes.NewPeer(tor, o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	return p
}

func extPeer(t *testing.T, tor *torrentgen.Torrent, o fakes.ExtPeerOptions) *fakes.ExtPeer {
	t.Helper()
	p, err := fakes.NewExtPeer(tor, o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	return p
}

func httpTracker(t *testing.T) *fakes.Tracker {
	t.Helper()
	tr := fakes.NewTracker()
	t.Cleanup(tr.Close)
	return tr
}

func udpTracker(t *testing.T) *fakes.UDPTracker {
	t.Helper()
	tr, err := fakes.NewUDPTracker()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(tr.Close)
	return tr
}

func dhtNode(t *testing.T, id [20]byte) *fakes.DHTNode {
	t.Helper()
	n, err := fakes.NewDHTNode(id)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(n.Close)
	return n
}

func lsd(t *testing.T, group string) *fakes.LSD {
	t.Helper()
	l, err := fakes.NewLSD(group, app.LoopbackInterface())
	if err != nil {
		t.Fatalf("joining LSD group %s on %s: %v", group, app.LoopbackInterface(), err)
	}
	t.Cleanup(l.Close)
	return l
}

// connected reports whether torrentd handshook with the peer for tor.
func connected(s fakes.ExtStats, tor *torrentgen.Torrent) bool {
	for _, h := range s.Handshakes {
		if h.InfoHash == tor.InfoHash {
			return true
		}
	}
	return false
}

func hashOf(q fakes.KRPCQuery) string { return q.Target }

func ihStr(tor *torrentgen.Torrent) string { return string(tor.InfoHash[:]) }

// lsdHas reports whether an LSD message names the torrent (BEP 14 hex,
// compared case-insensitively).
func lsdHas(a fakes.LSDAnnounce, tor *torrentgen.Torrent) bool {
	for _, h := range a.InfoHashes {
		if strings.EqualFold(h, tor.ID()) {
			return true
		}
	}
	return false
}

func fatalf(t *testing.T, e *env, format string, a ...any) {
	t.Helper()
	t.Fatalf("%s\nlogs:\n%s", fmt.Sprintf(format, a...), tail(e.inst.Logs()))
}

// eventually polls cond every 100 ms until it holds or the timeout expires.
func eventually(timeout time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(timeout)
	for {
		if cond() {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(100 * time.Millisecond)
	}
}
