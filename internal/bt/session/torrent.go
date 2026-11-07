package session

import (
	"context"
	"log/slog"
	"math"
	"math/rand/v2"
	"net/netip"
	"sync"
	"time"

	"torrentd/internal/bt/metainfo"
	"torrentd/internal/bt/peer"
	"torrentd/internal/bt/piece"
	"torrentd/internal/bt/storage"
	"torrentd/internal/bt/tracker"
)

// State is a torrent state (§5.1, §16.4).
type State string

// Torrent states.
const (
	StateChecking    State = "checking"
	StateDownloading State = "downloading"
	StateSeeding     State = "seeding"
	StatePaused      State = "paused"
	StateError       State = "error"
)

// Stats is a snapshot of one torrent (§5.4).
type Stats struct {
	ID              string
	Name            string
	State           State
	Private         bool
	SizeBytes       int64
	CompletedBytes  int64
	Progress        float64
	DownloadRate    int64
	UploadRate      int64
	UploadedTotal   int64
	DownloadedTotal int64
	WastedTotal     int64
	PeersConnected  int
	SeedsConnected  int
	// SwarmSeeders and SwarmLeechers are -1 until a tracker reports them.
	SwarmSeeders  int
	SwarmLeechers int
	DownloadDir   string
	AddedAt       time.Time
	// CompletedAt is zero until the torrent first reaches 100%.
	CompletedAt    time.Time
	SeedingSeconds int64
	ErrorMessage   string
	// Left is the number of bytes still missing (tracker "left").
	Left int64
}

// emaAlpha smooths rates over about 5 s with 1 s samples (§5.4).
var emaAlpha = 1 - math.Exp(-1.0/5)

// event types for the actor.
type (
	evPeers      struct{ addrs []netip.AddrPort }
	evConnected  struct{ c *candidate; conn *peer.Conn }
	evDialFailed struct{ c *candidate }
	evMsg        struct {
		p *peerState
		m *peer.Message
	}
	evClosed struct {
		p   *peerState
		err error
	}
	evHashed struct {
		piece int
		ok    bool
		data  []byte
	}
	evWritten struct {
		piece   int
		renamed []string
		err     error
	}
	evSwarm struct{ seeders, leechers int }
)

// Torrent is one torrent's actor: a goroutine that owns all of the
// torrent's state and processes events from peers, workers and the
// announcer.
type Torrent struct {
	s      *Session
	mi     *metainfo.MetaInfo
	dir    string
	paused bool
	log    *slog.Logger
	rng    *rand.Rand
	tiers  *tracker.Tiers

	ctx      context.Context
	cancel   context.CancelFunc
	events   chan any
	finished chan struct{} // signals the announcer that the download completed
	actorWG  sync.WaitGroup
	annWG    sync.WaitGroup

	mu    sync.Mutex
	stats Stats

	// Actor-owned state.
	st            *storage.Storage
	picker        *piece.Picker
	peers         map[*peerState]bool
	candidates    map[netip.AddrPort]*candidate
	dialing       int
	bufs          map[int][]byte
	verifiedBytes int64
	completed     int64
	downloaded    int64
	wasted        int64
	rate          float64
	tickBytes     int64
	seeders       int
	leechers      int
	state         State
	errMsg        string
	completedAt   time.Time
	seedingSecs   int64
}

func newTorrent(s *Session, m *metainfo.MetaInfo, dir string, paused bool, rng *rand.Rand) *Torrent {
	t := &Torrent{
		s:          s,
		mi:         m,
		dir:        dir,
		paused:     paused,
		log:        s.log.With("torrent", m.HexHash()),
		rng:        rng,
		events:     make(chan any, 1024),
		finished:   make(chan struct{}),
		peers:      map[*peerState]bool{},
		candidates: map[netip.AddrPort]*candidate{},
		bufs:       map[int][]byte{},
		picker:     piece.NewPicker(m.Info.TotalLength, m.Info.PieceLength),
		seeders:    -1,
		leechers:   -1,
		state:      StateChecking,
	}
	var tiers [][]string
	for _, tr := range m.Trackers {
		for len(tiers) <= tr.Tier {
			tiers = append(tiers, nil)
		}
		tiers[tr.Tier] = append(tiers[tr.Tier], tr.URL)
	}
	t.tiers = tracker.NewTiers(tiers, t.rng, s.cfg.HTTPClient)
	t.ctx, t.cancel = context.WithCancel(s.ctx)
	t.stats = Stats{
		ID:          m.HexHash(),
		Name:        m.Info.Name,
		Private:     m.Info.Private,
		SizeBytes:   m.Info.VisibleLength(),
		DownloadDir: dir,
		AddedAt:     s.clk.Now().UTC(),
	}
	t.publish()
	return t
}

func (t *Torrent) start() {
	t.actorWG.Add(1)
	go t.run()
}

// stop cancels the torrent; the announcer sends its stopped announce.
func (t *Torrent) stop() { t.cancel() }

// wait blocks until the actor and the announcer have exited.
func (t *Torrent) wait() {
	t.actorWG.Wait()
	t.annWG.Wait()
}

func (t *Torrent) closeStorage() {
	if t.st != nil {
		if err := t.st.Close(); err != nil {
			t.log.Warn("close files", "err", err)
		}
	}
}

// Stats returns the latest published snapshot.
func (t *Torrent) Stats() Stats {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.stats
}

