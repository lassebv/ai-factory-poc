package fakes

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/netip"
	"sync"
	"time"

	"torrentd.local/harness/acceptance/internal/benc"
	"torrentd.local/harness/acceptance/internal/torrentgen"
)

// Extension message ids this fake advertises in its BEP 10 handshake. They
// are deliberately not the common 1/2/3, so a client that hard-codes ids
// instead of using the peer's `m` dictionary is caught.
const (
	ExtPEXID      = 7
	ExtMetadataID = 9
)

// metadataPiece is the BEP 9 piece size.
const metadataPiece = 16 * 1024

// PEXEntry is one peer in a ut_pex message.
type PEXEntry struct {
	Addr  netip.AddrPort
	Flags byte
}

// PEXMsg is one ut_pex message torrentd sent.
type PEXMsg struct {
	Time    time.Time
	Conn    int // connection number (1-based)
	Added   []PEXEntry
	Dropped []netip.AddrPort
	Raw     map[string]any
	Err     string // decoding problem, if any
}

// ExtPeerOptions scripts an ExtPeer.
type ExtPeerOptions struct {
	// NoExtensions clears reserved bit 20 (a peer without BEP 10).
	NoExtensions bool
	// Pieces this peer has and serves. Nil means all; use HasNone for none.
	Has     []int
	HasNone bool
	// EarlyBitfield sends the bitfield right after the BitTorrent handshake,
	// before the extended handshake (and so possibly before torrentd has the
	// metadata of a magnet, T6).
	EarlyBitfield bool
	// BitfieldExtraBytes appends bytes to the bitfield (wrong length).
	BitfieldExtraBytes int

	// ServeMetadata advertises ut_metadata and metadata_size and serves the
	// info dict. CorruptMetadata serves it with one byte flipped.
	ServeMetadata   bool
	CorruptMetadata bool
	// MetadataSize, when > 0, overrides the advertised metadata_size (the
	// peer then rejects every request it can't serve).
	MetadataSize int
	// MetadataGate, when set, delays every metadata answer until it is closed.
	MetadataGate chan struct{}

	// PEX advertises ut_pex. SendPEX is sent once torrentd's extended
	// handshake has arrived (only if torrentd advertised ut_pex).
	PEX     bool
	SendPEX []PEXEntry

	// Listen is the listen address (default 127.0.0.1:0).
	Listen string
	// PeerID (20 bytes) defaults to "-FX0001-" + digits.
	PeerID string
	// ListenPort is the `p` this peer advertises (default: its listen port).
	ListenPort int
}

// ExtStats is what an ExtPeer observed across all its connections.
type ExtStats struct {
	Connections    int
	Open           int // connections currently open
	Handshakes     []Handshake
	ExtHandshakes  []map[string]any // torrentd's extended handshakes (decoded)
	ExtHSTimes     []time.Time
	MetaRequests   []int // ut_metadata piece indexes torrentd requested
	MetaDataSent   int   // ut_metadata data messages sent
	MetaRejects    int   // ut_metadata rejects sent
	PEX            []PEXMsg
	Requests       int // block requests
	BlocksSent     int
	Interested     int
	MsgCounts      map[byte]int // every message id torrentd sent
	ClosedByRemote []time.Time  // torrentd closed a connection
	BadMessages    []string
}

// ExtPeer is a scriptable BEP 3 + BEP 10 peer for one torrent. It listens,
// and can also dial torrentd (an incoming connection from torrentd's view).
type ExtPeer struct {
	t    *torrentgen.Torrent
	info []byte
	opts ExtPeerOptions
	ln   net.Listener

	mu    sync.Mutex
	st    ExtStats
	conns []net.Conn
	notify chan struct{}
}

// NewExtPeer starts a peer listening on 127.0.0.1.
func NewExtPeer(t *torrentgen.Torrent, opts ExtPeerOptions) (*ExtPeer, error) {
	addr := opts.Listen
	if addr == "" {
		addr = "127.0.0.1:0"
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	info, err := t.InfoBytes()
	if err != nil {
		ln.Close()
		return nil, err
	}
	p := &ExtPeer{t: t, info: info, opts: opts, ln: ln, notify: make(chan struct{}, 1)}
	p.st.MsgCounts = map[byte]int{}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go p.serve(c, false)
		}
	}()
	return p, nil
}

// Addr is the listen address.
func (p *ExtPeer) Addr() netip.AddrPort { return p.ln.Addr().(*net.TCPAddr).AddrPort() }

// Dial connects to torrentd at addr and runs the same script.
func (p *ExtPeer) Dial(addr string) error {
	c, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		return err
	}
	go p.serve(c, true)
	return nil
}

