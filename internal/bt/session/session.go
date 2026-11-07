package session

import (
	"context"
	crand "crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync"

	"torrentd/internal/bt/metainfo"
	"torrentd/internal/bt/peer"
	"torrentd/internal/bt/tracker"
	"torrentd/internal/clock"
)

// session implements Session.
type session struct {
	clk     clock.Clock
	defDir  string
	log     *slog.Logger
	peerID  [20]byte
	key     string
	tracker tracker.Client

	randMu sync.Mutex
	rnd    *rand.Rand

	ln        net.Listener
	port      int
	listenErr error

	mu       sync.Mutex
	closed   bool
	torrents map[[20]byte]*torrent
	order    []*torrent

	wg        sync.WaitGroup // accept loop and torrent actors
	closeOnce sync.Once
	closeErr  error
}

func newSession(cfg Config) (Session, error) {
	if cfg.PeerPort < 0 || cfg.PeerPort > 65535 {
		return nil, fmt.Errorf("session: invalid peer port %d", cfg.PeerPort)
	}
	s := &session{
		clk:      cfg.Clock,
		defDir:   cfg.DefaultDownloadDir,
		log:      cfg.Logger,
		rnd:      cfg.Rand,
		torrents: map[[20]byte]*torrent{},
	}
	if s.clk == nil {
		s.clk = clock.Real()
	}
	if s.log == nil {
		s.log = slog.Default()
	}
	if s.rnd == nil {
		var seed [32]byte
		if _, err := crand.Read(seed[:]); err != nil {
			return nil, fmt.Errorf("session: seed randomness: %w", err)
		}
		s.rnd = rand.New(rand.NewChaCha8(seed))
	}
	s.peerID = peer.NewPeerID(s.rnd)
	var k [8]byte
	binary.LittleEndian.PutUint64(k[:], s.rnd.Uint64())
	s.key = hex.EncodeToString(k[:])
	s.tracker = tracker.NewHTTPClient(cfg.HTTPClient)

	ln, err := net.Listen("tcp", ":"+strconv.Itoa(cfg.PeerPort))
	if err != nil {
		s.listenErr = fmt.Errorf("session: listen on peer port %d: %w", cfg.PeerPort, err)
		s.log.Warn("could not bind the peer port; only outgoing connections will work", "port", cfg.PeerPort, "err", err)
	} else {
		s.ln = ln
		s.port = ln.Addr().(*net.TCPAddr).Port
		s.wg.Add(1)
		go s.acceptLoop()
	}
	return s, nil
}

// acceptLoop accepts incoming peer connections and closes them (seeding is
// M4, ADR-0015 §4).
func (s *session) acceptLoop() {
	defer s.wg.Done()
	for {
		c, err := s.ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			s.log.Debug("accept failed", "err", err)
			return
		}
		_ = c.Close()
	}
}

func (s *session) PeerPort() int    { return s.port }
func (s *session) ListenErr() error { return s.listenErr }
func (s *session) PeerID() [20]byte { return s.peerID }
func (s *session) withRand(f func(*rand.Rand)) {
	s.randMu.Lock()
	defer s.randMu.Unlock()
	f(s.rnd)
}

// resolveDir validates and creates the download dir.
func resolveDir(dir string) (string, error) {
	if dir == "" || !filepath.IsAbs(dir) {
		return "", fmt.Errorf("%w: %q is not an absolute path", ErrInvalidDownloadDir, dir)
	}
	dir = filepath.Clean(dir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("%w: %v", ErrInvalidDownloadDir, err)
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return "", fmt.Errorf("%w: %q is not a directory", ErrInvalidDownloadDir, dir)
	}
	return dir, nil
}

func (s *session) Add(_ context.Context, m *metainfo.MetaInfo, opts AddOptions) (Status, error) {
	if m == nil || m.Info == nil {
		return Status{}, errors.New("session: no metainfo")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return Status{}, ErrClosed
	}
	if _, ok := s.torrents[m.InfoHash]; ok {
		return Status{}, ErrAlreadyExists
	}
	dir := opts.DownloadDir
	if dir == "" {
		dir = s.defDir
	}
	dir, err := resolveDir(dir)
	if err != nil {
		return Status{}, err
	}
	t := newTorrent(s, m, dir, opts.StartPaused)
	s.torrents[m.InfoHash] = t
	s.order = append(s.order, t)
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		t.run()
	}()
	return *t.status.Load(), nil
}

func (s *session) List() []Status {
	s.mu.Lock()
	ts := append([]*torrent(nil), s.order...)
	s.mu.Unlock()
	out := make([]Status, 0, len(ts))
	for _, t := range ts {
		out = append(out, *t.status.Load())
	}
	return out
}

func (s *session) Get(id [20]byte) (Status, bool) {
	s.mu.Lock()
	t, ok := s.torrents[id]
	s.mu.Unlock()
	if !ok {
		return Status{}, false
	}
	return *t.status.Load(), true
}

func (s *session) Close(ctx context.Context) error {
	s.closeOnce.Do(func() { s.closeErr = s.close(ctx) })
	return s.closeErr
}

func (s *session) close(ctx context.Context) error {
	s.mu.Lock()
	s.closed = true
	ts := append([]*torrent(nil), s.order...)
	s.mu.Unlock()
	if s.ln != nil {
		_ = s.ln.Close()
	}
	for _, t := range ts {
		t.stop(ctx)
	}
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	var errs []error
	select {
	case <-done:
	case <-ctx.Done():
		return fmt.Errorf("session: close: %w", ctx.Err())
	}
	for _, t := range ts {
		if t.stopErr != nil {
			errs = append(errs, t.stopErr)
		}
	}
	if len(errs) > 0 {
		// A failed stopped announce is logged, not fatal: the trackers
		// will time the peer out (§6.2).
		s.log.Warn("stopped announces failed", "err", errors.Join(errs...))
	}
	return nil
}
