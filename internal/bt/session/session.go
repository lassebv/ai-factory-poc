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
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"torrentd/internal/bt/metainfo"
	"torrentd/internal/bt/peer"
	"torrentd/internal/bt/storage"
	"torrentd/internal/bt/tracker"
	"torrentd/internal/clock"
	"torrentd/internal/store"
)

// maxIncomingHandshakes bounds concurrent incoming handshakes process-wide.
const maxIncomingHandshakes = 64

// session implements Session.
type session struct {
	clk     clock.Clock
	defDir  string
	log     *slog.Logger
	peerID  [20]byte
	key     string
	tracker tracker.Client
	store   Store
	clean   bool
	pools   *storage.Pools

	randMu sync.Mutex
	rnd    *rand.Rand

	ln        net.Listener
	port      int
	listenErr error
	acceptSem chan struct{}

	ctx    context.Context // cancelled by Close
	cancel context.CancelFunc

	mu       sync.Mutex
	closed   bool
	torrents map[[20]byte]*torrent
	order    []*torrent

	banMu   sync.Mutex
	strikes map[netip.Addr]int
	banned  map[netip.Addr]time.Time

	flushReq  chan struct{}
	flushDone chan struct{}
	saveMu    sync.Mutex // serializes flushes

	wg        sync.WaitGroup // accept loop, incoming handshakes, actors
	closeOnce sync.Once
	closeErr  error
}

func newSession(cfg Config) (Session, error) {
	if cfg.PeerPort < 0 || cfg.PeerPort > 65535 {
		return nil, fmt.Errorf("session: invalid peer port %d", cfg.PeerPort)
	}
	s := &session{
		clk:       cfg.Clock,
		defDir:    cfg.DefaultDownloadDir,
		log:       cfg.Logger,
		rnd:       cfg.Rand,
		store:     cfg.Store,
		clean:     cfg.PreviousClean,
		torrents:  map[[20]byte]*torrent{},
		strikes:   map[netip.Addr]int{},
		banned:    map[netip.Addr]time.Time{},
		acceptSem: make(chan struct{}, maxIncomingHandshakes),
		flushReq:  make(chan struct{}, 1),
		flushDone: make(chan struct{}),
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
	s.ctx, s.cancel = context.WithCancel(context.Background())

	var recs []store.TorrentRecord
	if s.store != nil {
		bans, err := s.store.LoadBans(s.ctx, s.clk.Now())
		if err != nil {
			s.cancel()
			return nil, fmt.Errorf("session: load bans: %w", err)
		}
		for _, b := range bans {
			if a, err := netip.ParseAddr(b.IP); err == nil {
				s.banned[normAddr(a)] = b.ExpiresAt
			}
		}
		if recs, err = s.store.LoadTorrents(s.ctx); err != nil {
			s.cancel()
			return nil, fmt.Errorf("session: load torrents: %w", err)
		}
	}
	s.pools = storage.NewPools(cfg.DiskWorkers, cfg.HashWorkers)

	ln, err := net.Listen("tcp", ":"+strconv.Itoa(cfg.PeerPort))
	if err != nil {
		s.listenErr = fmt.Errorf("session: listen on peer port %d: %w", cfg.PeerPort, err)
		s.log.Warn("could not bind the peer port; only outgoing connections will work", "port", cfg.PeerPort, "err", err)
	} else {
		s.ln = ln
		s.port = ln.Addr().(*net.TCPAddr).Port
	}

	for _, r := range recs {
		t, err := restoreTorrent(s, r)
		if err != nil {
			s.log.Error("cannot restore torrent; skipped", "torrent", hex.EncodeToString(r.InfoHash[:]), "err", err)
			continue
		}
		s.start(t)
	}
	if s.ln != nil {
		s.wg.Add(1)
		go s.acceptLoop()
	}
	go s.flushLoop()
	return s, nil
}

// normAddr unmaps IPv4-mapped addresses and drops zones.
func normAddr(a netip.Addr) netip.Addr { return a.Unmap().WithZone("") }

// start registers t and runs its actor. s.mu must not be held... the
// caller guarantees t is not registered yet.
func (s *session) start(t *torrent) {
	s.torrents[t.m.InfoHash] = t
	s.order = append(s.order, t)
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		t.run()
	}()
}

// newRand derives an independent random source for one torrent.
func (s *session) newRand() *rand.Rand {
	s.randMu.Lock()
	defer s.randMu.Unlock()
	return rand.New(rand.NewPCG(s.rnd.Uint64(), s.rnd.Uint64()))
}

func (s *session) withRand(f func(*rand.Rand)) {
	s.randMu.Lock()
	defer s.randMu.Unlock()
	f(s.rnd)
}

// acceptLoop accepts incoming peer connections.
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
		ap, _ := netip.ParseAddrPort(c.RemoteAddr().String())
		ap = netip.AddrPortFrom(normAddr(ap.Addr()), ap.Port())
		if s.isBanned(ap.Addr()) {
			_ = c.Close()
			continue
		}
		select {
		case s.acceptSem <- struct{}{}:
		default:
			_ = c.Close() // too many handshakes in flight
			continue
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			defer func() { <-s.acceptSem }()
			s.incoming(c, ap)
		}()
	}
}

