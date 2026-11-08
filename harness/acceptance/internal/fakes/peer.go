package fakes

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/netip"
	"sync"
	"time"

	"torrentd.local/harness/acceptance/internal/torrentgen"
)

const (
	pstr     = "BitTorrent protocol"
	maxBlock = 16 * 1024
)

// PeerOptions scripts the fake peer's behavior.
type PeerOptions struct {
	CorruptOnce map[int]bool  // the first time each of these pieces is served, one byte is flipped
	StallFrom   int           // when > 0, requests for pieces >= StallFrom wait until Release()
	Delay       time.Duration // delay before each block is sent (lets requests pile up)
	Garbage     bool          // after the handshake, send random bytes and hang up
	Oversize    bool          // after bitfield+unchoke, send a message longer than 16 KiB + 13 bytes
	Seed        uint64

	// Holdout behaviors (milestone 3 quality comparison).
	CorruptAll     bool          // every block at offset 0 has a flipped byte, always
	DropAfter      int           // when > 0, the first connection is closed after this many blocks
	ChokeAfter     int           // when > 0, choke once after this many blocks...
	ChokeFor       time.Duration // ...for this long, then unchoke
	HaveNoBitfield bool          // announce pieces with `have` messages instead of a bitfield
	Bogus          bool          // after unchoke, send pieces nobody asked for (bad index, bad length, unrequested)
}

// Handshake is what torrentd sent.
type Handshake struct {
	Pstr     string
	Reserved [8]byte
	InfoHash [20]byte
	PeerID   [20]byte
}

// Request is one block request.
type Request struct {
	Index, Begin, Length uint32
}

// PeerStats summarizes what torrentd did on all connections to this peer.
type PeerStats struct {
	Connections    int
	Handshakes     []Handshake
	BadHandshakes  []string
	Requests       []Request
	BadRequests    []string
	MaxOutstanding int
	BlocksSent     int
	OversizeSent   int
	ClosedByRemote int // connections torrentd closed (EOF/reset on read)
}

// Peer is a scriptable BitTorrent seeder that serves one torrent.
type Peer struct {
	t    *torrentgen.Torrent
	opts PeerOptions
	ln   net.Listener

	mu        sync.Mutex
	cond      *sync.Cond
	released  bool
	corrupted map[int]bool
	stats     PeerStats
	conns     []net.Conn
}

// NewPeer starts a fake peer on 127.0.0.1.
func NewPeer(t *torrentgen.Torrent, opts PeerOptions) (*Peer, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	p := &Peer{t: t, opts: opts, ln: ln, corrupted: map[int]bool{}}
	p.cond = sync.NewCond(&p.mu)
	go p.accept()
	return p, nil
}

// Addr is the peer's listen address.
func (p *Peer) Addr() netip.AddrPort { return p.ln.Addr().(*net.TCPAddr).AddrPort() }

// Release lets stalled requests through.
func (p *Peer) Release() {
	p.mu.Lock()
	p.released = true
	p.cond.Broadcast()
	p.mu.Unlock()
}

// Stats returns a snapshot.
func (p *Peer) Stats() PeerStats {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := p.stats
	s.Handshakes = append([]Handshake(nil), s.Handshakes...)
	s.Requests = append([]Request(nil), s.Requests...)
	s.BadRequests = append([]string(nil), s.BadRequests...)
	s.BadHandshakes = append([]string(nil), s.BadHandshakes...)
	return s
}

// Close stops the peer and drops all connections.
func (p *Peer) Close() {
	p.ln.Close()
	p.Release()
	p.mu.Lock()
	for _, c := range p.conns {
		c.Close()
	}
	p.mu.Unlock()
}

func (p *Peer) accept() {
	for {
		c, err := p.ln.Accept()
		if err != nil {
			return
		}
		p.mu.Lock()
		p.stats.Connections++
		p.conns = append(p.conns, c)
		p.mu.Unlock()
		go p.serve(c)
	}
}

func (p *Peer) bad(format string, a ...any) {
	p.mu.Lock()
	p.stats.BadRequests = append(p.stats.BadRequests, fmt.Sprintf(format, a...))
	p.mu.Unlock()
}

func writeMsg(w io.Writer, id byte, payload []byte) error {
	buf := make([]byte, 5+len(payload))
	binary.BigEndian.PutUint32(buf, uint32(1+len(payload)))
	buf[4] = id
	copy(buf[5:], payload)
	_, err := w.Write(buf)
	return err
}

