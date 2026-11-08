package session

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"net"
	"net/netip"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"torrentd/internal/bt/choke"
	"torrentd/internal/bt/metainfo"
	"torrentd/internal/bt/peer"
	"torrentd/internal/bt/piece"
	"torrentd/internal/bt/storage"
	"torrentd/internal/bt/tracker"
	"torrentd/internal/store"
)

// eventQueueSize is the actor's event buffer.
const eventQueueSize = 1024

// torrent is one torrent and its actor (ADR-0015). Fields below "actor
// state" are only touched by the actor goroutine.
type torrent struct {
	s        *session
	m        *metainfo.MetaInfo
	dir      string
	size     int64
	trackers []store.TrackerRecord
	files    []store.FileRecord
	rnd      *rand.Rand

	status    atomic.Pointer[Status]
	persist   atomic.Pointer[persistSnap]
	accepting atomic.Bool // incoming connections are welcome
	events    chan event
	actions   chan evAction
	quit      chan context.Context
	ctx       context.Context // cancelled when the torrent shuts down
	cancel    context.CancelFunc
	wg        sync.WaitGroup // dials, checking, upload helpers, announces
	done      chan struct{}  // closed when the actor has exited
	// stopErr is the stopped-announce error; written before done closes.
	stopErr error

	completed, downloaded, wasted, uploaded atomic.Int64
	checkProgress                           atomic.Uint64 // float64 bits

	// actor state
	state         State
	desired       store.DesiredState
	stopReason    StopReason
	limitOverride bool
	errMsg        string
	addedAt       time.Time
	completedAt   time.Time
	seedingSecs   int64
	queuePos      int
	savedBF       []byte // persisted bitfield (until checking finishes)
	savedFP       []byte // persisted fingerprint JSON
	clean         bool   // fast-resume rules for the next check
	fullRecheck   bool   // next check ignores the saved bitfield
	// hideCheck reports a StartPaused add's local check as paused (the
	// contract test requires Add to return paused).
	hideCheck bool

	layout      piece.Layout
	picker      piece.Picker
	choker      choke.Choker
	store       storage.Storage
	loop        tracker.Loop
	checkCancel context.CancelFunc
	checkGen    int

	peers    map[piece.PeerKey]*peerState
	nextKey  piece.PeerKey
	addrs    map[netip.AddrPort]bool // dialling or connected
	queue    []netip.AddrPort        // known, not yet dialled
	backoff  map[netip.AddrPort]redial
	halfOpen int
	writes   map[int]int  // piece -> accepted writes in flight
	toVerify map[int]bool // pending pieces waiting for writes
	hashing  map[int]bool // pieces being hashed
	ticks    int
	have     *piece.Bitfield // verified pieces (mirror of the picker)
	haveGen  int             // bumped on every verified piece
	haveBF   []byte          // cached wire bitfield
	haveN    int             // haveGen when haveBF was built
}

// redial is the backoff of one address.
type redial struct {
	until time.Time
	n     int
}

// event is a message to the actor.
type event interface{}

type actionKind int

const (
	actPause actionKind = iota
	actResume
	actStopSeeding
)

