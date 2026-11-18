package discovery

import (
	"fmt"
	"math/rand/v2"
	"path/filepath"
	"testing"
	"time"

	"torrentd.local/harness/acceptance/internal/fakes"
	"torrentd.local/harness/acceptance/internal/torrentgen"
)

// dhtQueriesFor returns the DHT queries naming the torrent, at any node.
func dhtQueriesFor(nodes []*fakes.DHTNode, tor *torrentgen.Torrent, since time.Time) []string {
	var out []string
	for i, n := range nodes {
		for _, q := range n.Queries() {
			if q.Target == ihStr(tor) && q.Time.After(since) && (q.Method == "get_peers" || q.Method == "announce_peer") {
				out = append(out, fmt.Sprintf("%s to node %d at %s", q.Method, i, q.Time.Format("15:04:05.000")))
			}
		}
	}
	return out
}

func lsdFor(l *fakes.LSD, tor *torrentgen.Torrent, since time.Time) int {
	n := 0
	for _, a := range l.Announces() {
		if lsdHas(a, tor) && a.Time.After(since) && a.Cookie != "harness-lsd" {
			n++
		}
	}
	return n
}

// PRIV-01: a private .torrent uses only its own tracker (BEP 27, ADR-0020
// §5). In one instance with a working DHT and LSD, a public control torrent
// is looked up in the DHT and announced over LSD (and a peer announced for
// it over LSD is used), while for the private torrent, over 40 s: no
// get_peers/announce_peer, no LSD announce, no connection to a peer
// announced for it over LSD, and no ut_pex in its extended handshakes. The
// private torrent still downloads from its tracker's peer.
func TestPrivateTorrentNoDHTPEXLSD(t *testing.T) {
	t.Parallel()
	tr := httpTracker(t)
	pub := gen(t, "priv-control", torrentgen.Spec{})
	priv := gen(t, "priv", torrentgen.Spec{Announce: tr.URL(), Private: true})
	rng := rand.New(rand.NewPCG(101, 102))
	boot := dhtNode(t, fakes.RandomID(rng))
	e := newEnv(t, opts{Bootstrap: boot.Addr().String()})
	listener := lsd(t, e.inst.LSDGroup)
	trPeer := extPeer(t, priv, fakes.ExtPeerOptions{PEX: true})
	tr.SetPeers(trPeer.Addr())
	lsdPub := extPeer(t, pub, fakes.ExtPeerOptions{HasNone: true})
	lsdPriv := extPeer(t, priv, fakes.ExtPeerOptions{})

	start := time.Now()
	e.add(t, pub, "")
	dir := filepath.Join(t.TempDir(), "dl")
	e.add(t, priv, dir)

	if !eventually(30*time.Second, func() bool { return len(dhtQueriesFor([]*fakes.DHTNode{boot}, pub, start)) > 0 }) {
		fatalf(t, e, "control: no DHT get_peers for the public torrent within 30 s")
	}
	if !eventually(30*time.Second, func() bool { return lsdFor(listener, pub, start) > 0 }) {
		fatalf(t, e, "control: no LSD announce for the public torrent within 30 s")
	}
	if !eventually(30*time.Second, func() bool {
		listener.Send(int(lsdPub.Addr().Port()), "harness-lsd", pub.ID())
		listener.Send(int(lsdPriv.Addr().Port()), "harness-lsd", priv.ID())
		return lsdPub.WaitFor(2*time.Second, func(s fakes.ExtStats) bool { return connected(s, pub) })
	}) {
		fatalf(t, e, "control: torrentd did not connect to the LSD-announced peer of the public torrent within 30 s")
	}
	e.waitSeeding(t, priv.ID(), 60*time.Second)
	if err := priv.Verify(dir); err != nil {
		t.Fatal(err)
	}
	if x := e.get(t, priv.ID()); !x.Private {
		t.Errorf("private torrent reported private=false")
	}
	time.Sleep(time.Until(start.Add(40 * time.Second)))

	if q := dhtQueriesFor([]*fakes.DHTNode{boot}, priv, start); len(q) > 0 {
		t.Errorf("DHT traffic for the private torrent: %v", q)
	}
	if n := lsdFor(listener, priv, start); n > 0 {
		t.Errorf("%d LSD announces for the private torrent", n)
	}
	if connected(lsdPriv.Stats(), priv) {
		t.Errorf("torrentd connected to a peer announced over LSD for the private torrent")
	}
	st := trPeer.Stats()
	if len(st.ExtHandshakes) == 0 {
		t.Errorf("no extended handshake to the private torrent's tracker peer (BEP 10 is still used; only ut_pex is dropped)")
	}
	for _, d := range st.ExtHandshakes {
		info, _ := priv.InfoBytes()
		checkExtHandshake(t, "private torrent", e, d, len(info), false)
	}
	if len(st.PEX) > 0 {
		t.Errorf("torrentd sent %d ut_pex messages for the private torrent", len(st.PEX))
	}
}

