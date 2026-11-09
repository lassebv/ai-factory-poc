// Command fakesrv runs the M5 harness fakes next to real libtorrent inside
// the oracle container (harness/oracle) and checks that they interoperate.
// Running both in one container keeps all traffic on its loopback (UDP and
// multicast behave as on the gate machine; no Docker NAT in between).
//
//	fakesrv udp|dht|magnet|pex
//
// It exits 0 and prints PASS when the scenario worked.
package main

import (
	"bufio"
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"torrentd.local/harness/acceptance/internal/fakes"
	"torrentd.local/harness/acceptance/internal/torrentgen"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Println("usage: fakesrv udp|dht|magnet|pex")
		os.Exit(2)
	}
	scenarios := map[string]func() error{"udp": udpScenario, "dht": dhtScenario, "magnet": magnetScenario, "pex": pexScenario}
	f, ok := scenarios[os.Args[1]]
	if !ok {
		fmt.Println("unknown scenario", os.Args[1])
		os.Exit(2)
	}
	if err := f(); err != nil {
		fmt.Println("FAIL:", err)
		os.Exit(1)
	}
	fmt.Println("PASS")
}

// lt is a running lt_m5.py process.
type lt struct {
	cmd   *exec.Cmd
	mu    sync.Mutex
	lines []string
}

func startLT(args ...string) (*lt, error) {
	cmd := exec.Command("python3", append([]string{"-u", "/oracle/lt_m5.py"}, args...)...)
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	l := &lt{cmd: cmd}
	go func() {
		sc := bufio.NewScanner(out)
		for sc.Scan() {
			fmt.Println("  lt:", sc.Text())
			l.mu.Lock()
			l.lines = append(l.lines, sc.Text())
			l.mu.Unlock()
		}
	}()
	return l, nil
}

func (l *lt) stop() { l.cmd.Process.Kill(); l.cmd.Wait() }

// wait returns the first line with the prefix, or an error after timeout.
func (l *lt) wait(prefix string, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		l.mu.Lock()
		for _, s := range l.lines {
			if s == prefix || strings.HasPrefix(s, prefix+" ") {
				l.mu.Unlock()
				return s, nil
			}
		}
		l.mu.Unlock()
		time.Sleep(100 * time.Millisecond)
	}
	return "", fmt.Errorf("libtorrent printed no %q within %s", prefix, timeout)
}

func freePort() int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func gen(name string, announce string, files ...torrentgen.File) (*torrentgen.Torrent, string, error) {
	dir, _ := os.MkdirTemp("", "oracle-")
	tor, err := torrentgen.Generate(filepath.Join(dir, "content"), torrentgen.Spec{
		Name: name, PieceLength: 16384, Files: files, Announce: announce, Seed: uint64(len(name)) * 31})
	if err != nil {
		return nil, "", err
	}
	path := filepath.Join(dir, "x.torrent")
	return tor, path, os.WriteFile(path, tor.Bytes, 0o644)
}

func emptyDir() string { d, _ := os.MkdirTemp("", "save-"); return d }

// udpScenario: libtorrent announces to and scrapes the fake UDP tracker,
// which sends a wrong-transaction-id decoy before every reply.
func udpScenario() error {
	tr, err := fakes.NewUDPTracker()
	if err != nil {
		return err
	}
	defer tr.Close()
	tr.SetPeers(netip.MustParseAddrPort("127.0.0.1:1111"), netip.MustParseAddrPort("127.0.0.1:2222"))
	tr.SetSwarm(5, 2)
	tr.SetScrape(7, 20, 3)
	tr.SetWrongTxFirst(true)
	tor, path, err := gen("udp", tr.URL(), torrentgen.File{Size: 100000})
	if err != nil {
		return err
	}
	port := freePort()
	l, err := startLT("udp", path, emptyDir(), fmt.Sprint(port))
	if err != nil {
		return err
	}
	defer l.stop()
	if s, err := l.wait("TRACKER_REPLY", 30*time.Second); err != nil || s != "TRACKER_REPLY 2" {
		return fmt.Errorf("announce: %q %v", s, err)
	}
	if s, err := l.wait("SCRAPE", 30*time.Second); err != nil || s != "SCRAPE 7 3" {
		return fmt.Errorf("scrape: %q %v", s, err)
	}
	var connect, announce, scrape bool
	for _, p := range tr.Packets() {
		switch p.Action {
		case fakes.UDPConnect:
			connect = connect || !p.BadMagic
		case fakes.UDPAnnounce:
			if p.InfoHash != tor.InfoHash || !p.KnownConn || p.Event != fakes.UDPEventStarted || int(p.Port) != port {
				return fmt.Errorf("announce packet %+v (want info-hash %x, port %d, event started)", p, tor.InfoHash, port)
			}
			announce = true
		case fakes.UDPScrape:
			if len(p.InfoHashes) != 1 || p.InfoHashes[0] != tor.InfoHash || !p.KnownConn {
				return fmt.Errorf("scrape packet %+v", p)
			}
			scrape = true
		}
	}
	if !connect || !announce || !scrape {
		return fmt.Errorf("connect=%v announce=%v scrape=%v", connect, announce, scrape)
	}
	return nil
}