type (
	evAction struct {
		kind  actionKind
		reply chan error
	}
	evChecked struct {
		gen      int
		st       storage.Storage
		verified []int
		missing  bool
		err      error
	}
	evPeers      struct{ addrs []netip.AddrPort }
	evDialFailed struct{ addr netip.AddrPort }
	evHandshaken struct {
		addr     netip.AddrPort
		nc       net.Conn
		hs       peer.Handshake
		incoming bool
	}
	evMessage struct {
		key piece.PeerKey
		msg peer.Message
	}
	evBlock struct {
		key   piece.PeerKey
		msg   peer.Message
		reply chan bool
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
	evPieceSent struct {
		key piece.PeerKey
		n   int
	}
	evBanned struct{ ip netip.Addr }
)

func newTorrent(s *session, m *metainfo.MetaInfo, dir string) *torrent {
	ctx, cancel := context.WithCancel(context.Background())
	t := &torrent{
		s:        s,
		m:        m,
		dir:      dir,
		size:     m.Info.VisibleLength(),
		rnd:      s.newRand(),
		events:   make(chan event, eventQueueSize),
		actions:  make(chan evAction),
		quit:     make(chan context.Context, 1),
		ctx:      ctx,
		cancel:   cancel,
		done:     make(chan struct{}),
		state:    StateChecking,
		desired:  store.DesiredActive,
		addedAt:  s.clk.Now(),
		clean:    true,
		peers:    map[piece.PeerKey]*peerState{},
		addrs:    map[netip.AddrPort]bool{},
		backoff:  map[netip.AddrPort]redial{},
		writes:   map[int]int{},
		toVerify: map[int]bool{},
		hashing:  map[int]bool{},
		layout: piece.Layout{
			NumPieces:   m.Info.NumPieces(),
			PieceLength: m.Info.PieceLength,
			TotalLength: m.Info.TotalLength,
		},
	}
	t.trackers = t.trackerRecords()
	t.files = t.fileRecords()
	return t
}

// restoreTorrent rebuilds a torrent from its persisted record (§6.1).
func restoreTorrent(s *session, r store.TorrentRecord) (*torrent, error) {
	info, err := metainfo.ParseInfo(r.RawInfo)
	if err != nil {
		return nil, err
	}
	m := &metainfo.MetaInfo{InfoHash: r.InfoHash, RawInfo: r.RawInfo, Info: info}
	for _, tr := range r.Trackers {
		m.Trackers = append(m.Trackers, metainfo.Tracker{URL: tr.URL, Tier: tr.Tier})
	}
	t := newTorrent(s, m, r.DownloadDir)
	t.desired = r.DesiredState
	t.stopReason = StopReason(r.StopReason)
	t.limitOverride = r.LimitOverride
	t.savedBF, t.savedFP = r.Bitfield, r.ResumeFingerprint
	t.uploaded.Store(r.UploadedTotal)
	t.downloaded.Store(r.DownloadedTotal)
	t.wasted.Store(r.WastedTotal)
	t.seedingSecs = r.SeedingSeconds
	t.queuePos = r.QueuePosition
	if !r.AddedAt.IsZero() {
		t.addedAt = r.AddedAt
	}
	t.completedAt = r.CompletedAt
	t.clean = s.clean
	t.publish()
	return t, nil
}

// send delivers ev to the actor unless the torrent is shutting down.
func (t *torrent) send(ev event) bool {
	select {
	case t.events <- ev:
		return true
	case <-t.ctx.Done():
		return false
	}
}

// stop asks the actor to shut down; ctx bounds the stopped announce.
func (t *torrent) stop(ctx context.Context) {
	select {
	case t.quit <- ctx:
	default:
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

// run is the actor goroutine.
func (t *torrent) run() {
	defer close(t.done)
	t.startCheck()
	t.publish()
	tk := t.s.clk.NewTicker(time.Second)
	defer tk.Stop()
	for {
		select {
		case ctx := <-t.quit:
			t.shutdown(ctx)
			return
		case a := <-t.actions:
			err := t.act(a.kind)
			t.publish()
			a.reply <- err
		case ev := <-t.events:
			t.handle(ev)
			if !quietEvent(ev) {
				t.publish()
			}
		case <-tk.C():
			t.tick()
			t.publish()
		}
	}
}

// quietEvent reports events that do not need an immediate Status (the
// per-second tick publishes their counters).
func quietEvent(ev event) bool {
	switch ev.(type) {
	case evBlock, evWritten, evPieceSent, evMessage:
		return true
	}
	return false
}

func (t *torrent) handle(ev event) {
	switch ev := ev.(type) {
	case evChecked:
		t.checked(ev)
	case evPeers:
		t.addPeers(ev.addrs)
	case evDialFailed:
		t.halfOpen--
		delete(t.addrs, ev.addr)
		t.noteEnded(ev.addr)
		t.dialMore()
	case evHandshaken:
		if !ev.incoming {
			t.halfOpen--
		}
		t.connected(ev)
		t.dialMore()
	case evMessage:
		t.message(ev.key, ev.msg)
	case evBlock:
		ev.reply <- t.block(ev.key, ev.msg)
	case evPeerClosed:
		t.peerClosed(ev.key)
	case evWritten:
		t.written(ev)
	case evVerified:
		t.verified(ev)
	case evPieceSent:
		t.pieceSent(ev)
	case evBanned:
		t.dropIP(ev.ip)
	}
}

// tick runs every second: request expiry, choking, seeding time.
func (t *torrent) tick() {
	t.ticks++
	if t.state == StateSeeding {
		t.seedingSecs++
	}
	if !t.active() {
		return
	}
	if t.state == StateDownloading && t.picker != nil {
		touched := map[piece.PeerKey]bool{}
		for _, e := range t.picker.Expire() {
			if p := t.peers[e.Peer]; p != nil {
				_ = p.conn.Send(peer.Message{ID: peer.MsgCancel, Index: e.Block.Index, Begin: e.Block.Begin, Length: e.Block.Length})
			}
			touched[e.Peer] = true
		}
		if len(touched) > 0 {
			t.fillAll()
		}
	}
	if t.ticks%int(choke.RechokeInterval/time.Second) == 0 {
		t.rechoke(false)
	}
	for _, p := range t.peers {
		t.pump(p)
	}
}

// active reports whether the torrent transfers data.
func (t *torrent) active() bool {
	return t.state == StateDownloading || t.state == StateSeeding
}

// complete reports whether every wanted piece is verified.
func (t *torrent) complete() bool { return t.picker != nil && t.picker.Done() }

// act applies a lifecycle action (G18, ADR-0016/0019).
func (t *torrent) act(k actionKind) error {
	switch k {
	case actPause:
		switch t.state {
		case StatePaused:
			return nil
		case StateChecking, StateQueued, StateDownloading:
			t.deactivate()
			t.state = StatePaused
			t.desired = store.DesiredPaused
			t.saveSoon()
			return nil
		}
		return ErrInvalidState
	case actStopSeeding:
		switch t.state {
		case StateStopped:
			return nil
		case StateSeeding:
			t.deactivate()
			t.state = StateStopped
			t.stopReason = StopReasonUser
			t.desired = store.DesiredStopped
			t.saveSoon()
			return nil
		case StateError, StateMoving:
			return ErrInvalidState
		}
		return ErrNotComplete
	case actResume:
		switch t.state {
		case StatePaused, StateStopped:
			if t.state == StateStopped && (t.stopReason == StopReasonRatio || t.stopReason == StopReasonTime) {
				t.limitOverride = true
			}
			t.stopReason = StopReasonNone
			t.desired = store.DesiredActive
			if t.store == nil || t.picker == nil {
				t.startCheck()
			} else {
				t.activate()
			}
			t.saveSoon()
			return nil
		case StateError:
			t.desired = store.DesiredActive
			t.errMsg = ""
			t.fullRecheck = true
			t.picker, t.have = nil, nil
			t.haveGen++
			t.completed.Store(0)
			t.startCheck()
			t.saveSoon()
			return nil
		case StateMoving:
			return ErrInvalidState
		}
		return nil
	}
	return ErrInvalidState
}

// saveSoon publishes the persistence snapshot and asks for a flush.
func (t *torrent) saveSoon() {
	t.persist.Store(t.capture())
	t.s.requestFlush()
}

// activate moves a checked torrent to downloading or seeding and starts
// the network side.
func (t *torrent) activate() {
	if t.complete() {
		t.state = StateSeeding
		if t.completedAt.IsZero() {
			t.completedAt = t.s.clk.Now()
		}
	} else {
		t.state = StateDownloading
	}
	t.accepting.Store(true)
	t.startTracker()
}

// deactivate stops the network side: stopped announce (not awaited),
// every peer closed, checking cancelled.
func (t *torrent) deactivate() {
	t.accepting.Store(false)
	if t.checkCancel != nil {
		t.checkCancel()
		t.checkCancel = nil
		t.checkGen++
	}
	if t.loop != nil {
		loop := t.loop
		t.loop = nil
		t.goJob(func() {
			ctx, cancel := context.WithTimeout(t.ctx, ShutdownAnnounceTimeout)
			defer cancel()
			_ = loop.Stop(ctx)
		})
	}
	for key := range t.peers {
		t.removePeer(key)
	}
	t.queue = nil
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
			return tracker.Stats{Downloaded: t.downloaded.Load(), Uploaded: t.uploaded.Load(), Left: t.size - t.completed.Load()}
		},
		OnPeers: func(addrs []netip.AddrPort) {
			t.send(evPeers{append([]netip.AddrPort(nil), addrs...)})
		},
		Clock:  t.s.clk,
		Logger: t.s.log.With("torrent", t.m.HexHash()),
	})
}

