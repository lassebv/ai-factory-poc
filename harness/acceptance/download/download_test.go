package download

import (
	"bytes"
	"encoding/json"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"torrentd.local/harness/acceptance/internal/fakes"
	"torrentd.local/harness/acceptance/internal/torrentgen"
)

func TestAddTorrent(t *testing.T) {
	e := newEnv(t)
	tor := e.gen(t, "add-me.bin", 16384, f(40000))
	dir := filepath.Join(t.TempDir(), "not", "yet", "there")
	added := e.mustAdd(t, tor, dir)
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		t.Errorf("download dir %s was not created", dir)
	}
	if added.DownloadDir != dir {
		t.Errorf("downloadDir = %q, want %q", added.DownloadDir, dir)
	}
	e.get(t, tor.ID()) // validates against the Torrent schema

	list := e.inst.Do(t, "GET", "/api/v1/torrents", nil, nil)
	var all []torrent
	if list.Status != 200 || json.Unmarshal(list.Body, &all) != nil || len(all) != 1 || all[0].ID != tor.ID() {
		t.Errorf("GET /api/v1/torrents = %d %.300s, want a list containing %s", list.Status, list.Body, tor.ID())
	}

	dup := e.add(t, tor, dir)
	if dup.Status != 409 {
		t.Fatalf("adding the same torrent again = %d, want 409", dup.Status)
	}
	if err := spec.ValidateJSON("AlreadyExists", dup.Body); err != nil {
		t.Errorf("409 body: %v", err)
	}
	var ae struct{ ID string }
	json.Unmarshal(dup.Body, &ae)
	if ae.ID != tor.ID() {
		t.Errorf("409 body id = %q, want %s", ae.ID, tor.ID())
	}

	other := e.gen(t, "other.bin", 16384, f(1000))
	rel := e.add(t, other, "relative/dir")
	if rel.Status != 400 || !strings.Contains(string(rel.Body), "invalid_download_dir") {
		t.Errorf("relative downloadDir = %d %.200s, want 400 invalid_download_dir", rel.Status, rel.Body)
	}
	if err := spec.ValidateJSON("Error", rel.Body); err != nil {
		t.Errorf("400 body: %v", err)
	}
	if r := e.inst.Do(t, "GET", "/api/v1/torrents/"+strings.Repeat("ab", 20), nil, nil); r.Status != 404 {
		t.Errorf("GET unknown torrent = %d, want 404", r.Status)
	}
}

func TestAnnounceLifecycle(t *testing.T) {
	e := newEnv(t)
	tor := e.gen(t, "lifecycle", 32768, f(100000, "a.bin"), f(20000, "b.bin"))
	p := e.peer(t, tor, fakes.PeerOptions{})
	e.tracker.SetPeers(p.Addr())
	e.download(t, tor, 60*time.Second)

	ih := string(tor.InfoHash[:])
	started, ok := e.tracker.WaitFor(time.Second, func(a fakes.Announce) bool { return a.InfoHash == ih && a.Event == "started" })
	must(t, ok, "no announce with event=started")
	if started.Left != tor.Total {
		t.Errorf("started announce left=%d, want %d", started.Left, tor.Total)
	}
	done, ok := e.tracker.WaitFor(10*time.Second, func(a fakes.Announce) bool { return a.InfoHash == ih && a.Event == "completed" })
	must(t, ok, "no announce with event=completed after the download finished")
	if done.Left != 0 || done.Downloaded <= 0 {
		t.Errorf("completed announce left=%d downloaded=%d, want left=0 and downloaded>0", done.Left, done.Downloaded)
	}

	sent := time.Now()
	e.inst.Signal(syscall.SIGTERM)
	stopped, ok := e.tracker.WaitFor(5*time.Second, func(a fakes.Announce) bool { return a.InfoHash == ih && a.Event == "stopped" })
	must(t, ok, "no announce with event=stopped after SIGTERM")
	if d := stopped.Time.Sub(sent); d > 3500*time.Millisecond {
		t.Errorf("stopped announce took %s after SIGTERM (budget 3 s)", d)
	}
	if code, ok := e.inst.Wait(10 * time.Second); !ok || code != 0 {
		t.Errorf("exit after SIGTERM: code %d, exited %v", code, ok)
	}
}