func (p *Peer) serve(c net.Conn) {
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Minute))

	// Handshake from torrentd.
	var hs [68]byte
	if _, err := io.ReadFull(c, hs[:1]); err != nil {
		return
	}
	n := int(hs[0])
	rest := make([]byte, n+48)
	if _, err := io.ReadFull(c, rest); err != nil {
		p.mu.Lock()
		p.stats.BadHandshakes = append(p.stats.BadHandshakes, "short handshake: "+err.Error())
		p.mu.Unlock()
		return
	}
	h := Handshake{Pstr: string(rest[:n])}
	copy(h.Reserved[:], rest[n:n+8])
	copy(h.InfoHash[:], rest[n+8:n+28])
	copy(h.PeerID[:], rest[n+28:n+48])
	p.mu.Lock()
	p.stats.Handshakes = append(p.stats.Handshakes, h)
	p.mu.Unlock()
	if h.Pstr != pstr || h.InfoHash != p.t.InfoHash {
		p.mu.Lock()
		p.stats.BadHandshakes = append(p.stats.BadHandshakes, fmt.Sprintf("pstr %q / info_hash %x", h.Pstr, h.InfoHash))
		p.mu.Unlock()
		return
	}

	// Our handshake.
	var out bytes.Buffer
	out.WriteByte(byte(len(pstr)))
	out.WriteString(pstr)
	out.Write(make([]byte, 8))
	out.Write(p.t.InfoHash[:])
	out.WriteString("-FP0001-")
	rng := rand.New(rand.NewPCG(p.opts.Seed, uint64(time.Now().UnixNano())))
	for i := 0; i < 12; i++ {
		out.WriteByte(byte('0' + rng.IntN(10)))
	}
	if _, err := c.Write(out.Bytes()); err != nil {
		return
	}

	if p.opts.Garbage {
		junk := make([]byte, 1+rng.IntN(4096))
		for i := range junk {
			junk[i] = byte(rng.Uint32())
		}
		c.Write(junk)
		time.Sleep(50 * time.Millisecond)
		return
	}

	// Bitfield: we have everything.
	np := p.t.NumPieces()
	bf := make([]byte, (np+7)/8)
	for i := 0; i < np; i++ {
		bf[i/8] |= 0x80 >> (i % 8)
	}
	var wmu sync.Mutex
	send := func(id byte, payload []byte) error {
		wmu.Lock()
		defer wmu.Unlock()
		return writeMsg(c, id, payload)
	}
	if p.opts.HaveNoBitfield {
		for i := 0; i < np; i++ {
			have := make([]byte, 4)
			binary.BigEndian.PutUint32(have, uint32(i))
			if err := send(4, have); err != nil {
				return
			}
		}
	} else if err := send(5, bf); err != nil {
		return
	}
	p.mu.Lock()
	connNo := p.stats.Connections
	p.mu.Unlock()
	sentHere := 0
	choked := false

	if p.opts.Oversize {
		send(1, nil) // unchoke
		go func() {
			wmu.Lock()
			defer wmu.Unlock()
			frame := make([]byte, 4+maxBlock+9+200)
			binary.BigEndian.PutUint32(frame, uint32(maxBlock+9+200)) // > 16 KiB + 13 bytes on the wire
			// An unknown message id: a client without a size limit would read
			// and ignore it, so only the size check can cause the disconnect.
			frame[4] = 99
			p.mu.Lock()
			p.stats.OversizeSent++
			p.mu.Unlock()
			c.Write(frame)
		}()
		c.SetReadDeadline(time.Now().Add(15 * time.Second))
		if _, err := io.Copy(io.Discard, c); err == nil || !isTimeout(err) {
			p.mu.Lock()
			p.stats.ClosedByRemote++
			p.mu.Unlock()
		}
		return
	}

	queue := make(chan Request, 4096)
	var outstanding int
	cancelled := map[Request]bool{}
	var qmu sync.Mutex

	// serveOne answers one request; false means the connection is gone.
	serveOne := func(r Request) bool {
		qmu.Lock()
		if cancelled[r] {
			delete(cancelled, r)
			outstanding--
			qmu.Unlock()
			return true
		}
		qmu.Unlock()
		if p.opts.Delay > 0 {
			time.Sleep(p.opts.Delay)
		}
		block := make([]byte, r.Length)
		if _, err := p.t.ReadAt(block, int64(r.Index)*p.t.PieceLength+int64(r.Begin)); err != nil && err != io.EOF {
			return false
		}
		p.mu.Lock()
		if p.opts.CorruptOnce[int(r.Index)] && !p.corrupted[int(r.Index)] && r.Begin == 0 {
			p.corrupted[int(r.Index)] = true
			block[0] ^= 0xff
		}
		if p.opts.CorruptAll && r.Begin == 0 {
			block[0] ^= 0xff
		}
		p.stats.BlocksSent++
		p.mu.Unlock()
		payload := make([]byte, 8+len(block))
		binary.BigEndian.PutUint32(payload, r.Index)
		binary.BigEndian.PutUint32(payload[4:], r.Begin)
		copy(payload[8:], block)
		if err := send(7, payload); err != nil {
			return false
		}
		qmu.Lock()
		outstanding--
		qmu.Unlock()
		sentHere++
		if p.opts.DropAfter > 0 && connNo == 1 && sentHere >= p.opts.DropAfter {
			c.Close()
			return false
		}
		if p.opts.ChokeAfter > 0 && !choked && sentHere >= p.opts.ChokeAfter {
			choked = true
			send(0, nil) // choke: per BEP 3 the client must re-request after unchoke
			time.Sleep(p.opts.ChokeFor)
			qmu.Lock()
			for len(queue) > 0 { // pending requests are discarded while choked
				<-queue
				outstanding--
			}
			qmu.Unlock()
			send(1, nil)
		}
		return true
	}

	// Writer: answer requests in order. Requests for stalled pieces are held
	// back on their own (until Release) without blocking the other pieces, so
	// the stall does not depend on the client's piece order (M3 escalation).
	done := make(chan struct{})
	go func() {
		defer close(done)
		for r := range queue {
			if p.opts.StallFrom > 0 && int(r.Index) >= p.opts.StallFrom {
				p.mu.Lock()
				stalled := !p.released
				p.mu.Unlock()
				if stalled {
					go func(r Request) {
						p.mu.Lock()
						for !p.released {
							p.cond.Wait()
						}
						p.mu.Unlock()
						serveOne(r)
					}(r)
					continue
				}
			}
			if !serveOne(r) {
				return
			}
		}
	}()
	// Held requests finish when the test releases or closes the peer.
	defer func() { close(queue); <-done }()

	for {
		var lenBuf [4]byte
		if _, err := io.ReadFull(c, lenBuf[:]); err != nil {
			p.mu.Lock()
			p.stats.ClosedByRemote++
			p.mu.Unlock()
			return
		}
		l := binary.BigEndian.Uint32(lenBuf[:])
		if l == 0 {
			continue // keep-alive
		}
		if l > 1<<20 {
			p.bad("message of %d bytes from torrentd", l)
			return
		}
		msg := make([]byte, l)
		if _, err := io.ReadFull(c, msg); err != nil {
			return
		}
		switch msg[0] {
		case 2: // interested
			send(1, nil) // unchoke
			if p.opts.Bogus {
				bogus := func(index, begin uint32, n int) {
					pl := make([]byte, 8+n)
					binary.BigEndian.PutUint32(pl, index)
					binary.BigEndian.PutUint32(pl[4:], begin)
					send(7, pl)
				}
				bogus(uint32(np+5), 0, 16384)                       // index out of range
				bogus(0, uint32(p.t.PieceSize(0)), 16384)           // begin past the piece end
				bogus(0, 0, 100)                                    // unrequested, odd length
				bogus(uint32(np-1), 0, int(p.t.PieceSize(np-1))+16) // longer than the piece
			}
		case 6, 8: // request, cancel
			if len(msg) != 13 {
				p.bad("request/cancel with %d-byte payload", len(msg)-1)
				continue
			}
			r := Request{binary.BigEndian.Uint32(msg[1:]), binary.BigEndian.Uint32(msg[5:]), binary.BigEndian.Uint32(msg[9:])}
			if msg[0] == 8 {
				qmu.Lock()
				cancelled[r] = true
				qmu.Unlock()
				continue
			}
			if int(r.Index) >= np || r.Length == 0 || r.Length > maxBlock ||
				int64(r.Begin)+int64(r.Length) > p.t.PieceSize(int(r.Index)) {
				p.bad("invalid request %+v (piece size %d)", r, p.t.PieceSize(int(r.Index)))
				continue
			}
			p.mu.Lock()
			p.stats.Requests = append(p.stats.Requests, r)
			p.mu.Unlock()
			qmu.Lock()
			outstanding++
			if outstanding > 0 {
				p.mu.Lock()
				if outstanding > p.stats.MaxOutstanding {
					p.stats.MaxOutstanding = outstanding
				}
				p.mu.Unlock()
			}
			qmu.Unlock()
			select {
			case queue <- r:
			default:
				p.bad("more than 4096 outstanding requests")
			}
		}
	}
}

func isTimeout(err error) bool {
	ne, ok := err.(net.Error)
	return ok && ne.Timeout()
}
