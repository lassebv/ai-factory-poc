package peer

import (
	"bufio"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"torrentd/internal/clock"
)

// conn implements Conn.
//
// Goroutines (all started by newConn):
//   - readLoop: decodes messages and calls OnMessage sequentially;
//   - writeLoop: drains the send queue into nc;
//   - superviseLoop: owns the keep-alive and idle timers (driven by the
//     Config clock) and, once the conn is closing, waits for the other two,
//     closes Done and calls OnClose.
//
// The timers live on their own goroutine because both I/O goroutines may be
// blocked in nc for an unbounded time (a peer that neither reads nor
// writes); the idle timeout must still fire then.
type conn struct {
	nc     net.Conn
	remote Handshake
	cfg    Config
	clk    clock.Clock
	log    *slog.Logger

	// lastRead and lastWrite are clock times (UnixNano) of the last complete
	// message read and of the last successful write.
	lastRead  atomic.Int64
	lastWrite atomic.Int64

	mu     sync.Mutex
	queue  []Message
	closed bool  // closeWith was called; guarded by mu
	err    error // reason; guarded by mu, published by done
	wake   chan struct{}

	closing   chan struct{} // closed by closeWith
	done      chan struct{} // closed after all goroutines have exited
	readDone  chan struct{}
	writeDone chan struct{}

	keepAlive clock.Timer
	idle      clock.Timer
}

func newConn(nc net.Conn, remote Handshake, cfg Config) Conn {
	clk := cfg.Clock
	if clk == nil {
		clk = clock.Real()
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	c := &conn{
		nc:        nc,
		remote:    remote,
		cfg:       cfg,
		clk:       clk,
		log:       log.With("peer", nc.RemoteAddr().String()),
		wake:      make(chan struct{}, 1),
		closing:   make(chan struct{}),
		done:      make(chan struct{}),
		readDone:  make(chan struct{}),
		writeDone: make(chan struct{}),
	}
	now := clk.Now().UnixNano()
	c.lastRead.Store(now)
	c.lastWrite.Store(now)
	// Every timer exists before newConn returns, so a test advancing a fake
	// clock right after NewConn is observed.
	c.keepAlive = clk.NewTimer(KeepAliveInterval)
	c.idle = clk.NewTimer(IdleTimeout)
	go c.readLoop()
	go c.writeLoop()
	go c.superviseLoop()
	return c
}

// Send queues m. The message (including its slices) must not be modified by
// the caller afterwards.
func (c *conn) Send(m Message) error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return ErrClosed
	}
	if len(c.queue) >= SendQueueLimit {
		c.mu.Unlock()
		c.closeWith(ErrSendQueueFull)
		return ErrSendQueueFull
	}
	c.queue = append(c.queue, m)
	c.mu.Unlock()
	c.signal()
	return nil
}

func (c *conn) signal() {
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

// Close closes the connection without waiting for the goroutines.
func (c *conn) Close() error {
	c.closeWith(nil)
	return nil
}

// closeWith records the first close reason, then closes nc to unblock any
// pending read or write. Later calls are no-ops.
func (c *conn) closeWith(err error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	c.err = err
	c.queue = nil
	c.mu.Unlock()
	close(c.closing)
	_ = c.nc.Close()
	if err != nil {
		c.log.Debug("peer connection closed", "err", err)
	}
}

// Done is closed once the conn is fully shut down.
func (c *conn) Done() <-chan struct{} { return c.done }

// Err returns the close reason once Done is closed, nil before.
func (c *conn) Err() error {
	select {
	case <-c.done:
	default:
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

// Remote returns the remote handshake.
func (c *conn) Remote() Handshake { return c.remote }

// RemoteAddr returns the remote network address.
func (c *conn) RemoteAddr() net.Addr { return c.nc.RemoteAddr() }

func (c *conn) isClosing() bool {
	select {
	case <-c.closing:
		return true
	default:
		return false
	}
}

func (c *conn) readLoop() {
	defer close(c.readDone)
	br := bufio.NewReaderSize(c.nc, 32<<10)
	for {
		m, err := readMessage(br, c.cfg.MaxBitfieldLen)
		if err != nil {
			c.closeWith(fmt.Errorf("peer: read: %w", err))
			return
		}
		// Push the idle deadline back before OnMessage, which may block.
		c.lastRead.Store(c.clk.Now().UnixNano())
		if m.KeepAlive {
			continue
		}
		if c.isClosing() {
			return
		}
		c.cfg.OnMessage(m)
	}
}

func (c *conn) writeLoop() {
	defer close(c.writeDone)
	bw := bufio.NewWriterSize(c.nc, 32<<10)
	for {
		select {
		case <-c.closing:
			return
		case <-c.wake:
		}
		c.mu.Lock()
		batch := c.queue
		c.queue = nil
		c.mu.Unlock()
		for _, m := range batch {
			if err := writeMessage(bw, m); err != nil {
				c.closeWith(fmt.Errorf("peer: write: %w", err))
				return
			}
		}
		if err := bw.Flush(); err != nil {
			c.closeWith(fmt.Errorf("peer: write: %w", err))
			return
		}
		if len(batch) > 0 {
			c.lastWrite.Store(c.clk.Now().UnixNano())
		}
		if c.cfg.OnPieceSent != nil {
			for _, m := range batch {
				if !m.KeepAlive && m.ID == MsgPiece {
					c.cfg.OnPieceSent(m.Index, m.Begin, len(m.Block))
				}
			}
		}
	}
}

// superviseLoop runs the timers until the conn starts closing, then waits
// for the I/O goroutines and finishes the shutdown.
//
// A timer firing only means "check now": the actual decision compares the
// clock against lastRead/lastWrite, so stale timer values never cause a
// spurious timeout.
func (c *conn) superviseLoop() {
	defer c.finish()
	for {
		select {
		case <-c.closing:
			return
		case <-c.idle.C():
			if left := IdleTimeout - c.since(&c.lastRead); left > 0 {
				c.idle.Reset(left)
				continue
			}
			c.closeWith(ErrIdleTimeout)
			return
		case <-c.keepAlive.C():
			if left := KeepAliveInterval - c.since(&c.lastWrite); left > 0 {
				c.keepAlive.Reset(left)
				continue
			}
			c.enqueueKeepAlive()
			// Restart the interval now; the write updates lastWrite and the
			// next check reschedules from there.
			c.keepAlive.Reset(KeepAliveInterval)
		}
	}
}

func (c *conn) since(t *atomic.Int64) time.Duration {
	return time.Duration(c.clk.Now().UnixNano() - t.Load())
}

// enqueueKeepAlive queues a keep-alive unless other messages are already
// waiting (they serve the same purpose) or the conn is closed.
func (c *conn) enqueueKeepAlive() {
	c.mu.Lock()
	if c.closed || len(c.queue) > 0 {
		c.mu.Unlock()
		return
	}
	c.queue = append(c.queue, Message{KeepAlive: true})
	c.mu.Unlock()
	c.signal()
}

func (c *conn) finish() {
	c.keepAlive.Stop()
	c.idle.Stop()
	<-c.readDone
	<-c.writeDone
	_ = c.nc.Close()
	close(c.done)
	if c.cfg.OnClose != nil {
		c.mu.Lock()
		err := c.err
		c.mu.Unlock()
		c.cfg.OnClose(err)
	}
}
