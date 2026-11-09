package fakes

import (
	"bytes"
	"crypto/sha1"
	"encoding/binary"
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"net/netip"
	"sort"
	"sync"
	"time"

	"torrentd.local/harness/acceptance/internal/benc"
)

// NodeInfo is a DHT contact: a node id and its UDP address.
type NodeInfo struct {
	ID   [20]byte
	Addr netip.AddrPort
}

// CompactNodes encodes contacts in BEP 5 compact form (26 bytes each, IPv4).
func CompactNodes(ns []NodeInfo) []byte {
	var b []byte
	for _, n := range ns {
		if !n.Addr.Addr().Is4() {
			continue
		}
		ip := n.Addr.Addr().As4()
		b = append(b, n.ID[:]...)
		b = append(b, ip[:]...)
		b = binary.BigEndian.AppendUint16(b, n.Addr.Port())
	}
	return b
}

// ParseCompactNodes decodes BEP 5 compact node info. It returns an error if
// the length is not a multiple of 26.
func ParseCompactNodes(s string) ([]NodeInfo, error) {
	if len(s)%26 != 0 {
		return nil, fmt.Errorf("compact nodes: length %d is not a multiple of 26", len(s))
	}
	var out []NodeInfo
	for i := 0; i < len(s); i += 26 {
		var n NodeInfo
		copy(n.ID[:], s[i:i+20])
		ip := netip.AddrFrom4([4]byte([]byte(s[i+20 : i+24])))
		n.Addr = netip.AddrPortFrom(ip, binary.BigEndian.Uint16([]byte(s[i+24:i+26])))
		out = append(out, n)
	}
	return out, nil
}

// CompactPeer encodes an IPv4 peer as 6 bytes.
func CompactPeer(p netip.AddrPort) []byte {
	ip := p.Addr().As4()
	return binary.BigEndian.AppendUint16(append([]byte{}, ip[:]...), p.Port())
}

// ParseCompactPeer decodes a 6-byte compact IPv4 peer.
func ParseCompactPeer(s string) (netip.AddrPort, bool) {
	if len(s) != 6 {
		return netip.AddrPort{}, false
	}
	return netip.AddrPortFrom(netip.AddrFrom4([4]byte([]byte(s[:4]))), binary.BigEndian.Uint16([]byte(s[4:]))), true
}

// Distance returns the XOR distance of a and b.
func Distance(a, b [20]byte) (d [20]byte) {
	for i := range a {
		d[i] = a[i] ^ b[i]
	}
	return d
}

// Closer reports whether a is closer to target than b.
func Closer(target, a, b [20]byte) bool {
	da, db := Distance(target, a), Distance(target, b)
	return bytes.Compare(da[:], db[:]) < 0
}

// KRPCQuery is one query a fake node received.
type KRPCQuery struct {
	Time   time.Time
	From   netip.AddrPort
	Method string
	TxID   string
	Args   map[string]any
	ID     string // a.id (raw)
	// Target is a.target (find_node) or a.info_hash (get_peers, announce_peer).
	Target string
	// TokenOK is set for announce_peer: the token was one this node handed out
	// to this address.
	TokenOK bool
}

// DHTNode is a fake DHT node on 127.0.0.1 speaking KRPC (BEP 5). It answers
// ping, find_node, get_peers and announce_peer, records every query, and can
// send queries itself (to probe torrentd's DHT).
type DHTNode struct {
	ID   [20]byte
	conn *net.UDPConn

	mu      sync.Mutex
	queries []KRPCQuery
	// Contacts returns the nodes this node reports for a target (find_node,
	// and get_peers when it has no values). Nil means "no nodes".
	contacts func(target [20]byte) []NodeInfo
	values   map[[20]byte][]netip.AddrPort
	stored   map[[20]byte][]netip.AddrPort // from announce_peer with a valid token
	pending  map[string]chan map[string]any
	nextTx   uint16
	silent   bool
	malform  int
	notify   chan struct{}
}

// NewDHTNode starts a fake node with the given id.
func NewDHTNode(id [20]byte) (*DHTNode, error) {
	c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		return nil, err
	}
	n := &DHTNode{ID: id, conn: c, values: map[[20]byte][]netip.AddrPort{}, stored: map[[20]byte][]netip.AddrPort{},
		pending: map[string]chan map[string]any{}, notify: make(chan struct{}, 1)}
	go n.loop()
	return n, nil
}

// Addr is the node's UDP address.
func (n *DHTNode) Addr() netip.AddrPort { return n.conn.LocalAddr().(*net.UDPAddr).AddrPort() }

// Info is the node's contact.
func (n *DHTNode) Info() NodeInfo { return NodeInfo{ID: n.ID, Addr: n.Addr()} }

// Close stops the node.
func (n *DHTNode) Close() { n.conn.Close() }

