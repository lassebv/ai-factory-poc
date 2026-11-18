package fakes

import (
	"encoding/binary"
	"math/rand/v2"
	"net"
	"net/netip"
	"sync"
	"time"
)

// BEP 15 constants.
const (
	udpProtocolID   = 0x41727101980
	UDPConnect      = 0
	UDPAnnounce     = 1
	UDPScrape       = 2
	udpError        = 3
	UDPEventNone    = 0
	UDPEventDone    = 1 // completed
	UDPEventStarted = 2
	UDPEventStopped = 3
)

// UDPPacket is one request the fake UDP tracker received.
type UDPPacket struct {
	Time       time.Time
	From       netip.AddrPort
	Action     int32
	TxID       uint32
	ConnID     uint64 // connection id used (announce/scrape)
	ConnAge    time.Duration
	KnownConn  bool // ConnID was issued by this tracker
	BadMagic   bool // connect without the BEP 15 protocol id
	Dropped    bool // the tracker did not answer this packet
	InfoHash   [20]byte
	InfoHashes [][20]byte // scrape
	PeerID     [20]byte
	Downloaded int64
	Left       int64
	Uploaded   int64
	Event      int32
	Key        uint32
	NumWant    int32
	Port       uint16
	Len        int
}

// UDPTracker is a BEP 15 tracker on 127.0.0.1. It hands out the peers the
// test chose and records every request.
type UDPTracker struct {
	conn *net.UDPConn

	mu           sync.Mutex
	packets      []UDPPacket
	conns        map[uint64]time.Time
	peers        []netip.AddrPort
	interval     int32
	seeders      int32
	leechers     int32
	scrape       [3]int32 // seeders, completed, leechers
	dropConnects int
	dropAll      bool
	wrongTxFirst bool
	rng          *rand.Rand
	notify       chan struct{}
}

// NewUDPTracker starts a UDP tracker on 127.0.0.1:0.
func NewUDPTracker() (*UDPTracker, error) {
	c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		return nil, err
	}
	t := &UDPTracker{conn: c, conns: map[uint64]time.Time{}, interval: 1800, seeders: 1,
		rng: rand.New(rand.NewPCG(uint64(time.Now().UnixNano()), 15)), notify: make(chan struct{}, 1)}
	go t.loop()
	return t, nil
}

// URL is the announce URL (udp://127.0.0.1:port/announce).
func (t *UDPTracker) URL() string { return "udp://" + t.conn.LocalAddr().String() + "/announce" }

// Addr is the tracker's address.
func (t *UDPTracker) Addr() netip.AddrPort { return t.conn.LocalAddr().(*net.UDPAddr).AddrPort() }

// Close stops the tracker.
func (t *UDPTracker) Close() { t.conn.Close() }

// SetPeers sets the peers returned to every announce.
func (t *UDPTracker) SetPeers(p ...netip.AddrPort) { t.mu.Lock(); t.peers = p; t.mu.Unlock() }

// SetInterval sets the announce interval (seconds).
func (t *UDPTracker) SetInterval(s int32) { t.mu.Lock(); t.interval = s; t.mu.Unlock() }

// SetSwarm sets the seeders/leechers counts in announce responses.
func (t *UDPTracker) SetSwarm(seeders, leechers int32) {
	t.mu.Lock()
	t.seeders, t.leechers = seeders, leechers
	t.mu.Unlock()
}

// SetScrape sets the counts returned for every scraped info-hash.
func (t *UDPTracker) SetScrape(seeders, completed, leechers int32) {
	t.mu.Lock()
	t.scrape = [3]int32{seeders, completed, leechers}
	t.mu.Unlock()
}

// DropConnects makes the tracker ignore the next n connect requests.
func (t *UDPTracker) DropConnects(n int) { t.mu.Lock(); t.dropConnects = n; t.mu.Unlock() }

// SetDropAll makes the tracker ignore (but record) every packet.
func (t *UDPTracker) SetDropAll(v bool) { t.mu.Lock(); t.dropAll = v; t.mu.Unlock() }

// SetWrongTxFirst makes the tracker send, before every real reply, a reply of
// the same action with a different transaction id and a bogus payload. A
// client that matches transaction ids ignores it.
func (t *UDPTracker) SetWrongTxFirst(v bool) { t.mu.Lock(); t.wrongTxFirst = v; t.mu.Unlock() }

// Packets returns a copy of every request so far.
func (t *UDPTracker) Packets() []UDPPacket {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]UDPPacket(nil), t.packets...)
}

