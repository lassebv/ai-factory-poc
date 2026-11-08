package session

import (
	"context"
	"errors"
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

// Connection limits per torrent (§4.1 Connection limits) and actor sizing.
const (
	maxPeers       = 50
	maxHalfOpen    = 20
	dialTimeout    = 10 * time.Second
	eventQueueSize = 1024
	// maxPendingWrites bounds received blocks waiting for the disk pool
	// (bounded memory, §11); the pipeline pauses above it.
	maxPendingWrites = 512
	tickInterval     = time.Second
)

// torrentInit is how a torrent starts: added now or restored from a Record.
type torrentInit struct {
	desired    string
	stopReason StopReason
	addedAt    time.Time
	restored   *Record
}

// torrent is one torrent and its actor (ADR-0015). Fields below "actor
// state" are only touched by the actor goroutine.
type torrent struct {
	s        *session
	m        *metainfo.MetaInfo
	dir      string
	addedAt  time.Time
	size     int64
	layout   piece.Layout
	position int

	status    atomic.Pointer[Status]
	accepting atomic.Bool // incoming connections are welcome
	events    chan event
	quit      chan context.Context
	ctx       context.Context // cancelled when the torrent shuts down
	cancel    context.CancelFunc
	wg        sync.WaitGroup // dials, pool jobs, tracker stops
	done      chan struct{}  // closed when the actor has exited
	// stopErr and final are written by the actor before done is closed.
	stopErr error
	final   *Record

	completed, downloaded, uploaded, wasted atomic.Int64

	// actor state
	state       State
	desired     string
	stopReason  StopReason
	errMsg      string
	completedAt time.Time
	picker      piece.Picker
	store       storage.Storage
	restored    *Record // resume data not yet validated against the files
	fprint      string  // last known fingerprint (kept while storage is closed)
	opening     bool    // an open job is in flight
	ready       bool    // storage open and the verified set is known
	needCheck   bool    // every piece must be rechecked before running
	checking    int     // verify jobs in flight during checking
	gen         int     // bumped on every stop; stale job results are ignored
	loop        tracker.Loop
	peers       map[piece.PeerKey]*peerState
	closing     map[piece.PeerKey]peer.Conn // closed, waiting for Done
	nextKey     piece.PeerKey
	addrs       map[netip.AddrPort]bool // dialling or connected
	queue       []netip.AddrPort        // known, not yet dialled
	halfOpen    int
	writes      map[int]int  // piece -> writes in flight
	toVerify    map[int]bool // piece complete, waiting for writes
	pending     int          // block writes queued or in flight
	throttled   bool         // fill skipped because pending was full
	contrib     map[int]map[netip.Addr]struct{}
	optimistic  piece.PeerKey
	optSince    time.Time
	lastRechoke time.Time
}

// event is a message to the actor.
type event interface{}

type cmdKind int

const (
	cmdPause cmdKind = iota
	cmdResume
	cmdStopSeeding
)

type (
	evOpened struct {
		gen     int
		st      storage.Storage
		err     error
		missing bool
		recheck bool
		trusted bool
	}
	evChecked struct {
		gen, index int
		ok         bool
		err        error
	}
	evPeers      struct{ addrs []netip.AddrPort }
	evDialFailed struct{ addr netip.AddrPort }
	evHandshaken struct {
		addr netip.AddrPort
		nc   net.Conn
		hs   peer.Handshake
	}
	evIncoming struct {
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
	evRead struct {
		key  piece.PeerKey
		b    piece.Block
		data []byte
		err  error
	}
	evCommand struct {
		kind  cmdKind
		reply chan error
	}
	evSnapshot struct{ reply chan snapshot }
	evBanned   struct{ ip netip.Addr }
)

// snapshot is what the flusher saves for one torrent.
type snapshot struct {
	rec Record
	st  storage.Storage // non-nil when the fingerprint must be refreshed
}

func newTorrent(s *session, m *metainfo.MetaInfo, dir string, init torrentInit) *torrent {
	ctx, cancel := context.WithCancel(context.Background())
	t := &torrent{
		s:          s,
		m:          m,
		dir:        dir,
		addedAt:    init.addedAt,
		size:       m.Info.VisibleLength(),
		events:     make(chan event, eventQueueSize),
		quit:       make(chan context.Context, 1),
		ctx:        ctx,
		cancel:     cancel,
		done:       make(chan struct{}),
		desired:    init.desired,
		stopReason: init.stopReason,
		restored:   init.restored,
		peers:      map[piece.PeerKey]*peerState{},
		closing:    map[piece.PeerKey]peer.Conn{},
		addrs:      map[netip.AddrPort]bool{},
		writes:     map[int]int{},
		toVerify:   map[int]bool{},
		contrib:    map[int]map[netip.Addr]struct{}{},
		layout: piece.Layout{
			NumPieces:   m.Info.NumPieces(),
			PieceLength: m.Info.PieceLength,
			TotalLength: m.Info.TotalLength,
		},
	}
	t.picker = t.newPicker()
	if r := init.restored; r != nil {
		t.downloaded.Store(r.DownloadedTotal)
		t.uploaded.Store(r.UploadedTotal)
		t.wasted.Store(r.WastedTotal)
		t.completedAt = r.CompletedAt
		t.fprint = r.Fingerprint
		if bf, err := piece.FromBytes(r.Bitfield, t.layout.NumPieces); err == nil {
			for i := 0; i < bf.Len(); i++ {
				if bf.Has(i) {
					t.picker.PieceVerified(i)
					t.completed.Add(wantedBytes(m.Info, i))
				}
			}
		}
		if r.LastError != "" && r.DesiredState == DesiredActive {
			t.errMsg = r.LastError
		}
	}
	switch t.desired {
	case DesiredPaused:
		t.state = StatePaused
	case DesiredStopped:
		t.state = StateStopped
		if t.stopReason == "" {
			t.stopReason = StopReasonUser
		}
	default:
		t.desired = DesiredActive
		t.state = StateChecking
	}
	t.publish()
	return t
}

func (t *torrent) newPicker() piece.Picker {
	var pk piece.Picker
	t.s.withRand(func(r *rand.Rand) { pk = piece.NewPicker(t.layout, rand.New(rand.NewPCG(r.Uint64(), r.Uint64()))) })
	return pk
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

// snapshot asks the actor for a Record (flusher goroutine).
func (t *torrent) snapshot() (snapshot, bool) {
	reply := make(chan snapshot, 1)
	if !t.send(evSnapshot{reply}) {
		return snapshot{}, false
	}
	select {
	case snap := <-reply:
		return snap, true
	case <-t.done:
		return snapshot{}, false
	}
}

// run is the actor goroutine.
func (t *torrent) run() {
	defer close(t.done)
	tk := t.s.clk.NewTicker(tickInterval)
	defer tk.Stop()
	if t.desired == DesiredActive {
		t.activate()
	}
	t.publish()
	for {
		select {
		case ctx := <-t.quit:
			t.shutdown(ctx)
			return
		case now := <-tk.C():
			t.tick(now)
			t.publish()
		case ev := <-t.events:
			t.handle(ev)
			t.publish()
		}
	}
}

// goJob runs f on pool p, tracked by t.wg.
func (t *torrent) goJob(p *pool, f func()) {
	t.wg.Add(1)
	p.submit(func() {
		defer t.wg.Done()
		f()
	})
}

// goRoutine runs f in its own goroutine, tracked by t.wg.
func (t *torrent) goRoutine(f func()) {
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
	case evChecked:
		t.checked(ev)
	case evPeers:
		t.addPeers(ev.addrs)
	case evDialFailed:
		t.halfOpen--
		delete(t.addrs, ev.addr)
		t.dialMore()
	case evHandshaken:
		t.halfOpen--
		t.connected(ev.addr, ev.nc, ev.hs, false)
		t.dialMore()
	case evIncoming:
		t.connected(ev.addr, ev.nc, ev.hs, true)
	case evMessage:
		t.message(ev.key, ev.msg)
	case evPeerClosed:
		t.peerClosed(ev.key, ev.err)
	case evWritten:
		t.written(ev)
	case evVerified:
		t.verified(ev)
	case evRead:
		t.readDone(ev)
	case evCommand:
		ev.reply <- t.command(ev.kind)
	case evSnapshot:
		ev.reply <- t.snapshotNow()
	case evBanned:
		t.applyBan(ev.ip)
	}
}

// active reports whether the torrent is transferring (or looking for peers).
func (t *torrent) active() bool { return t.state == StateDownloading || t.state == StateSeeding }

// start enters downloading or seeding and starts the network side.
func (t *torrent) start() {
	if t.picker.Done() {
		t.state = StateSeeding
		if t.completedAt.IsZero() {
			t.completedAt = t.s.clk.Now()
		}
	} else {
		t.state = StateDownloading
	}
	t.stopReason = ""
	t.startTracker()
	t.accepting.Store(true)
}

func (t *torrent) startTracker() {
	if t.loop != nil {
		return
	}
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
			return tracker.Stats{Uploaded: t.uploaded.Load(), Downloaded: t.downloaded.Load(), Left: t.size - t.completed.Load()}
		},
		OnPeers: func(addrs []netip.AddrPort) {
			t.send(evPeers{append([]netip.AddrPort(nil), addrs...)})
		},
		Clock:  t.s.clk,
		Logger: t.s.log.With("torrent", t.m.HexHash()),
	})
}

