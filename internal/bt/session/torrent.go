package session

import (
	"context"
	"fmt"
	"math/rand/v2"
	"net"
	"net/netip"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"torrentd/internal/bt/metainfo"
	"torrentd/internal/bt/peer"
	"torrentd/internal/bt/piece"
	"torrentd/internal/bt/storage"
	"torrentd/internal/bt/tracker"
)

// Connection limits per torrent (§4.1 Connection limits).
const (
	maxPeers       = 50
	maxHalfOpen    = 20
	dialTimeout    = 10 * time.Second
	eventQueueSize = 1024
)

// torrent is one torrent and its actor (ADR-0015). Fields below "actor
// state" are only touched by the actor goroutine.
type torrent struct {
	s       *session
	m       *metainfo.MetaInfo
	dir     string
	paused  bool
	addedAt time.Time
	size    int64

	status atomic.Pointer[Status]
	events chan event
	quit   chan context.Context
	ctx    context.Context // cancelled when the torrent stops
	cancel context.CancelFunc
	wg     sync.WaitGroup // dials and storage jobs
	done   chan struct{}  // closed when the actor has exited
	// stopErr is the stopped-announce error; written by the actor before
	// done is closed.
	stopErr error

	completed, downloaded, wasted atomic.Int64

	// actor state
	state       State
	errMsg      string
	completedAt time.Time
	layout      piece.Layout
	picker      piece.Picker
	store       storage.Storage
	loop        tracker.Loop
	peers       map[piece.PeerKey]*peerState
	nextKey     piece.PeerKey
	addrs       map[netip.AddrPort]bool // dialling or connected
	queue       []netip.AddrPort        // known, not yet dialled
	halfOpen    int
	writes      map[int]int  // piece -> writes in flight
	toVerify    map[int]bool // piece complete, waiting for writes
}

// event is a message to the actor.
type event interface{}

type (
	evOpened struct {
		st  storage.Storage
		err error
	}
	evPeers      struct{ addrs []netip.AddrPort }
	evDialFailed struct{ addr netip.AddrPort }
	evHandshaken struct {
		addr netip.AddrPort
		nc   net.Conn
		hs   peer.Handshake
	}
	evMessage struct {
		key piece.PeerKey
		msg peer.Message
	}
	evPeerClosed struct {
		key piece.PeerKey
		err error
	}
	evWritten struct {
		index int
		err   error
	}
	evVerified struct {
		index int
		ok    bool
		err   error
	}
)

func newTorrent(s *session, m *metainfo.MetaInfo, dir string, paused bool) *torrent {
	ctx, cancel := context.WithCancel(context.Background())
	t := &torrent{
		s:        s,
		m:        m,
		dir:      dir,
		paused:   paused,
		addedAt:  s.clk.Now(),
		size:     m.Info.VisibleLength(),
		events:   make(chan event, eventQueueSize),
		quit:     make(chan context.Context, 1),
		ctx:      ctx,
		cancel:   cancel,
		done:     make(chan struct{}),
		state:    StateChecking,
		peers:    map[piece.PeerKey]*peerState{},
		addrs:    map[netip.AddrPort]bool{},
		writes:   map[int]int{},
		toVerify: map[int]bool{},
		layout: piece.Layout{
			NumPieces:   m.Info.NumPieces(),
			PieceLength: m.Info.PieceLength,
			TotalLength: m.Info.TotalLength,
		},
	}
	if paused {
		t.state = StatePaused
	}
	t.publish()
	return t
}

// send delivers ev to the actor unless the torrent is stopping. It reports
// whether ev was delivered.
func (t *torrent) send(ev event) bool {
	select {
	case t.events <- ev:
		return true
	case <-t.ctx.Done():
		return false
	}
}

// stop asks the actor to shut down; ctx bounds the stopped announce. It
// does not wait (Session.Close waits for every actor).
func (t *torrent) stop(ctx context.Context) {
	select {
	case t.quit <- ctx:
	default:
	}
	t.cancel()
}