// dhtScenario: libtorrent bootstraps from the fake network, walks it to the
// nodes holding peers, connects to the peer they return, and announces. A
// probe node then queries libtorrent's DHT the way the acceptance tests
// query torrentd's.
func dhtScenario() error {
	tor, path, err := gen("dht", "http://127.0.0.1:1/announce", torrentgen.File{Size: 200000})
	if err != nil {
		return err
	}
	seeder, err := fakes.NewExtPeer(tor, fakes.ExtPeerOptions{})
	if err != nil {
		return err
	}
	defer seeder.Close()
	network, err := fakes.NewDHTNetwork(tor.InfoHash, 20, 4, 3, 99, seeder.Addr())
	if err != nil {
		return err
	}
	defer network.Close()
	port := freePort()
	l, err := startLT("dht", path, emptyDir(), fmt.Sprint(port), network.Bootstrap().String())
	if err != nil {
		return err
	}
	defer l.stop()
	isGetPeers := func(q fakes.KRPCQuery) bool { return q.Method == "get_peers" && q.Target == string(tor.InfoHash[:]) }
	holders := func() []int {
		var out []int
		for _, i := range network.Queried(isGetPeers) {
			if i >= 17 {
				out = append(out, i)
			}
		}
		return out
	}
	deadline := time.Now().Add(90 * time.Second)
	for len(holders()) == 0 && time.Now().Before(deadline) {
		time.Sleep(200 * time.Millisecond)
	}
	fmt.Println("  nodes that got get_peers:", network.Queried(isGetPeers))
	if len(holders()) == 0 {
		return errors.New("libtorrent never reached the nodes holding peers")
	}
	if !seeder.WaitFor(60*time.Second, func(s fakes.ExtStats) bool { return len(s.Handshakes) > 0 }) {
		return errors.New("libtorrent did not connect to the peer the DHT returned")
	}
	var announced bool
	for wait := time.Now().Add(60 * time.Second); !announced && time.Now().Before(wait); time.Sleep(200 * time.Millisecond) {
		for _, n := range network.Nodes {
			for _, q := range n.Queries() {
				if q.Method == "announce_peer" && q.TokenOK {
					p, _ := q.Args["port"].(int64)
					ip, _ := q.Args["implied_port"].(int64)
					fmt.Printf("  announce_peer port=%d implied_port=%d\n", p, ip)
					announced = true
				}
			}
		}
	}
	if !announced {
		return errors.New("libtorrent sent no announce_peer with a valid token")
	}

	// Probe libtorrent's DHT server side.
	probe, err := fakes.NewDHTNode(fakes.RandomID(rand.New(rand.NewPCG(1, 2))))
	if err != nil {
		return err
	}
	defer probe.Close()
	ltAddr := netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), uint16(port))
	return probeDHT(probe, ltAddr, tor.InfoHash)
}

