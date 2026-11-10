package discovery

import (
	"math/rand/v2"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"torrentd.local/harness/acceptance/internal/fakes"
	"torrentd.local/harness/acceptance/internal/torrentgen"
)

func isAnnounce(event int32) func(fakes.UDPPacket) bool {
	return func(p fakes.UDPPacket) bool { return p.Action == fakes.UDPAnnounce && p.Event == event }
}

// checkFromPeerPort fails if a UDP packet did not come from the peer port (C5).
func checkFromPeerPort(t *testing.T, e *env, ps []fakes.UDPPacket) {
	t.Helper()
	for _, p := range ps {
		if int(p.From.Port()) != e.peerPort {
			fatalf(t, e, "UDP tracker packet (action %d) came from port %d, want the peer port %d: DHT and UDP trackers share the socket bound to the peer port (C5, ADR-0020 §2)", p.Action, p.From.Port(), e.peerPort)
		}
	}
}

// UDP-01: a torrent whose only tracker is udp:// downloads from the peers
// that tracker returns. connect uses the BEP 15 protocol id; replies with a
// wrong transaction id are ignored; announces carry the info-hash, our peer
// id, the peer port and the right event (started, completed, stopped).
func TestUDPTrackerAnnounce(t *testing.T) {
	t.Parallel()
	tr := udpTracker(t)
	tr.SetWrongTxFirst(true)
	e := newEnv(t, opts{})
	tor := gen(t, "udp-announce", torrentgen.Spec{Announce: tr.URL()})
	seeder := peer(t, tor, fakes.PeerOptions{})
	tr.SetPeers(seeder.Addr())

	dir := filepath.Join(t.TempDir(), "dl")
	e.add(t, tor, dir)
	started, ok := tr.WaitFor(20*time.Second, isAnnounce(fakes.UDPEventStarted))
	if !ok {
		fatalf(t, e, "no UDP announce with event=started within 20 s; packets: %+v", tr.Packets())
	}
	e.waitSeeding(t, tor.ID(), 60*time.Second)
	if err := tor.Verify(dir); err != nil {
		t.Fatalf("downloaded data: %v", err)
	}
	done, ok := tr.WaitFor(15*time.Second, isAnnounce(fakes.UDPEventDone))
	if !ok {
		fatalf(t, e, "no UDP announce with event=completed within 15 s of seeding; packets: %+v", tr.Packets())
	}

	e.inst.Signal(syscall.SIGTERM)
	stopped, ok := tr.WaitFor(5*time.Second, isAnnounce(fakes.UDPEventStopped))
	if !ok {
		fatalf(t, e, "no UDP announce with event=stopped after SIGTERM")
	}
	e.inst.Wait(10 * time.Second)

	ps := tr.Packets()
	checkFromPeerPort(t, e, ps)
	for _, p := range ps {
		if p.Action == fakes.UDPConnect && p.BadMagic {
			t.Errorf("connect request without the BEP 15 protocol id 0x41727101980")
		}
		if p.Action == fakes.UDPAnnounce && !p.KnownConn {
			t.Errorf("announce with connection id %#x, which the tracker never issued (was a reply with a wrong transaction id accepted?)", p.ConnID)
		}
	}
	for name, a := range map[string]fakes.UDPPacket{"started": started, "completed": done, "stopped": stopped} {
		if a.InfoHash != tor.InfoHash {
			t.Errorf("%s announce: info_hash %x, want %x", name, a.InfoHash, tor.InfoHash)
		}
		if !strings.HasPrefix(string(a.PeerID[:]), "-TD0100-") {
			t.Errorf("%s announce: peer_id %q, want the -TD0100- prefix (D13)", name, a.PeerID)
		}
		if int(a.Port) != e.peerPort {
			t.Errorf("%s announce: port %d, want the peer port %d", name, a.Port, e.peerPort)
		}
		if a.Key != started.Key {
			t.Errorf("%s announce: key %#x differs from the started announce's %#x (one key per session)", name, a.Key, started.Key)
		}
	}
	if started.Left != tor.Total {
		t.Errorf("started announce: left=%d, want %d", started.Left, tor.Total)
	}
	if done.Left != 0 || done.Downloaded < tor.Total {
		t.Errorf("completed announce: left=%d downloaded=%d, want 0 and >= %d", done.Left, done.Downloaded, tor.Total)
	}
}