func TestAnnounceParams(t *testing.T) {
	e := newEnv(t)
	tor := e.gen(t, "params.bin", 16384, f(50000))
	p := e.peer(t, tor, fakes.PeerOptions{})
	e.tracker.SetPeers(p.Addr())
	e.download(t, tor, 60*time.Second)

	as := e.tracker.Announces()
	must(t, len(as) >= 2, "expected at least the started and completed announces, got %d", len(as))
	for i, a := range as {
		if a.InfoHash != string(tor.InfoHash[:]) {
			t.Errorf("announce %d: info_hash %x, want %x", i, a.InfoHash, tor.InfoHash)
		}
		if len(a.PeerID) != 20 || !strings.HasPrefix(a.PeerID, "-TD0100-") {
			t.Errorf("announce %d: peer_id %q, want 20 bytes starting with -TD0100-", i, a.PeerID)
		}
		if a.Port != e.peerPort {
			t.Errorf("announce %d: port %d, want the --peer-port %d", i, a.Port, e.peerPort)
		}
		if a.Compact != "1" || a.NumWant != "50" {
			t.Errorf("announce %d: compact=%q numwant=%q, want 1 and 50", i, a.Compact, a.NumWant)
		}
		for _, k := range []string{"uploaded", "downloaded", "left"} {
			if _, ok := a.Query[k]; !ok {
				t.Errorf("announce %d: missing %s", i, k)
			}
		}
		if a.Key == "" || a.Key != as[0].Key || a.PeerID != as[0].PeerID {
			t.Errorf("announce %d: key %q / peer_id must be set and stay the same for the session", i, a.Key)
		}
	}
}

func TestNonCompactPeers(t *testing.T) {
	e := newEnv(t)
	e.tracker.SetCompact(false)
	tor := e.gen(t, "dict-peers.bin", 16384, f(60000))
	p := e.peer(t, tor, fakes.PeerOptions{})
	e.tracker.SetPeers(p.Addr())
	e.download(t, tor, 60*time.Second)
}

func TestHandshake(t *testing.T) {
	e := newEnv(t)
	tor := e.gen(t, "handshake.bin", 16384, f(30000))
	p := e.peer(t, tor, fakes.PeerOptions{})
	e.tracker.SetPeers(p.Addr())
	e.download(t, tor, 60*time.Second)
	st := p.Stats()
	must(t, len(st.BadHandshakes) == 0, "bad handshakes: %v", st.BadHandshakes)
	must(t, len(st.Handshakes) > 0, "torrentd never connected")
	h := st.Handshakes[0]
	if h.Pstr != "BitTorrent protocol" || h.InfoHash != tor.InfoHash || !bytes.HasPrefix(h.PeerID[:], []byte("-TD0100-")) {
		t.Errorf("handshake = %+v", h)
	}
	if as := e.tracker.Announces(); len(as) > 0 && string(h.PeerID[:]) != as[0].PeerID {
		t.Errorf("handshake peer_id %q differs from the announced %q", h.PeerID, as[0].PeerID)
	}
}

func TestOversizeMessageDisconnects(t *testing.T) {
	e := newEnv(t)
	tor := e.gen(t, "oversize.bin", 16384, f(80000))
	bad := e.peer(t, tor, fakes.PeerOptions{Oversize: true})
	good := e.peer(t, tor, fakes.PeerOptions{StallFrom: 1}) // hold the download until the bad peer is handled
	e.tracker.SetPeers(bad.Addr(), good.Addr())
	dir := filepath.Join(t.TempDir(), "d")
	e.mustAdd(t, tor, dir)
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if st := bad.Stats(); st.OversizeSent > 0 && st.ClosedByRemote > 0 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	st := bad.Stats()
	must(t, st.OversizeSent > 0, "torrentd never connected to the second tracker peer (connections: %d)", st.Connections)
	must(t, st.ClosedByRemote > 0, "torrentd did not disconnect a peer that sent a %d-byte message", 16384+9+200)
	good.Release()
	e.waitState(t, tor.ID(), "seeding", 60*time.Second)
	if err := tor.Verify(dir); err != nil {
		t.Fatal(err)
	}
}