// probeDHT runs the same server-side queries as DHT-07 against a node.
func probeDHT(probe *fakes.DHTNode, to netip.AddrPort, infoHash [20]byte) error {
	r, err := probe.Query(to, "ping", nil, 5*time.Second)
	if err != nil || r["y"] != "r" {
		return fmt.Errorf("ping: %v %v", r, err)
	}
	if id, _ := r["r"].(map[string]any)["id"].(string); len(id) != 20 {
		return fmt.Errorf("ping: r.id has %d bytes", len(id))
	}
	r, err = probe.Query(to, "find_node", map[string]any{"target": string(infoHash[:])}, 5*time.Second)
	if err != nil || r["y"] != "r" {
		return fmt.Errorf("find_node: %v %v", r, err)
	}
	nodes, _ := r["r"].(map[string]any)["nodes"].(string)
	if _, err := fakes.ParseCompactNodes(nodes); err != nil {
		return fmt.Errorf("find_node: %v", err)
	}
	r, err = probe.Query(to, "get_peers", map[string]any{"info_hash": string(infoHash[:])}, 5*time.Second)
	if err != nil || r["y"] != "r" {
		return fmt.Errorf("get_peers: %v %v", r, err)
	}
	token, _ := r["r"].(map[string]any)["token"].(string)
	if token == "" {
		return fmt.Errorf("get_peers: no token in %v", r)
	}
	r, err = probe.Query(to, "announce_peer", map[string]any{"info_hash": string(infoHash[:]), "port": 6881, "token": "forged-token"}, 5*time.Second)
	if err != nil || r["y"] != "e" {
		return fmt.Errorf("announce_peer with a forged token: want an error, got %v %v", r, err)
	}
	r, err = probe.Query(to, "announce_peer", map[string]any{"info_hash": string(infoHash[:]), "port": 6881, "token": token}, 5*time.Second)
	if err != nil || r["y"] != "r" {
		return fmt.Errorf("announce_peer with the token: %v %v", r, err)
	}
	// libtorrent stores the announce; a second node now gets it back.
	other, _ := fakes.NewDHTNode(fakes.RandomID(rand.New(rand.NewPCG(3, 4))))
	defer other.Close()
	r, err = other.Query(to, "get_peers", map[string]any{"info_hash": string(infoHash[:])}, 5*time.Second)
	if err != nil {
		return err
	}
	vals, _ := r["r"].(map[string]any)["values"].([]any)
	want := netip.MustParseAddrPort("127.0.0.1:6881")
	for _, v := range vals {
		if s, _ := v.(string); s != "" {
			if ap, ok := fakes.ParseCompactPeer(s); ok && ap == want {
				fmt.Println("  probe: ping, find_node, get_peers, announce_peer (forged rejected, valid stored) all OK")
				return nil
			}
		}
	}
	return fmt.Errorf("get_peers after announce_peer: %v does not contain %v", vals, want)
}

// magnetScenario: libtorrent fetches multi-piece metadata from the fake
// ut_metadata peer, then the data. A peer serving corrupted metadata must
// not satisfy libtorrent.
func magnetScenario() error {
	var files []torrentgen.File
	for i := 0; i < 400; i++ {
		files = append(files, torrentgen.File{Path: []string{fmt.Sprintf("a-rather-long-directory-name-%03d", i/50), fmt.Sprintf("a-file-with-a-long-name-for-metadata-size-%03d.bin", i)}, Size: 1000 + int64(i)})
	}
	tor, _, err := gen("magnet", "http://127.0.0.1:1/announce", files...)
	if err != nil {
		return err
	}
	info, _ := tor.InfoBytes()
	fmt.Printf("  info dict: %d bytes (%d metadata pieces)\n", len(info), (len(info)+16383)/16384)

	bad, err := fakes.NewExtPeer(tor, fakes.ExtPeerOptions{ServeMetadata: true, CorruptMetadata: true, HasNone: true})
	if err != nil {
		return err
	}
	defer bad.Close()
	l, err := startLT("magnet", tor.Magnet(nil, nil), emptyDir(), fmt.Sprint(freePort()), bad.Addr().String())
	if err != nil {
		return err
	}
	if !bad.WaitFor(30*time.Second, func(s fakes.ExtStats) bool { return s.MetaDataSent >= 3 }) {
		l.stop()
		return fmt.Errorf("libtorrent did not fetch the corrupt metadata: %+v", bad.Stats())
	}
	if _, err := l.wait("METADATA", 5*time.Second); err == nil {
		l.stop()
		return errors.New("libtorrent accepted corrupted metadata: the corruption is not detectable")
	}
	l.stop()
	fmt.Println("  corrupt metadata: fetched and rejected by libtorrent")

	good, err := fakes.NewExtPeer(tor, fakes.ExtPeerOptions{ServeMetadata: true})
	if err != nil {
		return err
	}
	defer good.Close()
	l, err = startLT("magnet", tor.Magnet(nil, nil), emptyDir(), fmt.Sprint(freePort()), good.Addr().String())
	if err != nil {
		return err
	}
	defer l.stop()
	if _, err := l.wait("METADATA", 30*time.Second); err != nil {
		return fmt.Errorf("%v; peer stats %+v", err, good.Stats())
	}
	if _, err := l.wait("FINISHED", 60*time.Second); err != nil {
		return err
	}
	st := good.Stats()
	fmt.Printf("  good peer: metadata requests %v, blocks sent %d, ext handshake from libtorrent %v\n", st.MetaRequests, st.BlocksSent, st.ExtHandshakes)
	return nil
}