// startCheck runs fast resume / verification in a helper goroutine.
func (t *torrent) startCheck() {
	t.state = StateChecking
	t.checkProgress.Store(0)
	t.checkGen++
	gen := t.checkGen
	ctx, cancel := context.WithCancel(t.ctx)
	t.checkCancel = cancel
	in := storage.ResumeInput{Info: t.m.Info, Clean: t.clean}
	if !t.fullRecheck {
		if t.picker != nil {
			in.Saved = t.picker.Have()
			in.Clean = false
		} else if t.savedBF != nil {
			if bf, err := piece.FromBytes(t.savedBF, t.layout.NumPieces); err == nil {
				in.Saved = bf
			}
		}
		if t.savedFP != nil {
			var fp storage.Fingerprint
			if json.Unmarshal(t.savedFP, &fp) == nil {
				in.SavedFingerprint = fp
			}
		}
	}
	t.fullRecheck = false
	st := t.store
	t.goJob(func() {
		ev := t.check(ctx, in, st)
		ev.gen = gen
		if !t.send(ev) && ev.st != nil && st == nil {
			_ = ev.st.Close()
		}
	})
}

// check is the checking job (helper goroutine).
func (t *torrent) check(ctx context.Context, in storage.ResumeInput, st storage.Storage) evChecked {
	cur, err := storage.Stat(t.dir, t.m.Info)
	if err != nil {
		return evChecked{st: st, err: err}
	}
	in.Current = cur
	plan := storage.PlanResume(in)
	if plan.FilesMissing {
		return evChecked{st: st, missing: true}
	}
	if st == nil {
		if st, err = storage.Open(t.dir, t.m.Info, storage.Options{Pools: t.s.pools}); err != nil {
			return evChecked{err: err}
		}
	}
	var verified []int
	for i := 0; i < t.layout.NumPieces; i++ {
		if plan.Trusted.Has(i) {
			verified = append(verified, i)
		}
	}
	for k, i := range plan.Recheck {
		if ctx.Err() != nil {
			return evChecked{st: st, err: ctx.Err()}
		}
		ok, err := st.VerifyPiece(i)
		if err != nil {
			return evChecked{st: st, err: err}
		}
		if ok {
			verified = append(verified, i)
		}
		t.checkProgress.Store(floatBits(float64(k+1) / float64(len(plan.Recheck))))
	}
	for _, i := range verified {
		if err := st.MarkVerified(i); err != nil {
			return evChecked{st: st, err: err}
		}
	}
	return evChecked{st: st, verified: verified}
}