// PRIV-02: G15/ADR-0007. A magnet (tr = a private tracker, no x.pe)
// resolves through the DHT: the only metadata source D is found via
// get_peers and also introduces a PEX peer P; a peer L is announced over
// LSD. D answers only once L and P had their chance to connect (or 15 s
// passed). The metadata says private=1. Within 10 s torrentd drops every
// connection to D, P and L and never reconnects them; there is no further
// get_peers/announce_peer or LSD announce for the info-hash and no
// connection to a peer newly announced over LSD; the download completes
// from the tracker's peer.
func TestPrivateMagnetLockdown(t *testing.T) {
	t.Parallel()
	tr := httpTracker(t)
	tor := gen(t, "priv-magnet", torrentgen.Spec{Announce: tr.URL(), Private: true})
	trPeer := extPeer(t, tor, fakes.ExtPeerOptions{})
	tr.SetPeers(trPeer.Addr())
	p := extPeer(t, tor, fakes.ExtPeerOptions{HasNone: true, PEX: true})
	l := extPeer(t, tor, fakes.ExtPeerOptions{HasNone: true, PEX: true})
	gate := make(chan struct{})
	d := extPeer(t, tor, fakes.ExtPeerOptions{ServeMetadata: true, HasNone: true, PEX: true, MetadataGate: gate,
		SendPEX: []fakes.PEXEntry{{Addr: p.Addr(), Flags: 0x10}}})
	network := dhtNetwork(t, tor.InfoHash, 8, 4, 3, 111, d.Addr())
	e := newEnv(t, opts{Bootstrap: network.Bootstrap().String()})
	listener := lsd(t, e.inst.LSDGroup)
	dir := filepath.Join(t.TempDir(), "dl")
	e.addMagnet(t, tor.Magnet([]string{tr.URL()}, nil), dir)

	if !d.WaitFor(30*time.Second, func(s fakes.ExtStats) bool { return connected(s, tor) }) {
		fatalf(t, e, "the magnet never reached its DHT peer within 30 s (get_peers seen: %v)", dhtQueriesFor(network.Nodes, tor, time.Time{}))
	}
	eventually(15*time.Second, func() bool {
		listener.Send(int(l.Addr().Port()), "harness-lsd", tor.ID())
		return l.WaitFor(time.Second, func(s fakes.ExtStats) bool { return connected(s, tor) }) &&
			connected(p.Stats(), tor)
	})
	usedL, usedP := connected(l.Stats(), tor), connected(p.Stats(), tor)
	t.Logf("during metadata: LSD peer connected=%v, PEX peer connected=%v (both optional, ADR-0007)", usedL, usedP)
	close(gate)
	x := e.waitFor(t, tor.ID(), 30*time.Second, "hasMetadata=true", func(x torrent) bool { return x.HasMetadata })
	resolved := time.Now()
	if !x.Private {
		t.Errorf("resolved magnet reports private=false; the info dict has private=1")
	}
	for name, peer := range map[string]*fakes.ExtPeer{"DHT peer D": d, "PEX peer P": p, "LSD peer L": l} {
		if !peer.WaitFor(10*time.Second, func(s fakes.ExtStats) bool { return s.Open == 0 }) {
			t.Errorf("%s still connected 10 s after the metadata showed private=1 (G15)", name)
		}
	}
	conns := map[*fakes.ExtPeer]int{d: d.Stats().Connections, p: p.Stats().Connections, l: l.Stats().Connections}
	late := extPeer(t, tor, fakes.ExtPeerOptions{})
	settle := time.Now().Add(2 * time.Second)
	for time.Now().Before(resolved.Add(25 * time.Second)) {
		listener.Send(int(late.Addr().Port()), "harness-lsd", tor.ID())
		time.Sleep(3 * time.Second)
	}
	e.waitSeeding(t, tor.ID(), 60*time.Second)
	if err := tor.Verify(dir); err != nil {
		t.Fatal(err)
	}
	for peer, n := range conns {
		if peer.Stats().Connections > n {
			t.Errorf("torrentd reconnected to a dht/pex/lsd peer after the torrent turned out private")
		}
	}
	if connected(late.Stats(), tor) {
		t.Errorf("torrentd connected to a peer announced over LSD after the torrent turned out private")
	}
	if q := dhtQueriesFor(network.Nodes, tor, settle); len(q) > 0 {
		t.Errorf("DHT traffic for the info-hash after it turned out private: %v", q)
	}
	if n := lsdFor(listener, tor, settle); n > 0 {
		t.Errorf("%d LSD announces for the info-hash after it turned out private", n)
	}
	if trPeer.Stats().BlocksSent == 0 {
		t.Errorf("nothing was downloaded from the tracker's peer")
	}
}