// UDP-02: at shutdown the UDP stopped announce is one attempt within the 3 s
// budget: with a tracker that never answers it, exactly one stopped packet
// is sent and torrentd still exits promptly.
func TestUDPStoppedAnnounceSingleAttempt(t *testing.T) {
	t.Parallel()
	tr := udpTracker(t)
	e := newEnv(t, opts{})
	tor := gen(t, "udp-stopped", torrentgen.Spec{Announce: tr.URL()})
	e.add(t, tor, "")
	if _, ok := tr.WaitFor(20*time.Second, isAnnounce(fakes.UDPEventStarted)); !ok {
		fatalf(t, e, "no UDP announce with event=started within 20 s")
	}
	time.Sleep(time.Second)
	tr.SetDropAll(true)
	sig := time.Now()
	e.inst.Signal(syscall.SIGTERM)
	code, ok := e.inst.Wait(10 * time.Second)
	if !ok {
		t.Fatal("torrentd did not exit within 10 s of SIGTERM with an unresponsive UDP tracker")
	}
	if code != 0 {
		t.Errorf("exit code %d, want 0", code)
	}
	time.Sleep(time.Second)
	var stops, connects []time.Duration
	for _, p := range tr.Packets() {
		if p.Time.Before(sig) {
			continue
		}
		switch {
		case p.Action == fakes.UDPConnect:
			connects = append(connects, p.Time.Sub(sig))
		case p.Action == fakes.UDPAnnounce && p.Event == fakes.UDPEventStopped:
			stops = append(stops, p.Time.Sub(sig))
		}
	}
	if len(stops) != 1 {
		t.Fatalf("stopped announce packets after SIGTERM: %d at %v (connects at %v); want exactly one attempt (D8)", len(stops), stops, connects)
	}
	if stops[0] > 3500*time.Millisecond {
		t.Errorf("the stopped announce left %v after SIGTERM; it must fit in the 3 s budget", stops[0])
	}
	if len(connects) > 0 {
		t.Errorf("connect requests after SIGTERM at %v: the connection id from the started announce was < 1 min old and must be reused", connects)
	}
}

// UDP-03: retransmission follows BEP 15: a connect that gets no answer is
// retried after 15 s, then after another 30 s (15·2ⁿ).
func TestUDPRetransmitBackoff(t *testing.T) {
	t.Parallel()
	tr := udpTracker(t)
	tr.DropConnects(2)
	e := newEnv(t, opts{})
	tor := gen(t, "udp-backoff", torrentgen.Spec{Announce: tr.URL()})
	e.add(t, tor, "")
	if _, ok := tr.WaitFor(70*time.Second, isAnnounce(fakes.UDPEventStarted)); !ok {
		fatalf(t, e, "no announce within 70 s although the third connect is answered; packets: %+v", tr.Packets())
	}
	var at []time.Time
	for _, p := range tr.Packets() {
		if p.Action == fakes.UDPConnect {
			at = append(at, p.Time)
		}
	}
	if len(at) < 3 {
		t.Fatalf("saw %d connect requests before the announce, want 3 (two dropped)", len(at))
	}
	g1, g2 := at[1].Sub(at[0]), at[2].Sub(at[1])
	t.Logf("retransmit gaps: %v, %v", g1, g2)
	if g1 < 14*time.Second || g1 > 20*time.Second {
		t.Errorf("first retransmission after %v, want 15 s (15·2⁰, D8)", g1)
	}
	if g2 < 28*time.Second || g2 > 38*time.Second {
		t.Errorf("second retransmission after %v, want 30 s (15·2¹, D8)", g2)
	}
	checkFromPeerPort(t, e, tr.Packets())
}