// halt stops all network activity: the tracker loop sends stopped (in the
// background, bounded by ShutdownAnnounceTimeout), peers are closed and
// in-flight job results become stale.
func (t *torrent) halt() {
	t.gen++
	t.accepting.Store(false)
	t.checking = 0
	if l := t.loop; l != nil {
		t.loop = nil
		t.goRoutine(func() {
			ctx, cancel := context.WithTimeout(t.ctx, ShutdownAnnounceTimeout)
			defer cancel()
			_ = l.Stop(ctx)
		})
	}
	for k, p := range t.peers {
		t.dropPeer(k, p)
	}
	t.queue = nil
	t.optimistic = 0
}

// dropPeer closes p and forgets it (its Done is awaited at shutdown).
func (t *torrent) dropPeer(k piece.PeerKey, p *peerState) {
	_ = p.conn.Close()
	t.closing[k] = p.conn
	delete(t.peers, k)
	delete(t.addrs, p.addr)
	t.picker.RemovePeer(k)
	if t.optimistic == k {
		t.optimistic = 0
	}
}

// command applies a lifecycle action (G18).
func (t *torrent) command(c cmdKind) error {
	complete := t.picker.Done() && t.ready
	switch c {
	case cmdPause:
		switch {
		case t.state == StatePaused:
			return nil
		case t.state == StateError, t.state == StateStopped, t.picker.Done():
			return ErrInvalidState
		}
		t.halt()
		t.state, t.desired = StatePaused, DesiredPaused
	case cmdStopSeeding:
		switch {
		case t.state == StateStopped:
			return nil
		case t.state == StateChecking, t.state == StateError:
			if !t.picker.Done() {
				return ErrNotComplete
			}
			return ErrInvalidState
		case !complete && t.state != StatePaused, !t.picker.Done():
			return ErrNotComplete
		}
		t.halt()
		t.state, t.desired, t.stopReason = StateStopped, DesiredStopped, StopReasonUser
	case cmdResume:
		switch t.state {
		case StatePaused, StateStopped:
			t.desired = DesiredActive
			t.activate()
		case StateError:
			t.desired = DesiredActive
			t.needCheck = t.store != nil
			t.ready = false
			t.activate()
		}
	}
	return nil
}