// pexScenario: (1) libtorrent learns a seeder from our ut_pex message and
// downloads from it; (2) libtorrent, seeding, sends ut_pex messages that our
// decoder understands.
func pexScenario() error {
	tor, path, err := gen("pex", "http://127.0.0.1:1/announce", torrentgen.File{Size: 300000})
	if err != nil {
		return err
	}
	seeder, err := fakes.NewExtPeer(tor, fakes.ExtPeerOptions{PEX: true})
	if err != nil {
		return err
	}
	defer seeder.Close()
	introducer, err := fakes.NewExtPeer(tor, fakes.ExtPeerOptions{HasNone: true, PEX: true,
		SendPEX: []fakes.PEXEntry{{Addr: seeder.Addr(), Flags: 0x12}}})
	if err != nil {
		return err
	}
	defer introducer.Close()
	leechPort := freePort()
	leech, err := startLT("leech", path, emptyDir(), fmt.Sprint(leechPort))
	if err != nil {
		return err
	}
	defer leech.stop()

	seedPort := freePort()
	seed, err := startLT("seed", path, tor.ContentDir, fmt.Sprint(seedPort))
	if err != nil {
		return err
	}
	defer seed.stop()

	time.Sleep(2 * time.Second)
	if err := introducer.Dial(fmt.Sprintf("127.0.0.1:%d", leechPort)); err != nil {
		return err
	}
	if _, err := seed.wait("SEEDING", 30*time.Second); err != nil {
		return err
	}
	var ps []*fakes.ExtPeer
	for i := 0; i < 3; i++ {
		p, err := fakes.NewExtPeer(tor, fakes.ExtPeerOptions{HasNone: true, PEX: true})
		if err != nil {
			return err
		}
		defer p.Close()
		if err := p.Dial(fmt.Sprintf("127.0.0.1:%d", seedPort)); err != nil {
			return err
		}
		ps = append(ps, p)
	}

	if _, err := leech.wait("FINISHED", 60*time.Second); err != nil {
		return fmt.Errorf("receive: libtorrent did not download from the peer our ut_pex introduced: %v (seeder %+v)", err, seeder.Stats())
	}
	fmt.Println("  receive: libtorrent connected to the PEX-introduced seeder and finished")

	if !ps[0].WaitFor(120*time.Second, func(s fakes.ExtStats) bool { return len(s.PEX) > 0 }) {
		return errors.New("send: no ut_pex message from libtorrent within 120 s")
	}
	for i, p := range ps {
		for _, m := range p.Stats().PEX {
			fmt.Printf("  send: peer %d got ut_pex added=%v dropped=%v err=%q raw keys=%v\n", i, m.Added, m.Dropped, m.Err, keys(m.Raw))
			if m.Err != "" {
				return fmt.Errorf("our decoder rejected libtorrent's ut_pex: %s", m.Err)
			}
		}
	}
	return nil
}

func keys(m map[string]any) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}