func TestGarbageDoesNotCrash(t *testing.T) {
	e := newEnv(t)
	tor := e.gen(t, "garbage.bin", 16384, f(50000))
	var peers []netip.AddrPort
	var junk []*fakes.Peer
	for i := 0; i < 40; i++ {
		p := e.peer(t, tor, fakes.PeerOptions{Garbage: true, Seed: uint64(i + 1)})
		junk = append(junk, p)
		peers = append(peers, p.Addr())
	}
	good := e.peer(t, tor, fakes.PeerOptions{StallFrom: 1})
	e.tracker.SetPeers(append(peers, good.Addr())...)
	dir := filepath.Join(t.TempDir(), "d")
	e.mustAdd(t, tor, dir)
	deadline := time.Now().Add(30 * time.Second)
	conns := 0
	for time.Now().Before(deadline) {
		conns = 0
		for _, p := range junk {
			conns += p.Stats().Connections
		}
		if conns >= 40 {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	must(t, conns >= 20, "torrentd only connected to %d of 40 garbage peers in 30 s", conns)
	if r := e.inst.Do(t, "GET", "/healthz", nil, nil); r.Status != 200 {
		t.Fatalf("torrentd unhealthy after garbage: %d", r.Status)
	}
	good.Release()
	e.waitState(t, tor.ID(), "seeding", 60*time.Second)
	if err := tor.Verify(dir); err != nil {
		t.Fatal(err)
	}
}

func TestBlockRequests(t *testing.T) {
	e := newEnv(t)
	tor := e.gen(t, "blocks.bin", 262144, f(4<<20+12345))
	p := e.peer(t, tor, fakes.PeerOptions{Delay: 3 * time.Millisecond})
	e.tracker.SetPeers(p.Addr())
	e.download(t, tor, 120*time.Second)
	st := p.Stats()
	must(t, len(st.BadRequests) == 0, "invalid requests: %v", st.BadRequests)
	last := uint32(tor.NumPieces() - 1)
	for _, r := range st.Requests {
		isLastBlock := r.Index == last && int64(r.Begin)+int64(r.Length) == tor.PieceSize(int(last))
		if r.Length != 16384 && !isLastBlock {
			t.Errorf("request %+v: blocks must be 16 KiB (only the final block may be shorter)", r)
			break
		}
	}
	if st.MaxOutstanding < 5 || st.MaxOutstanding > 250 {
		t.Errorf("max outstanding requests = %d, want pipelining between 5 and 250", st.MaxOutstanding)
	}
}

func TestSpanningPieces(t *testing.T) {
	e := newEnv(t)
	tor := e.gen(t, "spanning", 32768,
		f(70000, "first.bin"), f(0, "empty.txt"), f(5000, "dir", "small.bin"),
		f(131079, "dir", "deeper", "big.bin"), f(33333, "last.bin"))
	p := e.peer(t, tor, fakes.PeerOptions{})
	e.tracker.SetPeers(p.Addr())
	e.download(t, tor, 60*time.Second)
}

func TestPartFiles(t *testing.T) {
	e := newEnv(t)
	tor := e.gen(t, "parts", 32768, f(65536, "a.bin"), f(65536, "b.bin"), f(65536, "c.bin"))
	p := e.peer(t, tor, fakes.PeerOptions{StallFrom: 4}) // pieces 4-5 are c.bin
	e.tracker.SetPeers(p.Addr())
	dir := filepath.Join(t.TempDir(), "d")
	e.mustAdd(t, tor, dir)
	root := filepath.Join(dir, "parts")
	exists := func(n string) bool { _, err := os.Stat(filepath.Join(root, n)); return err == nil }
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) && !(exists("a.bin") && exists("b.bin")) {
		time.Sleep(100 * time.Millisecond)
	}
	all, _ := files(dir)
	must(t, exists("a.bin") && exists("b.bin"), "finished files a.bin and b.bin should have their final names while c.bin is incomplete; on disk: %v", all)
	must(t, exists("c.bin.part") && !exists("c.bin"), "the unfinished file should be c.bin.part; on disk: %v", all)
	p.Release()
	e.waitState(t, tor.ID(), "seeding", 60*time.Second)
	all, _ = files(dir)
	for _, n := range all {
		if strings.HasSuffix(n, ".part") {
			t.Errorf("%s remains after completion", n)
		}
	}
	if err := tor.Verify(dir); err != nil {
		t.Fatal(err)
	}
}

func TestPadFilesNotCreated(t *testing.T) {
	e := newEnv(t)
	fx := filepath.Join(fixturesRoot(), "download", "hybrid")
	tor, err := torrentgen.Load(filepath.Join(fx, "hybrid.torrent"), filepath.Join(fx, "content"), e.tracker.URL())
	if err != nil {
		t.Fatal(err)
	}
	p := e.peer(t, tor, fakes.PeerOptions{})
	e.tracker.SetPeers(p.Addr())
	_, dir := e.download(t, tor, 60*time.Second) // Verify fails if a pad file exists
	all, _ := files(dir)
	for _, n := range all {
		if strings.Contains(n, ".pad") || strings.Contains(n, "padding") {
			t.Errorf("pad file on disk: %s", n)
		}
	}
}

func TestHashFailureRecovers(t *testing.T) {
	e := newEnv(t)
	tor := e.gen(t, "corrupt.bin", 32768, f(200000))
	p := e.peer(t, tor, fakes.PeerOptions{CorruptOnce: map[int]bool{2: true}})
	e.tracker.SetPeers(p.Addr())
	final, _ := e.download(t, tor, 60*time.Second)
	if final.WastedTotal < tor.PieceSize(2) {
		t.Errorf("wastedTotal = %d, want >= %d (the corrupted piece)", final.WastedTotal, tor.PieceSize(2))
	}
}

func TestOpenFileLimit(t *testing.T) {
	e := newEnv(t)
	var fs []torrentgen.File
	for i := 0; i < 300; i++ {
		fs = append(fs, f(3000, "many", "file-"+strconv.Itoa(i)+".bin"))
	}
	tor := e.gen(t, "many-files", 16384, fs...)
	p := e.peer(t, tor, fakes.PeerOptions{Delay: 20 * time.Millisecond})
	e.tracker.SetPeers(p.Addr())
	dir := filepath.Join(t.TempDir(), "d")
	os.MkdirAll(dir, 0o755)
	real, _ := filepath.EvalSymlinks(dir)
	e.mustAdd(t, tor, dir)

	maxOpen, samples := 0, 0
	deadline := time.Now().Add(120 * time.Second)
	for time.Now().Before(deadline) {
		out, _ := exec.Command("lsof", "-n", "-P", "-p", strconv.Itoa(e.inst.PID()), "-Fn").Output()
		n := 0
		for _, line := range strings.Split(string(out), "\n") {
			if strings.HasPrefix(line, "n") && (strings.HasPrefix(line[1:], real) || strings.HasPrefix(line[1:], dir)) {
				n++
			}
		}
		samples++
		maxOpen = max(maxOpen, n)
		if e.get(t, tor.ID()).State == "seeding" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	must(t, e.get(t, tor.ID()).State == "seeding", "download did not finish")
	must(t, samples >= 5, "download finished too fast to sample open files (%d samples)", samples)
	if maxOpen > 128 {
		t.Errorf("up to %d files under the download dir were open at once, want <= 128", maxOpen)
	}
	if err := tor.Verify(dir); err != nil {
		t.Fatal(err)
	}
}

func TestWritesStayInDownloadDir(t *testing.T) {
	e := newEnv(t)
	tor := e.gen(t, "contained", 16384, f(40000, "x.bin"), f(1000, "sub", "y.bin"))
	p := e.peer(t, tor, fakes.PeerOptions{})
	e.tracker.SetPeers(p.Addr())
	dir := filepath.Join(e.inst.Home, "downloads")
	got := e.mustAdd(t, tor, dir)
	e.waitState(t, got.ID, "seeding", 60*time.Second)
	all, _ := files(e.inst.Home)
	for _, n := range all {
		if !strings.HasPrefix(n, "downloads"+string(filepath.Separator)) && !strings.HasPrefix(n, "data"+string(filepath.Separator)) {
			t.Errorf("file created outside the download and data dirs: %s", n)
		}
	}
}

func fixturesRoot() string {
	return filepath.Join(repoRoot(), "harness", "fixtures")
}