// fail moves the torrent to state error and stops all network activity.
func (t *torrent) fail(err error) {
	t.s.log.Error("torrent failed", "torrent", t.m.HexHash(), "err", err)
	t.failMsg(err.Error())
}

func (t *torrent) failMsg(msg string) {
	if t.state == StateError {
		return
	}
	t.halt()
	t.state = StateError
	t.errMsg = msg
}

// finish moves a torrent that completed through downloading to seeding.
func (t *torrent) finish() {
	t.state = StateSeeding
	if t.completedAt.IsZero() {
		t.completedAt = t.s.clk.Now()
	}
	if t.loop != nil {
		t.loop.Completed()
	}
	t.s.log.Info("download complete", "torrent", t.m.HexHash(), "name", t.m.Info.Name)
	for k, p := range t.peers {
		if p.have != nil && p.have.Full() {
			t.dropPeer(k, p) // two seeds have nothing to exchange
			continue
		}
		if p.interested {
			p.interested = false
			_ = p.conn.Send(peer.Message{ID: peer.MsgNotInterested})
		}
	}
	t.s.requestFlush()
}

// written handles a finished block write.
func (t *torrent) written(ev evWritten) {
	t.pending--
	t.writes[ev.index]--
	if t.writes[ev.index] <= 0 {
		delete(t.writes, ev.index)
	}
	if ev.err != nil {
		if !errors.Is(ev.err, storage.ErrClosed) {
			t.fail(fmt.Errorf("write piece %d: %w", ev.index, ev.err))
		}
		return
	}
	t.maybeVerify(ev.index)
	if t.throttled && t.pending < maxPendingWrites/2 {
		t.throttled = false
		t.fillAll()
	}
}

// maybeVerify hashes a complete piece once all of its writes are done.
func (t *torrent) maybeVerify(index int) {
	if !t.toVerify[index] || t.writes[index] > 0 || t.state == StateError {
		return
	}
	delete(t.toVerify, index)
	st := t.store
	t.goJob(t.s.hash, func() {
		if t.ctx.Err() != nil {
			return
		}
		ok, err := st.VerifyPiece(index)
		if err == nil && ok {
			err = st.MarkVerified(index)
		}
		t.send(evVerified{index, ok, err})
	})
}