// Close stops listening and drops every connection.
func (p *ExtPeer) Close() {
	p.ln.Close()
	p.mu.Lock()
	for _, c := range p.conns {
		c.Close()
	}
	p.mu.Unlock()
}

// Stats returns a snapshot.
func (p *ExtPeer) Stats() ExtStats {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := p.st
	s.Handshakes = append([]Handshake(nil), s.Handshakes...)
	s.ExtHandshakes = append([]map[string]any(nil), s.ExtHandshakes...)
	s.ExtHSTimes = append([]time.Time(nil), s.ExtHSTimes...)
	s.MetaRequests = append([]int(nil), s.MetaRequests...)
	s.PEX = append([]PEXMsg(nil), s.PEX...)
	s.ClosedByRemote = append([]time.Time(nil), s.ClosedByRemote...)
	s.BadMessages = append([]string(nil), s.BadMessages...)
	s.MsgCounts = map[byte]int{}
	for k, v := range p.st.MsgCounts {
		s.MsgCounts[k] = v
	}
	return s
}

// WaitFor polls pred on the stats until it holds or the timeout expires.
func (p *ExtPeer) WaitFor(timeout time.Duration, pred func(ExtStats) bool) bool {
	deadline := time.Now().Add(timeout)
	for {
		if pred(p.Stats()) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		select {
		case <-p.notify:
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func (p *ExtPeer) update(f func(*ExtStats)) {
	p.mu.Lock()
	f(&p.st)
	p.mu.Unlock()
	select {
	case p.notify <- struct{}{}:
	default:
	}
}

func (p *ExtPeer) has(i int) bool {
	if p.opts.HasNone {
		return false
	}
	if p.opts.Has == nil {
		return true
	}
	for _, h := range p.opts.Has {
		if h == i {
			return true
		}
	}
	return false
}

func (p *ExtPeer) handshake() []byte {
	var b bytes.Buffer
	b.WriteByte(byte(len(pstr)))
	b.WriteString(pstr)
	var reserved [8]byte
	if !p.opts.NoExtensions {
		reserved[5] |= 0x10
	}
	b.Write(reserved[:])
	b.Write(p.t.InfoHash[:])
	id := p.opts.PeerID
	if len(id) != 20 {
		id = fmt.Sprintf("-FX0001-%012d", p.Addr().Port())
	}
	b.WriteString(id)
	return b.Bytes()
}

func readHandshake(c net.Conn) (Handshake, error) {
	var l [1]byte
	if _, err := io.ReadFull(c, l[:]); err != nil {
		return Handshake{}, err
	}
	rest := make([]byte, int(l[0])+48)
	if _, err := io.ReadFull(c, rest); err != nil {
		return Handshake{}, err
	}
	n := int(l[0])
	h := Handshake{Pstr: string(rest[:n])}
	copy(h.Reserved[:], rest[n:n+8])
	copy(h.InfoHash[:], rest[n+8:n+28])
	copy(h.PeerID[:], rest[n+28:n+48])
	return h, nil
}

func (p *ExtPeer) bitfield() []byte {
	np := p.t.NumPieces()
	bf := make([]byte, (np+7)/8+p.opts.BitfieldExtraBytes)
	for i := 0; i < np; i++ {
		if p.has(i) {
			bf[i/8] |= 0x80 >> (i % 8)
		}
	}
	return bf
}

func (p *ExtPeer) serve(c net.Conn, dialed bool) {
	defer c.Close()
	c.SetDeadline(time.Now().Add(10 * time.Minute))
	var connNo int
	p.update(func(s *ExtStats) {
		s.Connections++
		s.Open++
		connNo = s.Connections
	})
	p.mu.Lock()
	p.conns = append(p.conns, c)
	p.mu.Unlock()
	defer p.update(func(s *ExtStats) { s.Open-- })

	if dialed {
		if _, err := c.Write(p.handshake()); err != nil {
			return
		}
	}
	h, err := readHandshake(c)
	if err != nil {
		return
	}
	p.update(func(s *ExtStats) { s.Handshakes = append(s.Handshakes, h) })
	if h.Pstr != pstr || h.InfoHash != p.t.InfoHash {
		p.update(func(s *ExtStats) {
			s.BadMessages = append(s.BadMessages, fmt.Sprintf("bad handshake pstr=%q hash=%x", h.Pstr, h.InfoHash))
		})
		return
	}
	if !dialed {
		if _, err := c.Write(p.handshake()); err != nil {
			return
		}
	}

	var wmu sync.Mutex
	send := func(id byte, payload []byte) error {
		wmu.Lock()
		defer wmu.Unlock()
		return writeMsg(c, id, payload)
	}
	extOK := !p.opts.NoExtensions && h.Reserved[5]&0x10 != 0
	sendBitfield := func() {
		if !p.opts.HasNone {
			send(5, p.bitfield())
		}
	}
	if p.opts.EarlyBitfield {
		sendBitfield()
	}
	if extOK {
		m := map[string]any{}
		if p.opts.ServeMetadata || p.opts.MetadataSize > 0 {
			m["ut_metadata"] = ExtMetadataID
		}
		if p.opts.PEX {
			m["ut_pex"] = ExtPEXID
		}
		port := p.opts.ListenPort
		if port == 0 {
			port = int(p.Addr().Port())
		}
		hs := map[string]any{"m": m, "v": "FakeExt 1.0", "p": port, "reqq": 64}
		if p.opts.MetadataSize > 0 {
			hs["metadata_size"] = p.opts.MetadataSize
		} else if p.opts.ServeMetadata {
			hs["metadata_size"] = len(p.info)
		}
		send(20, append([]byte{0}, benc.Encode(hs)...))
	}
	if !p.opts.EarlyBitfield {
		sendBitfield()
	}

	var theirMeta, theirPEX int64 // torrentd's extension ids
	for {
		var lb [4]byte
		if _, err := io.ReadFull(c, lb[:]); err != nil {
			if err == io.EOF || !isTimeout(err) {
				p.update(func(s *ExtStats) { s.ClosedByRemote = append(s.ClosedByRemote, time.Now()) })
			}
			return
		}
		l := binary.BigEndian.Uint32(lb[:])
		if l == 0 {
			continue
		}
		if l > 1<<20 {
			p.update(func(s *ExtStats) { s.BadMessages = append(s.BadMessages, fmt.Sprintf("%d-byte message", l)) })
			return
		}
		msg := make([]byte, l)
		if _, err := io.ReadFull(c, msg); err != nil {
			p.update(func(s *ExtStats) { s.ClosedByRemote = append(s.ClosedByRemote, time.Now()) })
			return
		}
		p.update(func(s *ExtStats) { s.MsgCounts[msg[0]]++ })
		switch msg[0] {
		case 2: // interested
			p.update(func(s *ExtStats) { s.Interested++ })
			if !p.opts.HasNone {
				send(1, nil)
			}
		case 6: // request
			if len(msg) != 13 {
				continue
			}
			idx, begin, ln := binary.BigEndian.Uint32(msg[1:]), binary.BigEndian.Uint32(msg[5:]), binary.BigEndian.Uint32(msg[9:])
			p.update(func(s *ExtStats) { s.Requests++ })
			if int(idx) >= p.t.NumPieces() || !p.has(int(idx)) || ln == 0 || ln > maxBlock ||
				int64(begin)+int64(ln) > p.t.PieceSize(int(idx)) {
				p.update(func(s *ExtStats) {
					s.BadMessages = append(s.BadMessages, fmt.Sprintf("bad request %d/%d/%d", idx, begin, ln))
				})
				continue
			}
			block := make([]byte, ln)
			p.t.ReadAt(block, int64(idx)*p.t.PieceLength+int64(begin))
			pl := binary.BigEndian.AppendUint32(nil, idx)
			pl = binary.BigEndian.AppendUint32(pl, begin)
			if send(7, append(pl, block...)) != nil {
				return
			}
			p.update(func(s *ExtStats) { s.BlocksSent++ })
		case 20:
			if len(msg) < 2 {
				continue
			}
			body := msg[2:]
			switch msg[1] {
			case 0:
				v, _, err := benc.Decode(body)
				d, _ := v.(map[string]any)
				if err != nil || d == nil {
					p.update(func(s *ExtStats) {
						s.BadMessages = append(s.BadMessages, fmt.Sprintf("undecodable extended handshake: %v", err))
					})
					continue
				}
				if m, ok := d["m"].(map[string]any); ok {
					theirMeta, _ = m["ut_metadata"].(int64)
					theirPEX, _ = m["ut_pex"].(int64)
				}
				p.update(func(s *ExtStats) {
					s.ExtHandshakes = append(s.ExtHandshakes, d)
					s.ExtHSTimes = append(s.ExtHSTimes, time.Now())
				})
				if p.opts.PEX && theirPEX > 0 && len(p.opts.SendPEX) > 0 {
					var added, flags []byte
					for _, e := range p.opts.SendPEX {
						added = append(added, CompactPeer(e.Addr)...)
						flags = append(flags, e.Flags)
					}
					pm := benc.Encode(map[string]any{"added": added, "added.f": flags, "dropped": ""})
					send(20, append([]byte{byte(theirPEX)}, pm...))
				}
			case ExtMetadataID:
				p.handleMetadata(body, theirMeta, send)
			case ExtPEXID:
				p.update(func(s *ExtStats) { s.PEX = append(s.PEX, decodePEX(body, connNo)) })
			default:
				p.update(func(s *ExtStats) {
					s.BadMessages = append(s.BadMessages, fmt.Sprintf("extended message with id %d, which this peer never advertised", msg[1]))
				})
			}
		}
	}
}

func (p *ExtPeer) handleMetadata(body []byte, theirID int64, send func(byte, []byte) error) {
	// The dict may be followed by data (msg_type 1); decode only the dict.
	end := dictEnd(body)
	v, _, err := benc.Decode(body[:end])
	d, _ := v.(map[string]any)
	if err != nil || d == nil {
		p.update(func(s *ExtStats) { s.BadMessages = append(s.BadMessages, "undecodable ut_metadata message") })
		return
	}
	mt, _ := d["msg_type"].(int64)
	piece, _ := d["piece"].(int64)
	if mt != 0 {
		return
	}
	p.update(func(s *ExtStats) { s.MetaRequests = append(s.MetaRequests, int(piece)) })
	if theirID == 0 {
		p.update(func(s *ExtStats) {
			s.BadMessages = append(s.BadMessages, "ut_metadata request, but torrentd advertised no ut_metadata id")
		})
		return
	}
	answer := func() {
		start := int(piece) * metadataPiece
		if !p.opts.ServeMetadata || p.opts.MetadataSize > 0 || piece < 0 || start >= len(p.info) {
			send(20, append([]byte{byte(theirID)}, benc.Encode(map[string]any{"msg_type": 2, "piece": int(piece)})...))
			p.update(func(s *ExtStats) { s.MetaRejects++ })
			return
		}
		data := append([]byte(nil), p.info[start:min(start+metadataPiece, len(p.info))]...)
		if p.opts.CorruptMetadata && piece == 0 {
			data[len(data)/2] ^= 0xff
		}
		hdr := benc.Encode(map[string]any{"msg_type": 1, "piece": int(piece), "total_size": len(p.info)})
		send(20, append(append([]byte{byte(theirID)}, hdr...), data...))
		p.update(func(s *ExtStats) { s.MetaDataSent++ })
	}
	if p.opts.MetadataGate != nil {
		go func() {
			<-p.opts.MetadataGate
			answer()
		}()
		return
	}
	answer()
}

// dictEnd returns the length of the bencoded value at the start of b (or
// len(b) if it can't be parsed).
func dictEnd(b []byte) int {
	i := 0
	var walk func() bool
	walk = func() bool {
		if i >= len(b) {
			return false
		}
		switch c := b[i]; {
		case c == 'i':
			j := bytes.IndexByte(b[i:], 'e')
			if j < 0 {
				return false
			}
			i += j + 1
		case c >= '0' && c <= '9':
			j := bytes.IndexByte(b[i:], ':')
			if j < 0 {
				return false
			}
			var n int
			fmt.Sscanf(string(b[i:i+j]), "%d", &n)
			i += j + 1 + n
		case c == 'l' || c == 'd':
			i++
			for i < len(b) && b[i] != 'e' {
				if !walk() {
					return false
				}
			}
			i++
		default:
			return false
		}
		return i <= len(b)
	}
	if !walk() {
		return len(b)
	}
	return i
}

func decodePEX(body []byte, conn int) PEXMsg {
	m := PEXMsg{Time: time.Now(), Conn: conn}
	v, _, err := benc.Decode(body)
	d, _ := v.(map[string]any)
	if err != nil || d == nil {
		m.Err = fmt.Sprintf("undecodable ut_pex: %v", err)
		return m
	}
	m.Raw = d
	added, _ := d["added"].(string)
	flags, _ := d["added.f"].(string)
	dropped, _ := d["dropped"].(string)
	if len(added)%6 != 0 || len(dropped)%6 != 0 {
		m.Err = fmt.Sprintf("added (%d bytes) or dropped (%d bytes) is not a multiple of 6", len(added), len(dropped))
		return m
	}
	if _, ok := d["added.f"]; ok && len(flags) != len(added)/6 {
		m.Err = fmt.Sprintf("added.f has %d bytes for %d added peers", len(flags), len(added)/6)
	}
	for i := 0; i+6 <= len(added); i += 6 {
		ap, _ := ParseCompactPeer(added[i : i+6])
		e := PEXEntry{Addr: ap}
		if i/6 < len(flags) {
			e.Flags = flags[i/6]
		}
		m.Added = append(m.Added, e)
	}
	for i := 0; i+6 <= len(dropped); i += 6 {
		ap, _ := ParseCompactPeer(dropped[i : i+6])
		m.Dropped = append(m.Dropped, ap)
	}
	return m
}