// SetContacts sets the function that picks the nodes this node reports.
func (n *DHTNode) SetContacts(f func(target [20]byte) []NodeInfo) {
	n.mu.Lock()
	n.contacts = f
	n.mu.Unlock()
}

// SetStaticContacts makes the node report the 8 contacts closest to the
// target out of ns.
func (n *DHTNode) SetStaticContacts(ns []NodeInfo) {
	n.SetContacts(func(target [20]byte) []NodeInfo { return ClosestK(target, ns, 8) })
}

// SetValues makes get_peers for infoHash return these peers.
func (n *DHTNode) SetValues(infoHash [20]byte, peers ...netip.AddrPort) {
	n.mu.Lock()
	n.values[infoHash] = peers
	n.mu.Unlock()
}

// SetSilent makes the node record queries without answering them.
func (n *DHTNode) SetSilent(v bool) { n.mu.Lock(); n.silent = v; n.mu.Unlock() }

// Stored returns the peers announced to this node (valid token) for a hash.
func (n *DHTNode) Stored(infoHash [20]byte) []netip.AddrPort {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]netip.AddrPort(nil), n.stored[infoHash]...)
}

// Queries returns a copy of every query received.
func (n *DHTNode) Queries() []KRPCQuery {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]KRPCQuery(nil), n.queries...)
}

// Malformed counts packets from others that were not valid KRPC.
func (n *DHTNode) Malformed() int { n.mu.Lock(); defer n.mu.Unlock(); return n.malform }

// ClosestK returns the k contacts closest to target.
func ClosestK(target [20]byte, ns []NodeInfo, k int) []NodeInfo {
	s := append([]NodeInfo(nil), ns...)
	sort.Slice(s, func(i, j int) bool { return Closer(target, s[i].ID, s[j].ID) })
	if len(s) > k {
		s = s[:k]
	}
	return s
}

func (n *DHTNode) token(from netip.AddrPort) string {
	h := sha1.Sum(append(n.ID[:], from.Addr().String()...))
	return string(h[:8])
}

func (n *DHTNode) loop() {
	buf := make([]byte, 65536)
	for {
		sz, from, err := n.conn.ReadFromUDPAddrPort(buf)
		if err != nil {
			return
		}
		n.handle(append([]byte(nil), buf[:sz]...), from)
		select {
		case n.notify <- struct{}{}:
		default:
		}
	}
}

func (n *DHTNode) send(to netip.AddrPort, m map[string]any) {
	n.conn.WriteToUDPAddrPort(benc.Encode(m), to)
}

func (n *DHTNode) handle(b []byte, from netip.AddrPort) {
	v, _, err := benc.Decode(b)
	m, ok := v.(map[string]any)
	if err != nil || !ok {
		n.mu.Lock()
		n.malform++
		n.mu.Unlock()
		return
	}
	tx, _ := m["t"].(string)
	y, _ := m["y"].(string)
	if y == "r" || y == "e" {
		n.mu.Lock()
		ch := n.pending[tx]
		delete(n.pending, tx)
		n.mu.Unlock()
		if ch != nil {
			ch <- m
		}
		return
	}
	if y != "q" {
		n.mu.Lock()
		n.malform++
		n.mu.Unlock()
		return
	}
	method, _ := m["q"].(string)
	args, _ := m["a"].(map[string]any)
	q := KRPCQuery{Time: time.Now(), From: from, Method: method, TxID: tx, Args: args}
	if args != nil {
		q.ID, _ = args["id"].(string)
		if s, ok := args["target"].(string); ok {
			q.Target = s
		}
		if s, ok := args["info_hash"].(string); ok {
			q.Target = s
		}
	}
	n.mu.Lock()
	silent, contactsFn := n.silent, n.contacts
	var target [20]byte
	copy(target[:], q.Target)
	resp := map[string]any{"id": string(n.ID[:])}
	errCode := 0
	switch method {
	case "ping":
	case "find_node":
		if contactsFn != nil {
			resp["nodes"] = CompactNodes(contactsFn(target))
		} else {
			resp["nodes"] = ""
		}
	case "get_peers":
		resp["token"] = n.token(from)
		if vals := append(n.values[target], n.stored[target]...); len(vals) > 0 {
			var list []any
			for _, p := range vals {
				list = append(list, CompactPeer(p))
			}
			resp["values"] = list
		}
		if contactsFn != nil {
			resp["nodes"] = CompactNodes(contactsFn(target))
		}
	case "announce_peer":
		tok, _ := args["token"].(string)
		q.TokenOK = tok == n.token(from)
		if !q.TokenOK {
			errCode = 203
			break
		}
		port, _ := args["port"].(int64)
		if ip, _ := args["implied_port"].(int64); ip == 1 {
			port = int64(from.Port())
		}
		n.stored[target] = append(n.stored[target], netip.AddrPortFrom(from.Addr(), uint16(port)))
	default:
		errCode = 204
	}
	n.queries = append(n.queries, q)
	n.mu.Unlock()
	if silent {
		return
	}
	if errCode != 0 {
		n.send(from, map[string]any{"t": tx, "y": "e", "e": []any{errCode, "fake node refused"}})
		return
	}
	n.send(from, map[string]any{"t": tx, "y": "r", "r": resp})
}