// checked finishes checking and moves to the desired state.
func (t *torrent) checked(ev evChecked) {
	if ev.st != nil {
		t.store = ev.st
	}
	if ev.gen != t.checkGen {
		return // cancelled by Pause
	}
	t.checkCancel = nil
	t.hideCheck = false
	if ev.missing {
		t.state, t.errMsg = StateError, ErrorFilesMissing
		t.saveSoon()
		return
	}
	if ev.err != nil {
		t.fail(fmt.Errorf("checking: %w", ev.err))
		return
	}
	if t.picker == nil {
		t.picker = piece.New(t.layout, piece.Options{Rand: t.rnd, Clock: t.s.clk})
		t.choker = choke.NewChoker(t.rnd)
	}
	var done int64
	for _, i := range ev.verified {
		t.markHave(i)
	}
	have := t.haveSet()
	for i := 0; i < t.layout.NumPieces; i++ {
		if have.Has(i) {
			done += t.store.WantedBytes(i)
		}
	}
	t.completed.Store(done)
	t.savedBF = nil
	complete := t.complete()
	switch t.desired {
	case store.DesiredPaused:
		if complete {
			t.state, t.stopReason, t.desired = StateStopped, StopReasonUser, store.DesiredStopped
		} else {
			t.state = StatePaused
		}
	case store.DesiredStopped:
		if complete {
			t.state = StateStopped
			if t.stopReason == StopReasonNone {
				t.stopReason = StopReasonUser
			}
		} else {
			t.state, t.stopReason, t.desired = StatePaused, StopReasonNone, store.DesiredPaused
		}
	default:
		t.activate()
	}
	if complete && t.completedAt.IsZero() {
		t.completedAt = t.s.clk.Now()
	}
	t.saveSoon()
}

