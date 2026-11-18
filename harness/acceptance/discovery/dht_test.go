package discovery

import (
	"database/sql"
	"fmt"
	"math/rand/v2"
	"net/netip"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"torrentd.local/harness/acceptance/internal/app"
	"torrentd.local/harness/acceptance/internal/fakes"
	"torrentd.local/harness/acceptance/internal/torrentgen"
)

func dhtNetwork(t *testing.T, target [20]byte, size, fanout, holders int, seed uint64, peers ...netip.AddrPort) *fakes.DHTNetwork {
	t.Helper()
	n, err := fakes.NewDHTNetwork(target, size, fanout, holders, seed, peers...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(n.Close)
	return n
}

func respNodes(t *testing.T, r map[string]any) []fakes.NodeInfo {
	t.Helper()
	body, _ := r["r"].(map[string]any)
	s, _ := body["nodes"].(string)
	ns, err := fakes.ParseCompactNodes(s)
	if err != nil {
		t.Fatalf("find_node response: %v", err)
	}
	return ns
}

func flipBit(id [20]byte, bit int) [20]byte {
	id[bit/8] ^= 0x80 >> (bit % 8)
	return id
}

// DHT-01: the routing table has k=8 buckets that split. The bootstrap
// node answers torrentd's first query with 8 nodes near torrentd's id
// (sharing 12–19 leading bits) and 16 far ones (first bit differs); every
// node answers every query with all 24. torrentd's own-id lookup reaches the
// near nodes; then a torrent whose info-hash is on the far side makes its
// get_peers lookup reach the far ones. A single unsplit bucket of 8 would
// keep only one group. With splitting, find_node for a target next to
// torrentd's own id returns the near nodes, and for a far target 8 far
// nodes. No response has more than 8.
func TestDHTRoutingTableSplits(t *testing.T) {
	t.Parallel()
	rng := rand.New(rand.NewPCG(21, 22))
	boot := dhtNode(t, fakes.RandomID(rng))
	var far, near []*fakes.DHTNode
	for i := 0; i < 16; i++ {
		far = append(far, dhtNode(t, [20]byte{}))
	}
	for i := 0; i < 8; i++ {
		near = append(near, dhtNode(t, [20]byte{}))
	}
	var (
		once sync.Once
		self [20]byte
		all  []fakes.NodeInfo
	)
	contacts := func(q fakes.KRPCQuery) []fakes.NodeInfo {
		once.Do(func() {
			copy(self[:], q.ID)
			for _, n := range near {
				n.SetID(fakes.IDAtDistance(self, 12+len(all), rng))
				all = append(all, n.Info())
			}
			for _, n := range far {
				n.SetID(fakes.IDAtDistance(self, 0, rng))
				all = append(all, n.Info())
			}
		})
		return all
	}
	for _, n := range append(append([]*fakes.DHTNode{boot}, far...), near...) {
		n.SetQueryContacts(contacts)
	}
	e := newEnv(t, opts{Bootstrap: boot.Addr().String()})
	if _, ok := boot.WaitQuery(20*time.Second, func(q fakes.KRPCQuery) bool { return len(q.ID) == 20 }); !ok {
		fatalf(t, e, "the bootstrap node got no query with a 20-byte id within 20 s")
	}
	contacted := func(ns []*fakes.DHTNode) int {
		c := 0
		for _, n := range ns {
			if len(n.Queries()) > 0 {
				c++
			}
		}
		return c
	}
	if !eventually(20*time.Second, func() bool { return contacted(near) >= 6 }) {
		fatalf(t, e, "torrentd's bootstrap lookup contacted only %d of the 8 nodes closest to its own id within 20 s", contacted(near))
	}
	var tor *torrentgen.Torrent
	for i := 0; ; i++ {
		tor = gen(t, fmt.Sprintf("dht-far-%d", i), torrentgen.Spec{Files: []torrentgen.File{{Size: 50_000}}})
		if tor.InfoHash[0]&0x80 != self[0]&0x80 {
			break
		}
	}
	e.add(t, tor, "")
	if !eventually(20*time.Second, func() bool { return contacted(far) >= 8 }) {
		fatalf(t, e, "the get_peers lookup for a far info-hash contacted only %d of the 16 far nodes within 20 s", contacted(far))
	}
	time.Sleep(3 * time.Second)

	nearSet, farSet := map[netip.AddrPort]bool{}, map[netip.AddrPort]bool{}
	for _, n := range near {
		nearSet[n.Addr()] = true
	}
	for _, n := range far {
		farSet[n.Addr()] = true
	}
	probe := dhtNode(t, fakes.IDAtDistance(self, 0, rng))

	nearTarget := flipBit(self, 159)
	r, err := probe.Query(e.peerAddr(), "find_node", map[string]any{"target": string(nearTarget[:])}, 3*time.Second)
	if err != nil || r["y"] != "r" {
		fatalf(t, e, "find_node (near target): %v %v", r, err)
	}
	got := respNodes(t, r)
	nNear := 0
	for _, n := range got {
		if nearSet[n.Addr] {
			nNear++
		}
	}
	if len(got) > 8 {
		t.Errorf("find_node returned %d nodes, want at most k=8", len(got))
	}
	if nNear < 6 {
		t.Errorf("find_node for a target next to torrentd's own id returned %d of the 8 near nodes (%d nodes total): the bucket holding its own id must split instead of staying full of far nodes", nNear, len(got))
	}

	farTarget := fakes.IDAtDistance(self, 0, rng)
	r, err = probe.Query(e.peerAddr(), "find_node", map[string]any{"target": string(farTarget[:])}, 3*time.Second)
	if err != nil || r["y"] != "r" {
		fatalf(t, e, "find_node (far target): %v %v", r, err)
	}
	got = respNodes(t, r)
	nFar := 0 // nodes on the far side of torrentd's id (our far nodes, or the probe itself)
	for _, n := range got {
		if n.ID[0]&0x80 != self[0]&0x80 {
			nFar++
		}
	}
	if len(got) > 8 {
		t.Errorf("find_node returned %d nodes, want at most k=8", len(got))
	}
	if nFar < 8 {
		t.Errorf("find_node for a far target returned %d nodes from the far half (%d total), want 8: the far bucket should hold k=8 of the 16 far nodes", nFar, len(got))
	}
}

// DHT-02: an iterative lookup through 50 fake nodes: node k shares 3k
// leading bits with the info-hash and knows only nodes k+1…k+8, and only the
// 4 closest nodes know the seeder. Starting from node 0 (the bootstrap),
// torrentd walks the network, gets the seeder from get_peers, and
// downloads from it (its only source).
func TestDHTLookupFindsPeers(t *testing.T) {
	t.Parallel()
	tor := gen(t, "dht-lookup", torrentgen.Spec{})
	seeder := peer(t, tor, fakes.PeerOptions{})
	network := dhtNetwork(t, tor.InfoHash, 50, 8, 4, 31, seeder.Addr())
	e := newEnv(t, opts{Bootstrap: network.Bootstrap().String()})
	dir := filepath.Join(t.TempDir(), "dl")
	e.add(t, tor, dir)
	e.waitSeeding(t, tor.ID(), 90*time.Second)
	if err := tor.Verify(dir); err != nil {
		t.Fatal(err)
	}
	isGP := func(q fakes.KRPCQuery) bool { return q.Method == "get_peers" && q.Target == ihStr(tor) }
	asked := network.Queried(isGP)
	t.Logf("nodes that got get_peers: %v", asked)
	if len(asked) < 5 {
		t.Errorf("get_peers reached only nodes %v: want an iterative lookup over several hops", asked)
	}
	reachedHolder := false
	for _, i := range asked {
		reachedHolder = reachedHolder || i >= 46
	}
	if !reachedHolder {
		t.Errorf("no get_peers reached the nodes holding the peer (46–49)")
	}
	for _, n := range network.Nodes {
		for _, q := range n.Queries() {
			if int(q.From.Port()) != e.peerPort || len(q.ID) != 20 {
				t.Fatalf("KRPC %s from port %d with a %d-byte id: want the peer port %d (C5) and a 20-byte id", q.Method, q.From.Port(), len(q.ID), e.peerPort)
			}
		}
	}
}

// DHT-03: tokens. torrentd's get_peers answer carries a token; announce_peer
// with that token is accepted, announce_peer with a forged token gets a
// KRPC error and is not stored. (Rotation every 5 min is not observed: see
// VALIDATION.md.)
func TestDHTTokens(t *testing.T) {
	t.Parallel()
	rng := rand.New(rand.NewPCG(41, 42))
	e := newEnv(t, opts{})
	ih := fakes.RandomID(rng)
	a := dhtNode(t, fakes.RandomID(rng))
	r, err := a.Query(e.peerAddr(), "get_peers", map[string]any{"info_hash": string(ih[:])}, 5*time.Second)
	if err != nil || r["y"] != "r" {
		fatalf(t, e, "get_peers to the peer port: %v %v", r, err)
	}
	token, _ := r["r"].(map[string]any)["token"].(string)
	if token == "" {
		t.Fatalf("get_peers response has no token: %v", r)
	}
	r, err = a.Query(e.peerAddr(), "announce_peer", map[string]any{"info_hash": string(ih[:]), "port": 7002, "token": token + "x"}, 5*time.Second)
	if err != nil || r["y"] != "e" {
		t.Errorf("announce_peer with a forged token: got %v (%v), want a KRPC error (y=e)", r, err)
	}
	r, err = a.Query(e.peerAddr(), "announce_peer", map[string]any{"info_hash": string(ih[:]), "port": 7001, "token": token}, 5*time.Second)
	if err != nil || r["y"] != "r" {
		t.Fatalf("announce_peer with the token from get_peers: got %v (%v), want y=r", r, err)
	}
	b := dhtNode(t, fakes.RandomID(rng))
	vals := getPeersValues(t, e, b, ih)
	if !vals[netip.MustParseAddrPort("127.0.0.1:7001")] {
		t.Errorf("get_peers values %v lack the peer announced with a valid token (127.0.0.1:7001)", vals)
	}
	if vals[netip.MustParseAddrPort("127.0.0.1:7002")] {
		t.Errorf("get_peers values contain the peer announced with a forged token")
	}
}

func getPeersValues(t *testing.T, e *env, from *fakes.DHTNode, ih [20]byte) map[netip.AddrPort]bool {
	t.Helper()
	r, err := from.Query(e.peerAddr(), "get_peers", map[string]any{"info_hash": string(ih[:])}, 5*time.Second)
	if err != nil || r["y"] != "r" {
		fatalf(t, e, "get_peers: %v %v", r, err)
	}
	out := map[netip.AddrPort]bool{}
	vals, _ := r["r"].(map[string]any)["values"].([]any)
	for _, v := range vals {
		s, _ := v.(string)
		if ap, ok := fakes.ParseCompactPeer(s); ok {
			out[ap] = true
		} else {
			t.Errorf("get_peers value %q is not a 6-byte compact peer", s)
		}
	}
	return out
}

// DHT-07: torrentd answers KRPC on the peer port: ping and find_node
// return its 20-byte id (and compact nodes), get_peers returns peers stored
// by announce_peer, including implied_port (the sender's UDP port).
func TestDHTAnswersQueries(t *testing.T) {
	t.Parallel()
	rng := rand.New(rand.NewPCG(51, 52))
	e := newEnv(t, opts{})
	a := dhtNode(t, fakes.RandomID(rng))
	r, err := a.Query(e.peerAddr(), "ping", nil, 5*time.Second)
	if err != nil || r["y"] != "r" {
		fatalf(t, e, "ping to the peer port: %v %v", r, err)
	}
	id, _ := r["r"].(map[string]any)["id"].(string)
	if len(id) != 20 {
		t.Fatalf("ping response id has %d bytes, want 20", len(id))
	}
	if string(r["t"].(string)) == "" {
		t.Errorf("response has no transaction id")
	}
	r, err = a.Query(e.peerAddr(), "find_node", map[string]any{"target": string(a.ID[:])}, 5*time.Second)
	if err != nil || r["y"] != "r" {
		t.Fatalf("find_node: %v %v", r, err)
	}
	if rid, _ := r["r"].(map[string]any)["id"].(string); rid != id {
		t.Errorf("find_node answered with id %x, ping with %x", rid, id)
	}
	respNodes(t, r)

	ih := fakes.RandomID(rng)
	r, _ = a.Query(e.peerAddr(), "get_peers", map[string]any{"info_hash": string(ih[:])}, 5*time.Second)
	token, _ := r["r"].(map[string]any)["token"].(string)
	if r, err := a.Query(e.peerAddr(), "announce_peer", map[string]any{"info_hash": string(ih[:]), "port": 1, "implied_port": 1, "token": token}, 5*time.Second); err != nil || r["y"] != "r" {
		t.Fatalf("announce_peer with implied_port: %v %v", r, err)
	}
	b := dhtNode(t, fakes.RandomID(rng))
	vals := getPeersValues(t, e, b, ih)
	if !vals[a.Addr()] {
		t.Errorf("get_peers values %v lack %v (announce_peer with implied_port=1 uses the sender's UDP port)", vals, a.Addr())
	}
}

// DHT-08: torrentd announces its torrents: within 60 s of adding a
// (seeding) torrent it sends announce_peer for the info-hash, with a token
// it got from that node and its peer port (or implied_port), to the nodes
// closest to the info-hash.
func TestDHTAnnouncePeer(t *testing.T) {
	t.Parallel()
	tor := gen(t, "dht-announce", torrentgen.Spec{})
	network := dhtNetwork(t, tor.InfoHash, 12, 6, 3, 61)
	e := newEnv(t, opts{Bootstrap: network.Bootstrap().String()})
	e.add(t, tor, tor.ContentDir)
	e.waitSeeding(t, tor.ID(), 30*time.Second)
	var got []string
	if !eventually(60*time.Second, func() bool {
		got = nil
		for i, n := range network.Nodes {
			for _, q := range n.Queries() {
				if q.Method == "announce_peer" && q.Target == ihStr(tor) {
					port, _ := q.Args["port"].(int64)
					implied, _ := q.Args["implied_port"].(int64)
					got = append(got, fmt.Sprintf("node %d token_ok=%v port=%d implied=%d", i, q.TokenOK, port, implied))
				}
			}
		}
		for i := 9; i < 12; i++ {
			for _, p := range network.Nodes[i].Stored(tor.InfoHash) {
				if int(p.Port()) == e.peerPort {
					return true
				}
			}
		}
		return false
	}) {
		fatalf(t, e, "no valid announce_peer (token from get_peers, port = peer port %d) reached the 3 nodes closest to the info-hash within 60 s; announce_peer seen: %v", e.peerPort, got)
	}
}

// DHT-04: the node id and routing table persist (G3). After a graceful
// restart without any bootstrap node, torrentd uses the same node id and
// contacts nodes it learned before the restart. With dht_node_id_v4 deleted
// from the settings table, the stored table is discarded: a new id, and no
// contact with the old nodes.
func TestDHTPersistence(t *testing.T) {
	t.Parallel()
	rng := rand.New(rand.NewPCG(71, 72))
	boot := dhtNode(t, fakes.RandomID(rng))
	var known []*fakes.DHTNode
	var infos []fakes.NodeInfo
	for i := 0; i < 8; i++ {
		n := dhtNode(t, fakes.RandomID(rng))
		known = append(known, n)
		infos = append(infos, n.Info())
	}
	boot.SetStaticContacts(infos)
	e := newEnv(t, opts{Bootstrap: boot.Addr().String()})
	queriedKnown := func(since time.Time) (ids map[string]bool, count int) {
		ids = map[string]bool{}
		for _, n := range known {
			hit := false
			for _, q := range n.Queries() {
				if q.Time.After(since) {
					ids[q.ID] = true
					hit = true
				}
			}
			if hit {
				count++
			}
		}
		return ids, count
	}
	if !eventually(30*time.Second, func() bool { _, c := queriedKnown(time.Time{}); return c >= 4 }) {
		fatalf(t, e, "torrentd contacted fewer than 4 of the 8 nodes the bootstrap node returned within 30 s")
	}
	q, _ := boot.WaitQuery(time.Second, func(fakes.KRPCQuery) bool { return true })
	firstID := q.ID
	time.Sleep(3 * time.Second)

	e.stop(t)
	db, err := sql.Open("sqlite", "file:"+e.dbPath()+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	var idVal string
	if err := db.QueryRow(`SELECT value FROM settings WHERE key = 'dht_node_id_v4'`).Scan(&idVal); err != nil {
		t.Errorf("settings key dht_node_id_v4 after a graceful shutdown: %v (G3)", err)
	}
	var rows int
	db.QueryRow(`SELECT count(*) FROM dht_nodes`).Scan(&rows)
	db.Close()
	if rows == 0 {
		t.Errorf("dht_nodes is empty after a graceful shutdown (§6.2 step 3 flushes the routing table)")
	}

	noBoot := "TORRENTD_DHT_BOOTSTRAP=" + app.DiscardUDP()
	again := time.Now()
	e.start(t, noBoot)
	if !eventually(30*time.Second, func() bool { _, c := queriedKnown(again); return c >= 1 }) {
		fatalf(t, e, "after a restart with no bootstrap node, torrentd contacted none of the nodes from its stored routing table within 30 s")
	}
	ids, _ := queriedKnown(again)
	if !ids[firstID] || len(ids) != 1 {
		t.Errorf("node ids after restart %x, want only the persisted %x", keys(ids), firstID)
	}

	e.stop(t)
	rw, err := sql.Open("sqlite", "file:"+e.dbPath())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rw.Exec(`DELETE FROM settings WHERE key = 'dht_node_id_v4'`); err != nil {
		t.Fatal(err)
	}
	rw.Close()
	third := time.Now()
	e.start(t, noBoot)
	probe := dhtNode(t, fakes.RandomID(rng))
	r, err := probe.Query(e.peerAddr(), "ping", nil, 5*time.Second)
	if err != nil || r["y"] != "r" {
		fatalf(t, e, "ping after the restart: %v %v", r, err)
	}
	newID, _ := r["r"].(map[string]any)["id"].(string)
	if newID == firstID || len(newID) != 20 {
		t.Errorf("with dht_node_id_v4 deleted torrentd still uses id %x: a missing id must be regenerated", newID)
	}
	time.Sleep(15 * time.Second)
	known = append(known, boot)
	if _, c := queriedKnown(third); c > 0 {
		t.Errorf("with dht_node_id_v4 deleted, torrentd still contacted %d nodes of the old routing table: the stored table must be discarded (G3)", c)
	}
}

func keys(m map[string]bool) []string {
	var out []string
	for k := range m {
		out = append(out, fmt.Sprintf("%x", k))
	}
	sort.Strings(out)
	return out
}

// DHT-06: malformed UDP input on the peer port never stops the DHT. An
// unknown method first gets a KRPC error (BEP 5, 204 Method Unknown). Then
// 3,400 random, truncated, deeply nested and mistyped KRPC packets and fake
// tracker replies are sent from 127.0.0.1. torrentd may rate-limit or block
// that sender, but it keeps running: /healthz answers, its DHT still works
// outward (a torrent added afterwards is looked up at the bootstrap node),
// and its answers to a new node are well-formed.
func TestDHTMalformedPackets(t *testing.T) {
	t.Parallel()
	rng := rand.New(rand.NewPCG(81, 82))
	boot := dhtNode(t, fakes.RandomID(rng))
	e := newEnv(t, opts{Bootstrap: boot.Addr().String()})
	a := dhtNode(t, fakes.RandomID(rng))
	if r, err := a.Query(e.peerAddr(), "ping", nil, 5*time.Second); err != nil || r["y"] != "r" {
		fatalf(t, e, "ping before the garbage: %v %v", r, err)
	}
	if r, err := a.Query(e.peerAddr(), "no_such_method", nil, 5*time.Second); err != nil || r["y"] != "e" {
		t.Errorf("unknown KRPC method: got %v (%v), want an error response (BEP 5, 204 Method Unknown)", r, err)
	}
	id := string(a.ID[:])
	nested := ""
	for i := 0; i < 5000; i++ {
		nested += "l"
	}
	samples := [][]byte{
		[]byte("d1:ad2:id20:" + id + "e1:q4:ping1:t2:aa1:y1:q"), // truncated
		[]byte("d1:ad2:idi5ee1:q4:ping1:t2:aa1:y1:qe"),          // id is an int
		[]byte("d1:ad2:id20:" + id + "6:target3:abce1:q9:find_node1:t2:aa1:y1:qe"),
		[]byte("d1:ad2:id20:" + id + "e1:q9:get_peers1:t2:aa1:y1:qe"), // no info_hash
		[]byte("d1:ad2:id20:" + id + "e1:q4:ping1:y1:qe"),             // no t
		[]byte("d1:q4:ping1:t2:aa1:y1:qe"),                            // no a
		[]byte("d1:t2:aa1:y1:r1:rd2:id20:" + id + "ee"),               // unsolicited response
		[]byte("d1:t2:aa1:y1:ee"),                                     // error without e
		[]byte("d1:ad2:id20:" + id + "e1:q4:ping1:t999999999:aa1:y1:qe"),
		[]byte("d" + nested),
		[]byte("de"),
		[]byte("d1:y1:qe"),
		{0, 0, 0, 1, 0, 0, 0, 7, 1, 2, 3}, // looks like a UDP tracker announce reply
		{0, 0, 0, 0, 0, 0, 0, 9, 0, 0, 0, 0, 0, 0, 0, 1},
	}
	for round := 0; round < 100; round++ {
		for _, s := range samples {
			a.SendRaw(e.peerAddr(), s)
		}
		for i := 0; i < 20; i++ {
			junk := make([]byte, 1+rng.IntN(1400))
			for j := range junk {
				junk[j] = byte(rng.Uint32())
			}
			if rng.IntN(2) == 0 {
				junk[0] = 'd'
			}
			a.SendRaw(e.peerAddr(), junk)
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(time.Second)
	if resp := e.inst.Do(t, "GET", "/healthz", nil, nil); resp.Status != 200 {
		t.Fatalf("/healthz = %d after the garbage", resp.Status)
	}
	tor := gen(t, "dht-garbage", torrentgen.Spec{Files: []torrentgen.File{{Size: 50_000}}})
	since := time.Now()
	e.add(t, tor, "")
	if _, ok := boot.WaitQuery(30*time.Second, func(q fakes.KRPCQuery) bool { return q.Time.After(since) && q.Target == ihStr(tor) }); !ok {
		fatalf(t, e, "after the garbage, the DHT did not look up a newly added torrent at the bootstrap node within 30 s")
	}
	for _, q := range boot.Queries() {
		if len(q.ID) != 20 || q.TxID == "" {
			t.Errorf("malformed KRPC query from torrentd after the garbage: %+v", q)
		}
	}
}

// DHT-09: --dht-bootstrap replaces the bootstrap list and beats
// TORRENTD_DHT_BOOTSTRAP; an empty --dht-bootstrap means no bootstrap at
// all (the DHT still answers queries); the environment variable alone works.
func TestDHTBootstrapConfig(t *testing.T) {
	t.Parallel()
	rng := rand.New(rand.NewPCG(91, 92))
	byEnv, byFlag := dhtNode(t, fakes.RandomID(rng)), dhtNode(t, fakes.RandomID(rng))
	any := func(fakes.KRPCQuery) bool { return true }

	e := newEnv(t, opts{Bootstrap: byEnv.Addr().String(), Args: []string{"--dht-bootstrap", byFlag.Addr().String()}})
	if _, ok := byFlag.WaitQuery(20*time.Second, any); !ok {
		fatalf(t, e, "the --dht-bootstrap node got no query within 20 s")
	}
	if n := len(byEnv.Queries()); n > 0 {
		t.Errorf("the TORRENTD_DHT_BOOTSTRAP node got %d queries although --dht-bootstrap overrides it", n)
	}
	e.stop(t)

	before := len(byEnv.Queries())
	e2 := newEnv(t, opts{Bootstrap: byEnv.Addr().String(), Args: []string{"--dht-bootstrap="}})
	probe := dhtNode(t, fakes.RandomID(rng))
	if r, err := probe.Query(e2.peerAddr(), "ping", nil, 5*time.Second); err != nil || r["y"] != "r" {
		fatalf(t, e2, "with --dht-bootstrap= the DHT must still run and answer ping: %v %v", r, err)
	}
	time.Sleep(10 * time.Second)
	if n := len(byEnv.Queries()) - before; n > 0 {
		t.Errorf("with an empty --dht-bootstrap, torrentd still queried the TORRENTD_DHT_BOOTSTRAP node %d times", n)
	}
	e2.stop(t)

	e3 := newEnv(t, opts{Bootstrap: byEnv.Addr().String()})
	if _, ok := byEnv.WaitQuery(20*time.Second, func(q fakes.KRPCQuery) bool { return int(q.From.Port()) == e3.peerPort }); !ok {
		fatalf(t, e3, "TORRENTD_DHT_BOOTSTRAP alone: the node got no query within 20 s")
	}
}
