// Package session orchestrates the engine (§4.4, §4.5): one Session per
// process owns the peer listener, the worker pools and one Torrent actor
// per torrent. Peer goroutines never touch torrent state; they send events
// to the torrent's actor goroutine.
package session

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"torrentd/internal/bt/metainfo"
	"torrentd/internal/bt/storage"
	"torrentd/internal/clock"
)

// PeerIDPrefix is the Azureus-style client prefix of our peer ID (D13).
const PeerIDPrefix = "-TD0100-"

// Defaults (§4.1 connection limits, §6.2).
const (
	DefaultMaxPeers    = 50
	DefaultMaxHalfOpen = 20
	DefaultStopTimeout = 3 * time.Second
)

// ErrExists is returned by Add for a torrent that is already present.
var ErrExists = errors.New("session: torrent already exists")

// Config configures a Session.
type Config struct {
	// PeerPort is the TCP port for incoming peers; 0 picks any free port.
	PeerPort int
	// Clock is the time source (T1). Nil means clock.Real().
	Clock clock.Clock
	// Rand is the randomness source (T2): peer ID, key, tier shuffles. Nil
	// means a randomly seeded source.
	Rand *rand.Rand
	// Logger receives engine logs. Nil means slog.Default().
	Logger *slog.Logger
	// HTTPClient is used for tracker announces. Nil means a client with a
	// 30 s timeout.
	HTTPClient *http.Client
	// MaxPeers is the per-torrent connection limit.
	MaxPeers int
	// MaxHalfOpen bounds concurrent outgoing connection attempts.
	MaxHalfOpen int
	// StopTimeout bounds the stopped announces on shutdown (§6.2).
	StopTimeout time.Duration
	// MaxHandles is the open-file limit per torrent (D9).
	MaxHandles int
}

// Session is the engine.
type Session struct {
	cfg    Config
	clk    clock.Clock
	log    *slog.Logger
	ln     net.Listener
	peerID [20]byte
	key    string

	disk *storage.Pool
	hash *storage.Pool

	ctx      context.Context
	cancel   context.CancelFunc
	halfOpen chan struct{}
	acceptWG sync.WaitGroup

	mu       sync.Mutex
	rng      *rand.Rand
	torrents map[[20]byte]*Torrent
	order    []*Torrent
	closed   bool
}

// New starts a session: it binds the peer port on all interfaces and
// starts the worker pools.
func New(cfg Config) (*Session, error) {
	if cfg.Clock == nil {
		cfg.Clock = clock.Real()
	}
	if cfg.Rand == nil {
		cfg.Rand = rand.New(rand.NewPCG(rand.Uint64(), rand.Uint64()))
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 30 * time.Second}
	}
	if cfg.MaxPeers <= 0 {
		cfg.MaxPeers = DefaultMaxPeers
	}
	if cfg.MaxHalfOpen <= 0 {
		cfg.MaxHalfOpen = DefaultMaxHalfOpen
	}
	if cfg.StopTimeout <= 0 {
		cfg.StopTimeout = DefaultStopTimeout
	}
	ln, err := net.Listen("tcp", ":"+strconv.Itoa(max(cfg.PeerPort, 0)))
	if err != nil {
		return nil, fmt.Errorf("bind peer port %d: %w", cfg.PeerPort, err)
	}
	s := &Session{
		cfg:      cfg,
		clk:      cfg.Clock,
		log:      cfg.Logger,
		ln:       ln,
		rng:      cfg.Rand,
		disk:     storage.NewPool(storage.DefaultDiskWorkers),
		hash:     storage.NewPool(storage.DefaultHashWorkers()),
		halfOpen: make(chan struct{}, cfg.MaxHalfOpen),
		torrents: map[[20]byte]*Torrent{},
	}
	copy(s.peerID[:], PeerIDPrefix)
	for i := len(PeerIDPrefix); i < 20; i++ {
		s.peerID[i] = byte(s.rng.Uint32())
	}
	s.key = fmt.Sprintf("%08x", s.rng.Uint32())
	s.ctx, s.cancel = context.WithCancel(context.Background())
	s.acceptWG.Add(1)
	go s.accept()
	s.log.Info("peer listener ready", "port", s.Port())
	return s, nil
}

// accept takes incoming connections. Serving peers arrives in milestone 4,
// so they are accepted and closed (ADR-0011).
func (s *Session) accept() {
	defer s.acceptWG.Done()
	for {
		c, err := s.ln.Accept()
		if err != nil {
			if s.ctx.Err() != nil {
				return
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			return
		}
		c.Close()
	}
}

// Port returns the bound peer port.
func (s *Session) Port() int { return s.ln.Addr().(*net.TCPAddr).Port }

// PeerID returns the session's peer ID.
func (s *Session) PeerID() [20]byte { return s.peerID }

// Add adds a torrent that downloads into dir and starts it, unless paused.
// It returns ErrExists if the info-hash is already present.
func (s *Session) Add(m *metainfo.MetaInfo, dir string, paused bool) (Stats, error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return Stats{}, errors.New("session: closed")
	}
	if _, ok := s.torrents[m.InfoHash]; ok {
		s.mu.Unlock()
		return Stats{}, ErrExists
	}
	t := newTorrent(s, m, dir, paused, rand.New(rand.NewPCG(s.rng.Uint64(), s.rng.Uint64())))
	s.torrents[m.InfoHash] = t
	s.order = append(s.order, t)
	s.mu.Unlock()
	t.start()
	return t.Stats(), nil
}

// Get returns the stats of the torrent with the given hex info-hash.
func (s *Session) Get(id string) (Stats, bool) {
	var ih [20]byte
	if b, err := hex.DecodeString(id); err != nil || len(b) != 20 {
		return Stats{}, false
	} else {
		copy(ih[:], b)
	}
	s.mu.Lock()
	t := s.torrents[ih]
	s.mu.Unlock()
	if t == nil {
		return Stats{}, false
	}
	return t.Stats(), true
}

// List returns the stats of every torrent, in the order they were added.
func (s *Session) List() []Stats {
	s.mu.Lock()
	ts := append([]*Torrent(nil), s.order...)
	s.mu.Unlock()
	out := make([]Stats, 0, len(ts))
	for _, t := range ts {
		out = append(out, t.Stats())
	}
	return out
}

// Close stops every torrent (sending stopped announces in parallel, each
// bounded by StopTimeout), closes the peers, the files and the listener.
func (s *Session) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	ts := append([]*Torrent(nil), s.order...)
	s.mu.Unlock()
	for _, t := range ts {
		t.stop()
	}
	for _, t := range ts {
		t.wait()
	}
	s.cancel()
	s.ln.Close()
	s.acceptWG.Wait()
	s.disk.Close()
	s.hash.Close()
	for _, t := range ts {
		t.closeStorage()
	}
}
