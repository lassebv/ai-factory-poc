package peer

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"torrentd/internal/clock"
)

// Default timeouts for a connection.
const (
	DefaultDialTimeout      = 10 * time.Second
	DefaultHandshakeTimeout = 15 * time.Second
	// DefaultIdleTimeout disconnects a peer that sends nothing at all
	// (not even a keep-alive) for this long.
	DefaultIdleTimeout = 3 * time.Minute
	// DefaultKeepAlive is how long the writer may stay silent before it
	// sends a keep-alive.
	DefaultKeepAlive = 90 * time.Second
	writeTimeout     = 60 * time.Second
	sendQueueLen     = 1024
)

// ErrInfoHash is returned when the remote handshake names another torrent.
var ErrInfoHash = errors.New("peer: info-hash mismatch")

// ErrSendQueueFull is the close reason when a peer doesn't read fast enough
// for the send queue.
var ErrSendQueueFull = errors.New("peer: send queue full")

// ErrClosed is the close reason after Close.
var ErrClosed = errors.New("peer: connection closed")

// Config configures a connection.
type Config struct {
	InfoHash [20]byte
	PeerID   [20]byte
	Reserved [8]byte
	// MaxBitfield is the exact bitfield length for the torrent; larger
	// bitfields are ErrTooLarge.
	MaxBitfield int
	// Clock drives deadlines and keep-alives. Nil means clock.Real().
	Clock            clock.Clock
	DialTimeout      time.Duration
	HandshakeTimeout time.Duration
	IdleTimeout      time.Duration
	KeepAlive        time.Duration
}

func (c *Config) defaults() {
	if c.Clock == nil {
		c.Clock = clock.Real()
	}
	if c.DialTimeout <= 0 {
		c.DialTimeout = DefaultDialTimeout
	}
	if c.HandshakeTimeout <= 0 {
		c.HandshakeTimeout = DefaultHandshakeTimeout
	}
	if c.IdleTimeout <= 0 {
		c.IdleTimeout = DefaultIdleTimeout
	}
	if c.KeepAlive <= 0 {
		c.KeepAlive = DefaultKeepAlive
	}
}

// Conn is an established peer connection. After Start, one goroutine reads
// messages and hands them to the receive callback, and another writes the
// messages queued with Send.
type Conn struct {
	cfg    Config
	nc     net.Conn
	remote Handshake

	out    chan []byte
	closed chan struct{}
	once   sync.Once
	mu     sync.Mutex
	err    error
}

// Dial connects to addr and performs the handshake.
func Dial(ctx context.Context, addr string, cfg Config) (*Conn, error) {
	cfg.defaults()
	d := net.Dialer{Timeout: cfg.DialTimeout}
	nc, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	c, err := Establish(ctx, nc, cfg)
	if err != nil {
		nc.Close()
		return nil, err
	}
	return c, nil
}

// Establish sends our handshake on nc, reads and checks the remote one, and
// returns the connection. It doesn't close nc on failure.
func Establish(ctx context.Context, nc net.Conn, cfg Config) (*Conn, error) {
	cfg.defaults()
	stop := context.AfterFunc(ctx, func() { nc.SetDeadline(time.Unix(1, 0)) })
	defer stop()
	if err := nc.SetDeadline(cfg.Clock.Now().Add(cfg.HandshakeTimeout)); err != nil {
		return nil, err
	}
	hs := Handshake{Reserved: cfg.Reserved, InfoHash: cfg.InfoHash, PeerID: cfg.PeerID}
	if _, err := nc.Write(hs.Marshal()); err != nil {
		return nil, err
	}
	remote, err := ReadHandshake(nc)
	if err != nil {
		return nil, err
	}
	if remote.InfoHash != cfg.InfoHash {
		return nil, ErrInfoHash
	}
	if err := nc.SetDeadline(time.Time{}); err != nil {
		return nil, err
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return &Conn{cfg: cfg, nc: nc, remote: remote, out: make(chan []byte, sendQueueLen), closed: make(chan struct{})}, nil
}

// Remote returns the peer's handshake.
func (c *Conn) Remote() Handshake { return c.remote }

// RemoteAddr returns the peer's network address.
func (c *Conn) RemoteAddr() net.Addr { return c.nc.RemoteAddr() }

// Start launches the read and write goroutines. recv is called from the
// read goroutine for every message (keep-alives are not delivered).
// done is called exactly once, with the reason, when the connection ends.
// Both stop when ctx is cancelled.
func (c *Conn) Start(ctx context.Context, recv func(*Message), done func(error)) {
	stop := context.AfterFunc(ctx, func() { c.close(ctx.Err()) })
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		c.close(c.readLoop(recv))
	}()
	go func() {
		defer wg.Done()
		c.close(c.writeLoop())
	}()
	go func() {
		wg.Wait()
		stop()
		done(c.Err())
	}()
}

func (c *Conn) readLoop(recv func(*Message)) error {
	br := bufio.NewReaderSize(c.nc, 32<<10)
	for {
		if err := c.nc.SetReadDeadline(c.cfg.Clock.Now().Add(c.cfg.IdleTimeout)); err != nil {
			return err
		}
		m, err := ReadMessage(br, c.cfg.MaxBitfield)
		if err != nil {
			return err
		}
		if m != nil {
			recv(m)
		}
	}
}

func (c *Conn) writeLoop() error {
	t := c.cfg.Clock.NewTicker(c.cfg.KeepAlive)
	defer t.Stop()
	bw := bufio.NewWriterSize(c.nc, 32<<10)
	write := func(b []byte) error {
		if err := c.nc.SetWriteDeadline(c.cfg.Clock.Now().Add(writeTimeout)); err != nil {
			return err
		}
		_, err := bw.Write(b)
		return err
	}
	for {
		select {
		case <-c.closed:
			return ErrClosed
		case b := <-c.out:
			if err := write(b); err != nil {
				return err
			}
			// Coalesce whatever else is queued into one flush.
			for more := true; more; {
				select {
				case b := <-c.out:
					if err := write(b); err != nil {
						return err
					}
				default:
					more = false
				}
			}
			if err := bw.Flush(); err != nil {
				return err
			}
			t.Reset(c.cfg.KeepAlive)
		case <-t.C():
			if err := write((*Message)(nil).Marshal()); err != nil {
				return err
			}
			if err := bw.Flush(); err != nil {
				return err
			}
		}
	}
}

// Send queues m for the write goroutine without blocking. If the queue is
// full the connection is closed with ErrSendQueueFull. It reports whether m
// was queued.
func (c *Conn) Send(m *Message) bool {
	select {
	case <-c.closed:
		return false
	default:
	}
	select {
	case c.out <- m.Marshal():
		return true
	default:
		c.close(ErrSendQueueFull)
		return false
	}
}

// Close closes the connection.
func (c *Conn) Close() { c.close(ErrClosed) }

func (c *Conn) close(reason error) {
	c.once.Do(func() {
		c.mu.Lock()
		c.err = reason
		c.mu.Unlock()
		close(c.closed)
		c.nc.Close()
	})
}

// Err returns the reason the connection closed, or nil while it is open.
func (c *Conn) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

// String describes the connection for logs.
func (c *Conn) String() string { return fmt.Sprintf("peer %s", c.nc.RemoteAddr()) }
