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
	"runtime"
	"strconv"
	"sync"
	"time"

	"torrentd/internal/bt/metainfo"
	"torrentd/internal/bt/peer"
	"torrentd/internal/bt/storage"
	"torrentd/internal/bt/tracker"
	"torrentd/internal/clock"
)

// storeTimeout bounds one Store call.
const storeTimeout = 10 * time.Second

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

	disk, hash *pool

	randMu sync.Mutex
	rnd    *rand.Rand

	ln        net.Listener
	port      int
	listenErr error

	mu       sync.Mutex
	closed   bool
	torrents map[[20]byte]*torrent
	order    []*torrent
	incoming map[net.Conn]struct{} // connections still in the handshake

	banMu   sync.Mutex
	strikes map[netip.Addr]int
	bans    map[netip.Addr]time.Time // ip -> expiry

	kick      chan struct{} // asks the flusher for an early flush
	flushStop chan struct{}
	flushDone chan struct{}

	wg        sync.WaitGroup // accept loop, handshakes, torrent actors, misc
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
		clean:     cfg.PreviousShutdownClean,
		torrents:  map[[20]byte]*torrent{},
		incoming:  map[net.Conn]struct{}{},
		strikes:   map[netip.Addr]int{},
		bans:      map[netip.Addr]time.Time{},
		kick:      make(chan struct{}, 1),
		flushStop: make(chan struct{}),
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
	dw, hw := cfg.DiskWorkers, cfg.HashWorkers
	if dw <= 0 {
		dw = storage.DefaultDiskWorkers
	}
	if hw <= 0 {
		hw = max(1, runtime.GOMAXPROCS(0)/2)
	}
	s.disk, s.hash = newPool(dw), newPool(hw)
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
	go s.flushLoop()
	return s, nil
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
	desired := DesiredActive
	if opts.StartPaused {
		desired = DesiredPaused
	}
	t := newTorrent(s, m, dir, torrentInit{desired: desired, addedAt: s.clk.Now()})
	s.startLocked(t)
	s.requestFlush()
	return *t.status.Load(), nil
}

// startLocked registers t and starts its actor. s.mu must be held.
func (s *session) startLocked(t *torrent) {
	t.position = len(s.order)
	s.torrents[t.m.InfoHash] = t
	s.order = append(s.order, t)
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		t.run()
	}()
}