func (t *torrent) verified(ev evVerified) {
	contributors := t.contrib[ev.index]
	delete(t.contrib, ev.index)
	if t.state == StateError {
		return
	}
	if ev.err != nil {
		t.fail(fmt.Errorf("verify piece %d: %w", ev.index, ev.err))
		return
	}
	if !ev.ok {
		t.s.log.Warn("piece failed the hash check", "torrent", t.m.HexHash(), "piece", ev.index, "contributors", len(contributors))
		t.picker.PieceFailed(ev.index)
		t.wasted.Add(t.store.PieceSize(ev.index))
		for ip := range contributors {
			if t.s.strike(ip, t) {
				t.applyBan(ip)
			}
		}
		t.fillAll()
		return
	}
	t.picker.PieceVerified(ev.index)
	t.completed.Add(t.store.WantedBytes(ev.index))
	for _, p := range t.peers {
		if p.have == nil || !p.have.Has(ev.index) {
			_ = p.conn.Send(peer.Message{ID: peer.MsgHave, Index: uint32(ev.index)})
		}
	}
	if t.picker.Done() && t.state == StateDownloading {
		t.finish()
	}
}

// applyBan disconnects every peer at ip and discards the in-progress
// pieces it contributed to.
func (t *torrent) applyBan(ip netip.Addr) {
	for k, p := range t.peers {
		if p.addr.Addr() == ip {
			t.dropPeer(k, p)
		}
	}
	q := t.queue[:0]
	for _, a := range t.queue {
		if a.Addr() != ip {
			q = append(q, a)
		}
	}
	t.queue = q
	for idx, ips := range t.contrib {
		if _, ok := ips[ip]; ok && !t.toVerify[idx] && t.writes[idx] == 0 {
			t.picker.Discard(idx)
			delete(t.contrib, idx)
		}
	}
	t.fillAll()
}

// snapshotNow builds the Record to save.
func (t *torrent) snapshotNow() snapshot {
	rec := t.record()
	var st storage.Storage
	if t.store != nil && t.ready {
		st = t.store
	}
	return snapshot{rec: rec, st: st}
}

// record returns the persisted form of the torrent.
func (t *torrent) record() Record {
	r := Record{
		InfoHash:        t.m.InfoHash,
		Name:            t.m.Info.Name,
		RawInfo:         t.m.RawInfo,
		Trackers:        t.m.Trackers,
		Private:         t.m.Info.Private,
		SizeBytes:       t.size,
		PieceLength:     t.m.Info.PieceLength,
		DownloadDir:     t.dir,
		DesiredState:    t.desired,
		Bitfield:        t.picker.Have().Bytes(),
		Fingerprint:     t.fprint,
		UploadedTotal:   t.uploaded.Load(),
		DownloadedTotal: t.downloaded.Load(),
		WastedTotal:     t.wasted.Load(),
		QueuePosition:   t.position,
		AddedAt:         t.addedAt,
		CompletedAt:     t.completedAt,
	}
	if t.desired == DesiredStopped {
		r.StopReason = t.stopReason
	}
	if t.state == StateError {
		r.LastError = t.errMsg
	}
	return r
}

// shutdown stops the tracker loop (stopped announce, bounded by ctx and
// ShutdownAnnounceTimeout) in parallel with closing peers and storage,
// then records the final state for the Store.
func (t *torrent) shutdown(ctx context.Context) {
	t.accepting.Store(false)
	stopped := make(chan error, 1)
	if t.loop != nil {
		l := t.loop
		t.loop = nil
		go func() {
			actx, cancel := context.WithTimeout(ctx, ShutdownAnnounceTimeout)
			defer cancel()
			stopped <- l.Stop(actx)
		}()
	} else {
		stopped <- nil
	}
	t.cancel()
	for k, p := range t.peers {
		_ = p.conn.Close()
		t.closing[k] = p.conn
	}
	for _, c := range t.closing {
		<-c.Done()
	}
	t.wg.Wait()
	rec := t.record()
	if t.store != nil {
		if err := t.store.Sync(); err != nil {
			t.s.log.Error("sync torrent data", "torrent", t.m.HexHash(), "err", err)
		}
		_ = t.store.Close()
		if t.ready {
			rec.Fingerprint = fingerprint(t.m.Info, t.dir)
		}
	}
	t.final = &rec
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
		UploadedTotal:   t.uploaded.Load(),
		WastedTotal:     t.wasted.Load(),
		DownloadDir:     t.dir,
		AddedAt:         t.addedAt,
		CompletedAt:     t.completedAt,
	}
	switch t.state {
	case StateError:
		st.ErrorMessage = t.errMsg
	case StateStopped:
		st.StopReason = t.stopReason
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
		st.DownloadRate += p.rateDown
		st.UploadRate += p.rateUp
	}
	t.status.Store(st)
}