// WaitFor waits until pred holds for some packet, or the timeout expires.
func (t *UDPTracker) WaitFor(timeout time.Duration, pred func(UDPPacket) bool) (UDPPacket, bool) {
	deadline := time.Now().Add(timeout)
	for {
		for _, p := range t.Packets() {
			if pred(p) {
				return p, true
			}
		}
		if time.Now().After(deadline) {
			return UDPPacket{}, false
		}
		select {
		case <-t.notify:
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func (t *UDPTracker) loop() {
	buf := make([]byte, 2048)
	for {
		n, from, err := t.conn.ReadFromUDPAddrPort(buf)
		if err != nil {
			return
		}
		t.handle(append([]byte(nil), buf[:n]...), from)
		select {
		case t.notify <- struct{}{}:
		default:
		}
	}
}

func (t *UDPTracker) send(to netip.AddrPort, b []byte) { t.conn.WriteToUDPAddrPort(b, to) }

func (t *UDPTracker) errorReply(to netip.AddrPort, tx uint32, msg string) {
	b := binary.BigEndian.AppendUint32(nil, udpError)
	b = binary.BigEndian.AppendUint32(b, tx)
	t.send(to, append(b, msg...))
}

func (t *UDPTracker) handle(b []byte, from netip.AddrPort) {
	if len(b) < 16 {
		return
	}
	p := UDPPacket{Time: time.Now(), From: from, Len: len(b)}
	first := binary.BigEndian.Uint64(b)
	p.Action = int32(binary.BigEndian.Uint32(b[8:]))
	p.TxID = binary.BigEndian.Uint32(b[12:])

	t.mu.Lock()
	defer func() {
		t.packets = append(t.packets, p)
		t.mu.Unlock()
	}()
	drop := t.dropAll
	if p.Action == UDPConnect && t.dropConnects > 0 && !drop {
		t.dropConnects--
		drop = true
	}
	p.Dropped = drop

	if p.Action != UDPConnect {
		p.ConnID = first
		issued, ok := t.conns[first]
		p.KnownConn = ok
		if ok {
			p.ConnAge = p.Time.Sub(issued)
		}
	}
	switch p.Action {
	case UDPConnect:
		p.BadMagic = first != udpProtocolID
		if drop || p.BadMagic {
			return
		}
		id := t.rng.Uint64()
		t.conns[id] = p.Time
		if t.wrongTxFirst {
			bogus := binary.BigEndian.AppendUint32(nil, UDPConnect)
			bogus = binary.BigEndian.AppendUint32(bogus, p.TxID^0x5a5a5a5a)
			t.send(from, binary.BigEndian.AppendUint64(bogus, 0xdeadbeef))
		}
		r := binary.BigEndian.AppendUint32(nil, UDPConnect)
		r = binary.BigEndian.AppendUint32(r, p.TxID)
		t.send(from, binary.BigEndian.AppendUint64(r, id))
	case UDPAnnounce:
		if len(b) < 98 {
			return
		}
		copy(p.InfoHash[:], b[16:36])
		copy(p.PeerID[:], b[36:56])
		p.Downloaded = int64(binary.BigEndian.Uint64(b[56:]))
		p.Left = int64(binary.BigEndian.Uint64(b[64:]))
		p.Uploaded = int64(binary.BigEndian.Uint64(b[72:]))
		p.Event = int32(binary.BigEndian.Uint32(b[80:]))
		p.Key = binary.BigEndian.Uint32(b[88:])
		p.NumWant = int32(binary.BigEndian.Uint32(b[92:]))
		p.Port = binary.BigEndian.Uint16(b[96:])
		if drop {
			return
		}
		// BEP 15: a tracker accepts a connection id for 2 minutes.
		if !p.KnownConn || p.ConnAge > 2*time.Minute {
			t.errorReply(from, p.TxID, "connection id expired or unknown")
			return
		}
		if t.wrongTxFirst {
			bogus := binary.BigEndian.AppendUint32(nil, UDPAnnounce)
			bogus = binary.BigEndian.AppendUint32(bogus, p.TxID^0x5a5a5a5a)
			bogus = binary.BigEndian.AppendUint32(bogus, 5)
			bogus = binary.BigEndian.AppendUint32(bogus, 0)
			bogus = binary.BigEndian.AppendUint32(bogus, 0)
			t.send(from, append(bogus, 10, 255, 255, 1, 0, 1)) // a peer nobody should dial
		}
		r := binary.BigEndian.AppendUint32(nil, UDPAnnounce)
		r = binary.BigEndian.AppendUint32(r, p.TxID)
		r = binary.BigEndian.AppendUint32(r, uint32(t.interval))
		r = binary.BigEndian.AppendUint32(r, uint32(t.leechers))
		r = binary.BigEndian.AppendUint32(r, uint32(t.seeders))
		for _, pe := range t.peers {
			if pe.Addr().Is4() {
				ip := pe.Addr().As4()
				r = append(r, ip[:]...)
				r = binary.BigEndian.AppendUint16(r, pe.Port())
			}
		}
		t.send(from, r)
	case UDPScrape:
		for i := 16; i+20 <= len(b); i += 20 {
			var h [20]byte
			copy(h[:], b[i:i+20])
			p.InfoHashes = append(p.InfoHashes, h)
		}
		if drop {
			return
		}
		if !p.KnownConn || p.ConnAge > 2*time.Minute {
			t.errorReply(from, p.TxID, "connection id expired or unknown")
			return
		}
		r := binary.BigEndian.AppendUint32(nil, UDPScrape)
		r = binary.BigEndian.AppendUint32(r, p.TxID)
		for range p.InfoHashes {
			for _, v := range t.scrape {
				r = binary.BigEndian.AppendUint32(r, uint32(v))
			}
		}
		t.send(from, r)
	default:
		if !drop {
			t.errorReply(from, p.TxID, "unknown action")
		}
	}
}