// ErrTimeout is returned by Query when no response arrives in time.
var ErrTimeout = errors.New("krpc: no response")

// Query sends a KRPC query to addr and waits for the response dict (y=r or
// y=e). The "id" argument is filled in with this node's id.
func (n *DHTNode) Query(to netip.AddrPort, method string, args map[string]any, timeout time.Duration) (map[string]any, error) {
	a := map[string]any{"id": string(n.ID[:])}
	for k, v := range args {
		a[k] = v
	}
	ch := make(chan map[string]any, 1)
	n.mu.Lock()
	n.nextTx++
	tx := string(binary.BigEndian.AppendUint16(nil, n.nextTx))
	n.pending[tx] = ch
	n.mu.Unlock()
	n.send(to, map[string]any{"t": tx, "y": "q", "q": method, "a": a})
	select {
	case m := <-ch:
		return m, nil
	case <-time.After(timeout):
		n.mu.Lock()
		delete(n.pending, tx)
		n.mu.Unlock()
		return nil, ErrTimeout
	}
}

// SendRaw sends arbitrary bytes from this node's socket.
func (n *DHTNode) SendRaw(to netip.AddrPort, b []byte) { n.conn.WriteToUDPAddrPort(b, to) }

// WaitQuery waits until pred holds for some received query.
func (n *DHTNode) WaitQuery(timeout time.Duration, pred func(KRPCQuery) bool) (KRPCQuery, bool) {
	deadline := time.Now().Add(timeout)
	for {
		for _, q := range n.Queries() {
			if pred(q) {
				return q, true
			}
		}
		if time.Now().After(deadline) {
			return KRPCQuery{}, false
		}
		select {
		case <-n.notify:
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// RandomID returns a deterministic pseudo-random node id.
func RandomID(rng *rand.Rand) (id [20]byte) {
	for i := range id {
		id[i] = byte(rng.Uint32())
	}
	return id
}

// IDAtDistance returns an id that shares exactly `prefix` leading bits with
// base (bit `prefix` differs), with the remaining bits random.
func IDAtDistance(base [20]byte, prefix int, rng *rand.Rand) [20]byte {
	id := RandomID(rng)
	for i := 0; i < 160; i++ {
		byteI, mask := i/8, byte(0x80>>(i%8))
		switch {
		case i < prefix:
			id[byteI] = id[byteI]&^mask | base[byteI]&mask
		case i == prefix:
			id[byteI] = id[byteI]&^mask | ^base[byteI]&mask
		}
	}
	return id
}

// DHTNetwork is a set of fake nodes arranged so that a lookup for Target
// needs many hops: node k shares 3k leading bits with Target and knows only
// nodes k+1 … k+Fanout. Node 0 is the bootstrap node; the last Holders nodes
// return Peers for get_peers(Target).
type DHTNetwork struct {
	Target [20]byte
	Nodes  []*DHTNode
}

// NewDHTNetwork builds and starts the network.
func NewDHTNetwork(target [20]byte, size, fanout, holders int, seed uint64, peers ...netip.AddrPort) (*DHTNetwork, error) {
	rng := rand.New(rand.NewPCG(seed, 5))
	net := &DHTNetwork{Target: target}
	for k := 0; k < size; k++ {
		node, err := NewDHTNode(IDAtDistance(target, min(3*k, 159), rng))
		if err != nil {
			net.Close()
			return nil, err
		}
		net.Nodes = append(net.Nodes, node)
	}
	for k, node := range net.Nodes {
		var known []NodeInfo
		for j := k + 1; j <= k+fanout && j < size; j++ {
			known = append(known, net.Nodes[j].Info())
		}
		node.SetStaticContacts(known)
		if k >= size-holders {
			node.SetValues(target, peers...)
		}
	}
	return net, nil
}

// Bootstrap is the address of node 0.
func (d *DHTNetwork) Bootstrap() netip.AddrPort { return d.Nodes[0].Addr() }

// Close stops every node.
func (d *DHTNetwork) Close() {
	for _, n := range d.Nodes {
		n.Close()
	}
}

// Queried returns the indexes of nodes that received at least one query
// matching pred.
func (d *DHTNetwork) Queried(pred func(KRPCQuery) bool) []int {
	var out []int
	for i, n := range d.Nodes {
		for _, q := range n.Queries() {
			if pred(q) {
				out = append(out, i)
				break
			}
		}
	}
	return out
}