// run is the actor goroutine.
func (t *torrent) run() {
	defer close(t.done)
	if !t.paused {
		t.goJob(func() {
			st, err := storage.Open(t.dir, t.m.Info, storage.Options{})
			if !t.send(evOpened{st, err}) && st != nil {
				_ = st.Close()
			}
		})
	}
	for {
		select {
		case ctx := <-t.quit:
			t.shutdown(ctx)
			return
		case ev := <-t.events:
			t.handle(ev)
			t.publish()
		}
	}
}

// goJob runs f in a goroutine tracked by t.wg.
func (t *torrent) goJob(f func()) {
	t.wg.Add(1)
	go func() {
		defer t.wg.Done()
		f()
	}()
}

func (t *torrent) handle(ev event) {
	switch ev := ev.(type) {
	case evOpened:
		t.opened(ev)
	case evPeers:
		t.addPeers(ev.addrs)
	case evDialFailed:
		t.halfOpen--
		delete(t.addrs, ev.addr)
		t.dialMore()
	case evHandshaken:
		t.halfOpen--
		t.connected(ev)
		t.dialMore()
	case evMessage:
		t.message(ev.key, ev.msg)
	case evPeerClosed:
		t.peerClosed(ev.key, ev.err)
	case evWritten:
		t.written(ev)
	case evVerified:
		t.verified(ev)
	}
}

// opened finishes checking (storage is preallocated) and starts
// downloading.
func (t *torrent) opened(ev evOpened) {
	if ev.err != nil {
		t.fail(fmt.Errorf("open storage: %w", ev.err))
		return
	}
	t.store = ev.st
	t.picker = piece.NewPicker(t.layout, nil)
	t.state = StateDownloading
	if t.layout.NumPieces == 0 {
		t.finish()
	}
	t.startTracker()
}

func (t *torrent) startTracker() {
	byTier := map[int][]string{}
	var tiers []int
	for _, tr := range t.m.Trackers {
		if _, ok := byTier[tr.Tier]; !ok {
			tiers = append(tiers, tr.Tier)
		}
		byTier[tr.Tier] = append(byTier[tr.Tier], tr.URL)
	}
	sort.Ints(tiers)
	var urls [][]string
	for _, n := range tiers {
		urls = append(urls, byTier[n])
	}
	var ann tracker.Announcer
	t.s.withRand(func(r *rand.Rand) { ann = tracker.NewAnnouncer(urls, t.s.tracker, r) })
	t.loop = tracker.StartLoop(tracker.LoopConfig{
		Announcer: ann,
		InfoHash:  t.m.InfoHash,
		PeerID:    t.s.peerID,
		Port:      uint16(t.s.port),
		Key:       t.s.key,
		Stats: func() tracker.Stats {
			return tracker.Stats{Downloaded: t.downloaded.Load(), Left: t.size - t.completed.Load()}
		},
		OnPeers: func(addrs []netip.AddrPort) {
			t.send(evPeers{append([]netip.AddrPort(nil), addrs...)})
		},
		Clock:  t.s.clk,
		Logger: t.s.log.With("torrent", t.m.HexHash()),
	})
	if t.state == StateSeeding {
		t.loop.Completed()
	}
}

// fail moves the torrent to state error and drops every peer.
func (t *torrent) fail(err error) {
	t.s.log.Error("torrent failed", "torrent", t.m.HexHash(), "err", err)
	t.state = StateError
	t.errMsg = err.Error()
	for _, p := range t.peers {
		_ = p.conn.Close()
	}
}

// finish moves a fully verified torrent to seeding.
func (t *torrent) finish() {
	t.state = StateSeeding
	t.completedAt = t.s.clk.Now()
	if t.loop != nil {
		t.loop.Completed()
	}
	t.s.log.Info("download complete", "torrent", t.m.HexHash(), "name", t.m.Info.Name)
	for _, p := range t.peers {
		if p.interested {
			p.interested = false
			_ = p.conn.Send(peer.Message{ID: peer.MsgNotInterested})
		}
	}
}