// UDP-04: a connection id is reused while it is younger than 1 minute (the
// scrape after the first announce needs no new connect) and is not used once
// it is older than 1 minute: the tracker has no peers; 62 s after the first
// connect a seeder connects in, the download completes, and the completed
// announce comes after a fresh connect.
func TestUDPConnectionIDExpiry(t *testing.T) {
	t.Parallel()
	tr := udpTracker(t)
	e := newEnv(t, opts{})
	tor := gen(t, "udp-expiry", torrentgen.Spec{Announce: tr.URL(), Files: []torrentgen.File{{Size: 300_000}}})
	seeder := extPeer(t, tor, fakes.ExtPeerOptions{})
	dir := filepath.Join(t.TempDir(), "dl")
	e.add(t, tor, dir)
	started, ok := tr.WaitFor(20*time.Second, isAnnounce(fakes.UDPEventStarted))
	if !ok {
		fatalf(t, e, "no started announce within 20 s")
	}
	scrape, ok := tr.WaitFor(30*time.Second, func(p fakes.UDPPacket) bool { return p.Action == fakes.UDPScrape })
	if !ok {
		fatalf(t, e, "no UDP scrape within 30 s of the first announce (A10)")
	}
	if scrape.ConnID != started.ConnID {
		t.Errorf("the scrape %v after the announce used a different connection id: a connection id is valid for 1 minute and should be reused", scrape.Time.Sub(started.Time))
	}
	firstConnect := started.Time.Add(-started.ConnAge)
	time.Sleep(time.Until(firstConnect.Add(62 * time.Second)))
	if err := seeder.Dial(e.peerAddr().String()); err != nil {
		t.Fatal(err)
	}
	e.waitSeeding(t, tor.ID(), 60*time.Second)
	done, ok := tr.WaitFor(20*time.Second, isAnnounce(fakes.UDPEventDone))
	if !ok {
		fatalf(t, e, "no completed announce within 20 s of seeding")
	}
	if err := tor.Verify(dir); err != nil {
		t.Fatalf("downloaded data: %v", err)
	}
	if done.ConnID == started.ConnID || done.ConnAge >= 60*time.Second {
		t.Errorf("completed announce used a connection id that was %v old (the started announce's: %v); ids expire after 1 minute", done.ConnAge, done.ConnID == started.ConnID)
	}
	var connects int
	for _, p := range tr.Packets() {
		if p.Action == fakes.UDPConnect && p.Time.Before(scrape.Time) {
			connects++
		}
	}
	if connects != 1 {
		t.Errorf("%d connect requests before the scrape, want 1 (reuse within 1 minute)", connects)
	}
}

// UDPMUX-01: one UDP socket on the peer port carries both the UDP tracker
// and DHT traffic; KRPC queries to the peer port are answered while tracker
// replies arrive on the same socket.
func TestSharedUDPSocket(t *testing.T) {
	t.Parallel()
	rng := rand.New(rand.NewPCG(11, 12))
	boot := dhtNode(t, fakes.RandomID(rng))
	tr := udpTracker(t)
	e := newEnv(t, opts{Bootstrap: boot.Addr().String()})
	tor := gen(t, "udp-mux", torrentgen.Spec{Announce: tr.URL()})
	seeder := peer(t, tor, fakes.PeerOptions{})
	tr.SetPeers(seeder.Addr())
	dir := filepath.Join(t.TempDir(), "dl")
	e.add(t, tor, dir)

	q, ok := boot.WaitQuery(20*time.Second, func(fakes.KRPCQuery) bool { return true })
	if !ok {
		fatalf(t, e, "the DHT bootstrap node got no query within 20 s")
	}
	if int(q.From.Port()) != e.peerPort {
		t.Errorf("KRPC query came from port %d, want the peer port %d (C5)", q.From.Port(), e.peerPort)
	}
	probe := dhtNode(t, fakes.RandomID(rng))
	for i := 0; i < 20; i++ {
		r, err := probe.Query(e.peerAddr(), "ping", nil, 3*time.Second)
		if err != nil || r["y"] != "r" {
			fatalf(t, e, "ping %d to the peer port: %v %v", i, r, err)
		}
		id, _ := r["r"].(map[string]any)["id"].(string)
		if id != q.ID {
			t.Fatalf("ping answered with node id %x, but torrentd's queries use %x", id, q.ID)
		}
	}
	e.waitSeeding(t, tor.ID(), 60*time.Second)
	checkFromPeerPort(t, e, tr.Packets())
}
