package fakes

import (
	"bytes"
	"crypto/sha1"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"sync"
	"time"

	"torrentd.local/harness/acceptance/internal/torrentgen"
)

// LeecherOptions scripts a fake leecher that downloads from torrentd.
type LeecherOptions struct {
	Want      []int         // pieces to fetch; nil means all
	Pipeline  int           // outstanding block requests (default 16)
	Delay     time.Duration // pause before each request (throttles this leecher)
	Oversize  bool          // after the first unchoke, request a 32 KiB block
	StayAfter bool          // keep the connection open after completing
	PeerID    string        // 20 bytes; default "-FL0001-" + digits
}

// LeecherStats is what the leecher observed.
type LeecherStats struct {
	Handshaked     bool
	Unchoked       bool // current state
	Unchokes       int
	Chokes         int
	HaveMsgs       int
	Bitfield       bool
	Available      int // pieces torrentd announced (bitfield + have)
	Blocks         int
	Bytes          int64
	Verified       int
	BadPieces      int
	Rejects        int // BEP 6 reject_request messages
	Complete       bool
	Closed         bool // the connection ended
	ClosedByRemote bool // torrentd closed it
	ClosedAt       time.Time
}

// Leecher is one connection to torrentd's peer port.
type Leecher struct {
	t    *torrentgen.Torrent
	opts LeecherOptions
	c    net.Conn

	mu       sync.Mutex
	st       LeecherStats
	has      []bool
	want     map[int]bool
	done     map[int]bool
	pending  map[[2]uint32]bool // index, begin
	bufs     map[int][]byte
	got      map[int]int64
	wake     chan struct{}
	finished chan struct{}
	closeOne sync.Once
}

// DialLeecher connects to torrentd at addr and starts downloading.
func DialLeecher(addr string, t *torrentgen.Torrent, opts LeecherOptions) (*Leecher, error) {
	if opts.Pipeline == 0 {
		opts.Pipeline = 16
	}
	c, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		return nil, err
	}
	l := &Leecher{t: t, opts: opts, c: c, has: make([]bool, t.NumPieces()), want: map[int]bool{},
		done: map[int]bool{}, pending: map[[2]uint32]bool{}, bufs: map[int][]byte{}, got: map[int]int64{},
		wake: make(chan struct{}, 1), finished: make(chan struct{})}
	if opts.Want == nil {
		for i := 0; i < t.NumPieces(); i++ {
			l.want[i] = true
		}
	} else {
		for _, i := range opts.Want {
			l.want[i] = true
		}
	}
	id := opts.PeerID
	if len(id) != 20 {
		id = "-FL0001-000000000000"
	}
	var hs bytes.Buffer
	hs.WriteByte(byte(len(pstr)))
	hs.WriteString(pstr)
	hs.Write(make([]byte, 8))
	hs.Write(t.InfoHash[:])
	hs.WriteString(id)
	if _, err := c.Write(hs.Bytes()); err != nil {
		c.Close()
		return nil, err
	}
	resp := make([]byte, 68)
	c.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.ReadFull(c, resp); err != nil || !bytes.Equal(resp[28:48], t.InfoHash[:]) {
		c.Close()
		if err == nil {
			err = errors.New("fakes: handshake info_hash mismatch")
		}
		return nil, err
	}
	c.SetReadDeadline(time.Time{})
	l.st.Handshaked = true
	writeMsg(c, 2, nil) // interested
	go l.reader()
	go l.writer()
	return l, nil
}

// Stats returns a snapshot.
func (l *Leecher) Stats() LeecherStats {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.st
}

// WaitComplete waits until every wanted piece verified.
func (l *Leecher) WaitComplete(timeout time.Duration) bool {
	select {
	case <-l.finished:
		return l.Stats().Complete
	case <-time.After(timeout):
		return false
	}
}

// Close drops the connection.
func (l *Leecher) Close() { l.c.Close() }

func (l *Leecher) poke() {
	select {
	case l.wake <- struct{}{}:
	default:
	}
}

func (l *Leecher) end(remote bool) {
	l.closeOne.Do(func() {
		l.mu.Lock()
		l.st.Closed = true
		l.st.ClosedByRemote = remote
		l.st.ClosedAt = time.Now()
		l.mu.Unlock()
		l.c.Close()
		select {
		case <-l.finished:
		default:
			close(l.finished)
		}
		l.poke()
	})
}