// incoming performs the handshake of an accepted connection and hands it
// to its torrent.
func (s *session) incoming(c net.Conn, ap netip.AddrPort) {
	ctx, cancel := context.WithTimeout(s.ctx, DialTimeout)
	defer cancel()
	var target *torrent
	hs, err := peer.AcceptHandshake(ctx, c, func(ih [20]byte) (peer.Handshake, bool) {
		s.mu.Lock()
		t := s.torrents[ih]
		s.mu.Unlock()
		if t == nil || !t.accepting.Load() {
			return peer.Handshake{}, false
		}
		target = t
		return peer.Handshake{InfoHash: ih, PeerID: s.peerID}, true
	})
	if err != nil {
		_ = c.Close()
		return
	}
	if !target.send(evHandshaken{addr: ap, nc: c, hs: hs, incoming: true}) {
		_ = c.Close()
	}
}

// isBanned reports whether ip is currently banned.
func (s *session) isBanned(ip netip.Addr) bool {
	ip = normAddr(ip)
	s.banMu.Lock()
	defer s.banMu.Unlock()
	until, ok := s.banned[ip]
	if !ok {
		return false
	}
	if !s.clk.Now().Before(until) {
		delete(s.banned, ip)
		return false
	}
	return true
}

// strike records one hash-failure strike for ip and reports whether it is
// now banned.
func (s *session) strike(ip netip.Addr) bool {
	ip = normAddr(ip)
	if !ip.IsValid() {
		return false
	}
	s.banMu.Lock()
	s.strikes[ip]++
	if s.strikes[ip] < BanStrikes {
		s.banMu.Unlock()
		return false
	}
	delete(s.strikes, ip)
	now := s.clk.Now()
	s.banned[ip] = now.Add(BanDuration)
	s.banMu.Unlock()
	s.log.Warn("peer banned after repeated hash failures", "ip", ip, "strikes", BanStrikes)
	if s.store != nil {
		b := store.Ban{IP: ip.String(), Reason: "hash failures", BannedAt: now, ExpiresAt: now.Add(BanDuration)}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			if err := s.store.SaveBan(context.Background(), b); err != nil {
				s.log.Error("cannot persist ban", "ip", b.IP, "err", err)
			}
		}()
	}
	return true
}

// broadcastBan tells every torrent but from to drop connections from ip.
func (s *session) broadcastBan(from *torrent, ip netip.Addr) {
	s.mu.Lock()
	ts := append([]*torrent(nil), s.order...)
	s.mu.Unlock()
	for _, t := range ts {
		if t == from {
			continue
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			t.send(evBanned{ip: ip})
		}()
	}
}

func (s *session) PeerPort() int    { return s.port }
func (s *session) ListenErr() error { return s.listenErr }
func (s *session) PeerID() [20]byte { return s.peerID }

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

func (s *session) Add(ctx context.Context, m *metainfo.MetaInfo, opts AddOptions) (Status, error) {
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
	t := newTorrent(s, m, dir)
	t.queuePos = len(s.order)
	if opts.StartPaused {
		t.desired = store.DesiredPaused
		t.hideCheck = true
	}
	t.publish()
	if s.store != nil {
		if err := s.store.SaveTorrents(ctx, []store.TorrentRecord{t.persist.Load().rec}); err != nil {
			return Status{}, fmt.Errorf("session: persist torrent: %w", err)
		}
	}
	s.start(t)
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

// action runs a lifecycle action on the torrent's actor and waits for it.
func (s *session) action(ctx context.Context, id [20]byte, kind actionKind) error {
	s.mu.Lock()
	t, ok := s.torrents[id]
	closed := s.closed
	s.mu.Unlock()
	if closed {
		return ErrClosed
	}
	if !ok {
		return ErrNotFound
	}
	reply := make(chan error, 1)
	select {
	case t.actions <- evAction{kind: kind, reply: reply}:
	case <-t.done:
		return ErrClosed
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case err := <-reply:
		return err
	case <-t.done:
		return ErrClosed
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *session) Pause(ctx context.Context, id [20]byte) error {
	return s.action(ctx, id, actPause)
}

func (s *session) Resume(ctx context.Context, id [20]byte) error {
	return s.action(ctx, id, actResume)
}

func (s *session) StopSeeding(ctx context.Context, id [20]byte) error {
	return s.action(ctx, id, actStopSeeding)
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
	s.cancel() // aborts incoming handshakes and stops the flusher
	<-s.flushDone
	for _, t := range ts {
		t.stop(ctx)
	}
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		return fmt.Errorf("session: close: %w", ctx.Err())
	}
	s.flush(context.WithoutCancel(ctx))
	var errs []error
	for _, t := range ts {
		if t.stopErr != nil {
			errs = append(errs, t.stopErr)
		}
		if t.store != nil {
			_ = t.store.Close()
		}
	}
	s.pools.Close()
	if len(errs) > 0 {
		// A failed stopped announce is logged, not fatal: the trackers
		// will time the peer out (§6.2).
		s.log.Warn("stopped announces failed", "err", errors.Join(errs...))
	}
	return nil
}