// fail moves the torrent to state error and drops every peer.
func (t *torrent) fail(err error) {
	t.s.log.Error("torrent failed", "torrent", t.m.HexHash(), "err", err)
	t.deactivate()
	t.state = StateError
	t.errMsg = err.Error()
	t.saveSoon()
}

// finish moves a fully verified torrent to seeding.
func (t *torrent) finish() {
	t.state = StateSeeding
	if t.completedAt.IsZero() {
		t.completedAt = t.s.clk.Now()
	}
	if t.loop != nil {
		t.loop.Completed()
	}
	t.s.log.Info("download complete", "torrent", t.m.HexHash(), "name", t.m.Info.Name)
	for key, p := range t.peers {
		if t.picker.IsSeed(key) {
			t.removePeer(key) // no seed-to-seed connections
			continue
		}
		t.updateInterest(p)
	}
	t.saveSoon()
}

// shutdown stops the tracker loop (stopped announce, bounded by ctx and
// ShutdownAnnounceTimeout) in parallel with closing peers, then waits for
// every helper. Storage is closed by Session.Close after the final flush.
func (t *torrent) shutdown(ctx context.Context) {
	t.accepting.Store(false)
	stopped := make(chan error, 1)
	if t.loop != nil {
		loop := t.loop
		t.loop = nil
		go func() {
			actx, cancel := context.WithTimeout(ctx, ShutdownAnnounceTimeout)
			defer cancel()
			stopped <- loop.Stop(actx)
		}()
	} else {
		stopped <- nil
	}
	if t.checkCancel != nil {
		t.checkCancel()
	}
	conns := make([]peer.Conn, 0, len(t.peers))
	for key, p := range t.peers {
		conns = append(conns, p.conn)
		t.removePeer(key)
	}
	// Keep draining events while peers and jobs finish: read goroutines may
	// wait for a block decision or report a write.
	waited := make(chan struct{})
	go func() {
		for _, c := range conns {
			<-c.Done()
		}
		t.cancel()
		t.wg.Wait()
		close(waited)
	}()
	for {
		select {
		case ev := <-t.events:
			t.drainEvent(ev)
		case <-waited:
			for {
				select {
				case ev := <-t.events:
					t.drainEvent(ev)
				default:
					t.stopErr = <-stopped
					t.persist.Store(t.capture())
					t.publish()
					return
				}
			}
		}
	}
}

// drainEvent handles an event during shutdown: only bookkeeping that the
// final flush relies on.
func (t *torrent) drainEvent(ev event) {
	switch ev := ev.(type) {
	case evBlock:
		ev.reply <- false
	case evHandshaken:
		_ = ev.nc.Close()
	case evChecked:
		if ev.st != nil && t.store == nil {
			t.store = ev.st
		}
	case evVerified:
		if ev.err == nil && ev.ok && t.picker != nil {
			t.markHave(ev.index)
			t.completed.Add(t.store.WantedBytes(ev.index))
		}
	}
}

// publish stores a fresh Status snapshot and persistence snapshot.
func (t *torrent) publish() {
	st := &Status{
		ID:              t.m.InfoHash,
		Name:            t.m.Info.Name,
		State:           t.state,
		StopReason:      t.stopReason,
		Private:         t.m.Info.Private,
		SizeBytes:       t.size,
		CompletedBytes:  t.completed.Load(),
		DownloadedTotal: t.downloaded.Load(),
		UploadedTotal:   t.uploaded.Load(),
		WastedTotal:     t.wasted.Load(),
		DownloadDir:     t.dir,
		AddedAt:         t.addedAt,
		CompletedAt:     t.completedAt,
	}
	if t.state != StateStopped {
		st.StopReason = StopReasonNone
	}
	if t.state == StateChecking && t.hideCheck {
		st.State = StatePaused
	}
	if t.state == StateChecking {
		st.CheckingProgress = floatFrom(t.checkProgress.Load())
	}
	if t.state == StateError {
		st.ErrorMessage = t.errMsg
	}
	if st.SizeBytes > 0 {
		st.Progress = float64(st.CompletedBytes) / float64(st.SizeBytes)
	} else {
		st.Progress = 1
	}
	for key, p := range t.peers {
		st.PeersConnected++
		if t.picker != nil && t.picker.IsSeed(key) {
			st.SeedsConnected++
		}
		st.DownloadRate += p.down.Rate()
		st.UploadRate += p.up.Rate()
	}
	t.status.Store(st)
	t.persist.Store(t.capture())
}