// Load restores the saved torrents and bans.
func (s *session) Load(ctx context.Context) error {
	if s.store == nil {
		return nil
	}
	lctx, cancel := context.WithTimeout(ctx, storeTimeout)
	defer cancel()
	bans, err := s.store.LoadBans(lctx, s.clk.Now())
	if err != nil {
		return fmt.Errorf("session: load bans: %w", err)
	}
	s.banMu.Lock()
	for _, b := range bans {
		s.bans[b.IP.Unmap()] = b.ExpiresAt
	}
	s.banMu.Unlock()
	recs, err := s.store.LoadTorrents(lctx)
	if err != nil {
		return fmt.Errorf("session: load torrents: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	for _, r := range recs {
		m, err := r.metaInfo()
		if err != nil {
			s.log.Error("cannot restore torrent", "torrent", hex.EncodeToString(r.InfoHash[:]), "err", err)
			continue
		}
		if _, ok := s.torrents[m.InfoHash]; ok {
			continue
		}
		rec := r
		t := newTorrent(s, m, r.DownloadDir, torrentInit{
			desired:    r.DesiredState,
			stopReason: r.StopReason,
			addedAt:    r.AddedAt,
			restored:   &rec,
		})
		s.startLocked(t)
	}
	s.log.Info("torrents restored", "count", len(recs))
	return nil
}

// metaInfo rebuilds the metainfo of a saved torrent.
func (r Record) metaInfo() (*metainfo.MetaInfo, error) {
	info, err := metainfo.ParseInfo(r.RawInfo)
	if err != nil {
		return nil, err
	}
	return &metainfo.MetaInfo{InfoHash: r.InfoHash, RawInfo: r.RawInfo, Info: info, Trackers: r.Trackers}, nil
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

func (s *session) lookup(id [20]byte) *torrent {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.torrents[id]
}

func (s *session) Get(id [20]byte) (Status, bool) {
	t := s.lookup(id)
	if t == nil {
		return Status{}, false
	}
	return *t.status.Load(), true
}

func (s *session) Pause(id [20]byte) error       { return s.command(id, cmdPause) }
func (s *session) Resume(id [20]byte) error      { return s.command(id, cmdResume) }
func (s *session) StopSeeding(id [20]byte) error { return s.command(id, cmdStopSeeding) }

// command runs a lifecycle command on the torrent's actor and waits for
// its verdict.
func (s *session) command(id [20]byte, c cmdKind) error {
	t := s.lookup(id)
	if t == nil {
		return ErrNotFound
	}
	reply := make(chan error, 1)
	if !t.send(evCommand{kind: c, reply: reply}) {
		return ErrClosed
	}
	select {
	case err := <-reply:
		if err == nil {
			s.requestFlush()
		}
		return err
	case <-t.done:
		return ErrClosed
	}
}

// banned reports whether ip is banned now.
func (s *session) banned(ip netip.Addr) bool {
	s.banMu.Lock()
	defer s.banMu.Unlock()
	exp, ok := s.bans[ip.Unmap()]
	if !ok {
		return false
	}
	if !s.clk.Now().Before(exp) {
		delete(s.bans, ip.Unmap())
		delete(s.strikes, ip.Unmap())
		return false
	}
	return true
}

// strike records a strike against ip and reports whether it is now banned
// (for the first time).
func (s *session) strike(ip netip.Addr, from *torrent) bool {
	ip = ip.Unmap()
	s.banMu.Lock()
	if _, ok := s.bans[ip]; ok {
		s.banMu.Unlock()
		return false
	}
	s.strikes[ip]++
	if s.strikes[ip] < BanStrikes {
		s.banMu.Unlock()
		return false
	}
	now := s.clk.Now()
	b := Ban{IP: ip, Reason: fmt.Sprintf("%d pieces failed the hash check", BanStrikes), BannedAt: now, ExpiresAt: now.Add(BanDuration)}
	s.bans[ip] = b.ExpiresAt
	s.banMu.Unlock()
	s.log.Warn("peer banned", "ip", ip, "until", b.ExpiresAt)
	s.mu.Lock()
	others := make([]*torrent, 0, len(s.order))
	for _, t := range s.order {
		if t != from {
			others = append(others, t)
		}
	}
	closed := s.closed
	if !closed {
		s.wg.Add(1)
	}
	s.mu.Unlock()
	if closed {
		return true
	}
	// Tell the other torrents and persist the ban off the actor goroutine.
	go func() {
		defer s.wg.Done()
		for _, t := range others {
			t.send(evBanned{ip})
		}
		if s.store != nil {
			ctx, cancel := context.WithTimeout(context.Background(), storeTimeout)
			defer cancel()
			if err := s.store.SaveBan(ctx, b); err != nil {
				s.log.Error("save ban", "ip", ip, "err", err)
			}
		}
	}()
	return true
}

// requestFlush asks the flusher to save soon.
func (s *session) requestFlush() {
	select {
	case s.kick <- struct{}{}:
	default:
	}
}

// flushLoop saves every torrent every FlushInterval and on request.
func (s *session) flushLoop() {
	defer close(s.flushDone)
	if s.store == nil {
		<-s.flushStop
		return
	}
	tk := s.clk.NewTicker(FlushInterval)
	defer tk.Stop()
	for {
		select {
		case <-s.flushStop:
			return
		case <-tk.C():
		case <-s.kick:
		}
		s.flush()
	}
}

// flush snapshots every torrent, fsyncs its data, and saves the records.
func (s *session) flush() {
	s.mu.Lock()
	ts := append([]*torrent(nil), s.order...)
	s.mu.Unlock()
	recs := make([]Record, 0, len(ts))
	for _, t := range ts {
		snap, ok := t.snapshot()
		if !ok {
			continue
		}
		if snap.st != nil {
			if err := snap.st.Sync(); err != nil {
				if !errors.Is(err, storage.ErrClosed) {
					s.log.Error("sync torrent data", "torrent", t.m.HexHash(), "err", err)
				}
				continue // never save a bitfield whose data is not on disk
			}
			snap.rec.Fingerprint = fingerprint(t.m.Info, t.dir)
		}
		recs = append(recs, snap.rec)
	}
	s.save(recs)
}

func (s *session) save(recs []Record) {
	if s.store == nil || len(recs) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), storeTimeout)
	defer cancel()
	if err := s.store.SaveTorrents(ctx, recs); err != nil {
		s.log.Error("save torrents", "err", err)
	}
}

func (s *session) Close(ctx context.Context) error {
	s.closeOnce.Do(func() { s.closeErr = s.close(ctx) })
	return s.closeErr
}

func (s *session) close(ctx context.Context) error {
	s.mu.Lock()
	s.closed = true
	ts := append([]*torrent(nil), s.order...)
	for c := range s.incoming {
		_ = c.Close()
	}
	s.mu.Unlock()
	if s.ln != nil {
		_ = s.ln.Close()
	}
	close(s.flushStop)
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
	s.disk.close()
	s.hash.close()
	var errs []error
	var recs []Record
	for _, t := range ts {
		if t.stopErr != nil {
			errs = append(errs, t.stopErr)
		}
		if t.final != nil {
			recs = append(recs, *t.final)
		}
	}
	s.save(recs)
	if len(errs) > 0 {
		// A failed stopped announce is logged, not fatal: the trackers
		// will time the peer out (§6.2).
		s.log.Warn("stopped announces failed", "err", errors.Join(errs...))
	}
	return nil
}