func (l *Leecher) reader() {
	for {
		var lb [4]byte
		if _, err := io.ReadFull(l.c, lb[:]); err != nil {
			l.end(!l.Stats().Complete || !l.opts.StayAfter)
			return
		}
		n := binary.BigEndian.Uint32(lb[:])
		if n == 0 {
			continue
		}
		if n > 1<<20 {
			l.end(false)
			return
		}
		msg := make([]byte, n)
		if _, err := io.ReadFull(l.c, msg); err != nil {
			l.end(true)
			return
		}
		l.mu.Lock()
		switch msg[0] {
		case 0: // choke: pending requests are void (BEP 3)
			l.st.Unchoked = false
			l.st.Chokes++
			l.pending = map[[2]uint32]bool{}
		case 1:
			if !l.st.Unchoked {
				l.st.Unchokes++
			}
			l.st.Unchoked = true
		case 4:
			if len(msg) == 5 {
				if i := int(binary.BigEndian.Uint32(msg[1:])); i < len(l.has) && !l.has[i] {
					l.has[i] = true
					l.st.Available++
				}
				l.st.HaveMsgs++
			}
		case 5:
			l.st.Bitfield = true
			for i := range l.has {
				if i/8+1 < len(msg) && msg[1+i/8]&(0x80>>(i%8)) != 0 && !l.has[i] {
					l.has[i] = true
					l.st.Available++
				}
			}
		case 16: // reject_request (BEP 6)
			l.st.Rejects++
			if len(msg) == 13 {
				delete(l.pending, [2]uint32{binary.BigEndian.Uint32(msg[1:]), binary.BigEndian.Uint32(msg[5:])})
			}
		case 7:
			if len(msg) >= 9 {
				idx := int(binary.BigEndian.Uint32(msg[1:]))
				begin := binary.BigEndian.Uint32(msg[5:])
				block := msg[9:]
				key := [2]uint32{uint32(idx), begin}
				if idx < len(l.has) && l.pending[key] {
					delete(l.pending, key)
					l.st.Blocks++
					l.st.Bytes += int64(len(block))
					buf := l.bufs[idx]
					if buf == nil {
						buf = make([]byte, l.t.PieceSize(idx))
						l.bufs[idx] = buf
					}
					if int(begin)+len(block) <= len(buf) {
						copy(buf[begin:], block)
						l.got[idx] += int64(len(block))
					}
					if l.got[idx] >= int64(len(buf)) {
						if sha1.Sum(buf) == l.t.Pieces[idx] {
							l.done[idx] = true
							l.st.Verified++
						} else {
							l.st.BadPieces++
						}
						delete(l.bufs, idx)
						delete(l.got, idx)
						if len(l.done) == len(l.want) && !l.st.Complete {
							l.st.Complete = true
							close(l.finished)
						}
					}
				}
			}
		}
		complete := l.st.Complete
		l.mu.Unlock()
		if complete && !l.opts.StayAfter {
			l.end(false)
			return
		}
		l.poke()
	}
}

// writer keeps Pipeline requests outstanding while unchoked.
func (l *Leecher) writer() {
	oversizeSent := false
	for {
		select {
		case <-l.wake:
		case <-time.After(200 * time.Millisecond):
		}
		l.mu.Lock()
		if l.st.Closed {
			l.mu.Unlock()
			return
		}
		if !l.st.Unchoked || l.st.Complete {
			l.mu.Unlock()
			continue
		}
		if l.opts.Oversize && !oversizeSent {
			oversizeSent = true
			l.mu.Unlock()
			req := make([]byte, 12)
			binary.BigEndian.PutUint32(req[8:], 32768)
			writeMsg(l.c, 6, req)
			continue
		}
		var reqs [][3]uint32
	outer:
		for i := 0; i < len(l.has) && len(l.pending)+len(reqs) < l.opts.Pipeline; i++ {
			if !l.want[i] || l.done[i] || !l.has[i] {
				continue
			}
			size := l.t.PieceSize(i)
			for off := int64(0); off < size; off += maxBlock {
				key := [2]uint32{uint32(i), uint32(off)}
				if l.pending[key] || (l.bufs[i] != nil && l.got[i] > off) {
					continue
				}
				l.pending[key] = true
				reqs = append(reqs, [3]uint32{uint32(i), uint32(off), uint32(min(int64(maxBlock), size-off))})
				if len(l.pending) >= l.opts.Pipeline {
					break outer
				}
			}
		}
		l.mu.Unlock()
		for _, r := range reqs {
			if l.opts.Delay > 0 {
				time.Sleep(l.opts.Delay)
			}
			p := make([]byte, 12)
			binary.BigEndian.PutUint32(p, r[0])
			binary.BigEndian.PutUint32(p[4:], r[1])
			binary.BigEndian.PutUint32(p[8:], r[2])
			if writeMsg(l.c, 6, p) != nil {
				return
			}
		}
	}
}

// WatchUnchokes samples leechers every 100 ms for d and returns the largest
// number unchoked at the same time and how many distinct leechers were ever
// unchoked. A leecher that completed or closed no longer counts as unchoked.
func WatchUnchokes(ls []*Leecher, d time.Duration) (maxNow, distinct int) {
	ever := map[int]bool{}
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		n := 0
		for i, l := range ls {
			st := l.Stats()
			if st.Unchoked && !st.Closed {
				n++
				ever[i] = true
			}
		}
		maxNow = max(maxNow, n)
		time.Sleep(100 * time.Millisecond)
	}
	return maxNow, len(ever)
}