// publish copies the actor's state into the snapshot read by Stats.
func (t *Torrent) publish() {
	size := t.mi.Info.VisibleLength()
	seeds := 0
	for p := range t.peers {
		if p.has.Full() {
			seeds++
		}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	s := &t.stats
	s.State = t.state
	s.CompletedBytes = t.completed
	s.Progress = 1
	if size > 0 {
		s.Progress = float64(t.completed) / float64(size)
	}
	if t.picker.Complete() {
		s.Progress = 1
	}
	s.DownloadRate = int64(t.rate)
	s.DownloadedTotal = t.downloaded
	s.WastedTotal = t.wasted
	s.PeersConnected = len(t.peers)
	s.SeedsConnected = seeds
	s.SwarmSeeders, s.SwarmLeechers = t.seeders, t.leechers
	s.CompletedAt = t.completedAt
	s.SeedingSeconds = t.seedingSecs
	s.ErrorMessage = t.errMsg
	s.Left = t.mi.Info.TotalLength - t.verifiedBytes
}

// send delivers an event to the actor unless the torrent is stopping.
func (t *Torrent) send(ev any) {
	select {
	case t.events <- ev:
	case <-t.ctx.Done():
	}
}

// run is the actor goroutine.
func (t *Torrent) run() {
	defer t.actorWG.Done()
	defer t.shutdownPeers()

	if t.paused {
		t.state = StatePaused
		t.publish()
		<-t.ctx.Done()
		return
	}
	// checking: nothing is on disk yet at milestone 3, so this only
	// prepares the files (sparse .part preallocation).
	st, err := storage.Open(t.dir, t.mi.Info, t.s.cfg.MaxHandles)
	if err != nil {
		t.fail("prepare files: " + err.Error())
		<-t.ctx.Done()
		return
	}
	t.st = st
	t.state = StateDownloading
	t.publish()
	t.log.Info("torrent started", "name", t.mi.Info.Name, "pieces", t.picker.NumPieces())

	t.annWG.Add(1)
	go t.announceLoop()

	tick := t.s.clk.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-t.ctx.Done():
			return
		case <-tick.C():
			t.onTick()
		case ev := <-t.events:
			t.handle(ev)
		}
		t.publish()
	}
}

func (t *Torrent) handle(ev any) {
	switch ev := ev.(type) {
	case evPeers:
		t.addCandidates(ev.addrs)
	case evConnected:
		t.onConnected(ev.c, ev.conn)
	case evDialFailed:
		t.dialing--
		t.retryLater(ev.c)
		t.connectMore()
	case evMsg:
		if t.peers[ev.p] {
			t.onMessage(ev.p, ev.m)
		}
	case evClosed:
		t.onClosed(ev.p, ev.err)
	case evHashed:
		t.onHashed(ev)
	case evWritten:
		t.onWritten(ev)
	case evSwarm:
		t.seeders, t.leechers = ev.seeders, ev.leechers
	}
}

func (t *Torrent) fail(msg string) {
	t.log.Error("torrent error", "err", msg)
	t.state = StateError
	t.errMsg = msg
	for p := range t.peers {
		p.conn.Close()
	}
	t.publish()
}

func (t *Torrent) onTick() {
	t.rate += (float64(t.tickBytes) - t.rate) * emaAlpha
	if t.rate < 1 {
		t.rate = 0
	}
	t.tickBytes = 0
	if t.state == StateSeeding {
		t.seedingSecs++
	}
	for p := range t.peers {
		p.tick()
		t.fillRequests(p)
	}
	t.connectMore()
}

// onHashed handles a finished hash check.
func (t *Torrent) onHashed(ev evHashed) {
	if t.state == StateError {
		return
	}
	if !ev.ok {
		size := t.picker.PieceSize(ev.piece)
		t.wasted += size
		t.log.Warn("piece failed its hash check", "piece", ev.piece, "bytes", size)
		t.picker.Failed(ev.piece)
		for p := range t.peers {
			t.fillRequests(p)
		}
		return
	}
	i, data, st := ev.piece, ev.data, t.st
	t.s.disk.Submit(func() {
		err := st.WritePiece(i, data)
		var renamed []string
		if err == nil {
			renamed, err = st.MarkVerified(i)
		}
		t.send(evWritten{piece: i, renamed: renamed, err: err})
	})
}

// onWritten handles a verified piece that is now on disk.
func (t *Torrent) onWritten(ev evWritten) {
	if t.state == StateError {
		return
	}
	if ev.err != nil {
		t.fail("write piece: " + ev.err.Error())
		return
	}
	for _, f := range ev.renamed {
		t.log.Debug("file complete", "path", f)
	}
	t.picker.Verified(ev.piece)
	t.verifiedBytes += t.picker.PieceSize(ev.piece)
	t.completed += t.st.VisibleBytes(ev.piece)
	for p := range t.peers {
		p.conn.Send(&peer.Message{ID: peer.MsgHave, Index: uint32(ev.piece)})
	}
	if t.picker.Complete() && t.state == StateDownloading {
		t.state = StateSeeding
		t.completedAt = t.s.clk.Now().UTC()
		t.log.Info("download complete", "downloaded", t.downloaded, "wasted", t.wasted)
		for p := range t.peers {
			t.updateInterest(p)
		}
		t.publish()
		close(t.finished)
	}
}

// shutdownPeers closes every peer connection when the actor exits.
func (t *Torrent) shutdownPeers() {
	for p := range t.peers {
		p.conn.Close()
	}
}