// written handles a finished block write.
func (t *torrent) written(ev evWritten) {
	t.writes[ev.index]--
	if t.writes[ev.index] <= 0 {
		delete(t.writes, ev.index)
	}
	if ev.err != nil {
		if t.state != StateError {
			t.fail(fmt.Errorf("write piece %d: %w", ev.index, ev.err))
		}
		return
	}
	t.maybeVerify(ev.index)
}

// maybeVerify hashes a complete piece once all of its writes are done.
func (t *torrent) maybeVerify(index int) {
	if !t.toVerify[index] || t.writes[index] > 0 || t.state == StateError {
		return
	}
	delete(t.toVerify, index)
	st := t.store
	t.goJob(func() {
		ok, err := st.VerifyPiece(index)
		if err == nil && ok {
			err = st.MarkVerified(index)
		}
		t.send(evVerified{index, ok, err})
	})
}

func (t *torrent) verified(ev evVerified) {
	if t.state == StateError {
		return
	}
	if ev.err != nil {
		t.fail(fmt.Errorf("verify piece %d: %w", ev.index, ev.err))
		return
	}
	if !ev.ok {
		t.s.log.Warn("piece failed the hash check", "torrent", t.m.HexHash(), "piece", ev.index)
		t.picker.PieceFailed(ev.index)
		t.wasted.Add(t.store.PieceSize(ev.index))
		t.fillAll()
		return
	}
	t.picker.PieceVerified(ev.index)
	t.completed.Add(t.store.WantedBytes(ev.index))
	for _, p := range t.peers {
		_ = p.conn.Send(peer.Message{ID: peer.MsgHave, Index: uint32(ev.index)})
	}
	if t.picker.Done() && t.state == StateDownloading {
		t.finish()
	}
}

// shutdown stops the tracker loop (stopped announce, bounded by ctx and
// ShutdownAnnounceTimeout) in parallel with closing peers and storage.
func (t *torrent) shutdown(ctx context.Context) {
	t.cancel()
	stopped := make(chan error, 1)
	if t.loop != nil {
		go func() {
			actx, cancel := context.WithTimeout(ctx, ShutdownAnnounceTimeout)
			defer cancel()
			stopped <- t.loop.Stop(actx)
		}()
	} else {
		stopped <- nil
	}
	for _, p := range t.peers {
		_ = p.conn.Close()
	}
	for _, p := range t.peers {
		<-p.conn.Done()
	}
	t.wg.Wait()
	if t.store != nil {
		_ = t.store.Close()
	}
	t.stopErr = <-stopped
	t.peers = map[piece.PeerKey]*peerState{}
	t.publish()
}

// publish stores a fresh Status snapshot.
func (t *torrent) publish() {
	st := &Status{
		ID:              t.m.InfoHash,
		Name:            t.m.Info.Name,
		State:           t.state,
		Private:         t.m.Info.Private,
		SizeBytes:       t.size,
		CompletedBytes:  t.completed.Load(),
		DownloadedTotal: t.downloaded.Load(),
		WastedTotal:     t.wasted.Load(),
		DownloadDir:     t.dir,
		AddedAt:         t.addedAt,
		CompletedAt:     t.completedAt,
	}
	if t.state == StateError {
		st.ErrorMessage = t.errMsg
	}
	if st.SizeBytes > 0 {
		st.Progress = float64(st.CompletedBytes) / float64(st.SizeBytes)
	} else {
		st.Progress = 1
	}
	for _, p := range t.peers {
		st.PeersConnected++
		if p.have != nil && p.have.Full() {
			st.SeedsConnected++
		}
		st.DownloadRate += p.rate(t.s.clk.Now())
	}
	t.status.Store(st)
}
