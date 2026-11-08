// Package session orchestrates the engine: one Session per process and one
// torrent actor per torrent (§4.4, §4.5, §5, §6.1, §6.2, ADR-0011).
//
// FROZEN CONTRACT (ADR-0011, ADR-0015, ADR-0016, ADR-0017, ADR-0018,
// ADR-0019). This file is owned by the architect. The integrator provides,
// in their own files:
//
//	func newSession(cfg Config) (Session, error)
//
// # Torrent lifecycle (M4)
//
//   - Add validates and registers a torrent, persists it (desired_state
//     active, or paused with StartPaused) and starts its actor in checking.
//     Checking runs storage.Stat and storage.PlanResume (Saved nil), Opens
//     storage, hashes the Recheck pieces on the hash pool (CheckingProgress
//     grows from 0 to 1) and marks the verified ones. Then: complete ->
//     seeding (completedAt set if zero, G22: no completed announce);
//     otherwise downloading (or paused with StartPaused / desired paused).
//   - downloading: tracker.Loop started (event started), peers dialled and
//     accepted, blocks requested per the picker. On completion it calls
//     Loop.Completed, sets completedAt and moves to seeding.
//   - seeding: the same tracker loop and peer connections keep running;
//     interested peers are served under the choker.
//   - Pause (checking, queued, downloading; no-op in paused; G18): stop the
//     tracker loop (stopped announce, bounded by ShutdownAnnounceTimeout,
//     not awaited by Pause), close every peer, cancel in-flight checking,
//     state paused, desired_state paused. Pause on a complete torrent
//     (seeding, stopped) or in error/moving -> ErrInvalidState.
//   - StopSeeding (seeding, queued-for-seed; no-op in stopped): the same
//     teardown, state stopped, StopReason user, desired_state stopped. On a
//     torrent that is not complete (any other state, including checking) ->
//     ErrNotComplete; in error/moving -> ErrInvalidState.
//   - Resume: paused -> downloading (or seeding if complete); stopped ->
//     seeding (StopReason cleared; limit_override set if the reason was a
//     limit, §5.2); error -> checking with a full recheck (G1 retry); any
//     active state -> nil, no-op. desired_state becomes active.
//   - Every action returns once the new state is published: a Get right
//     after a successful Pause/StopSeeding/Resume sees the new state, and no
//     request is sent and no block uploaded by the torrent after Pause or
//     StopSeeding returned. Unknown id -> ErrNotFound.
//   - Torrents never leave the Session at M4 (removal is M6).
//
// # Startup (§6.1 step 4, C3, G8, ADR-0019)
//
// New loads every Store record in order and starts each actor: the
// MetaInfo is rebuilt from RawInfo (metainfo.ParseInfo) and Trackers;
// counters, addedAt and completedAt are restored. Checking runs
// storage.PlanResume with the saved bitfield and fingerprint and
// Config.PreviousClean: FilesMissing -> state error, ErrorMessage
// "Files missing" (no files are created); otherwise Trusted pieces are
// marked verified, Recheck pieces hashed, and the torrent moves to its
// desired state: active -> downloading/seeding, paused -> paused, stopped ->
// stopped with its StopReason (or paused if a recheck found it incomplete,
// C6). New does not wait for checking to finish.
//
// # Persistence (§7, C3, ADR-0019)
//
// Every FlushInterval, and in Close after all peers and disk writes have
// stopped, the Session flushes every torrent whose record changed: for
// each torrent it (1) captures the record (bitfield, counters, state), (2)
// calls storage.Sync, (3) takes the fingerprint with storage.Stat, then
// writes all records with one Store.SaveTorrents call. A bitfield is never
// persisted before the data of its pieces was synced. State changes
// (add, pause, resume, stop seeding, error) are saved immediately, in the
// same way. A Store error is logged and retried at the next flush; it
// never stops the engine.
//
// # Peers (§4.1, G10, ADR-0017)
//
//   - At most MaxPeersPerTorrent connections per torrent (incoming plus
//     outgoing), at most MaxHalfOpen dials in flight per torrent,
//     DialTimeout for dial plus handshake; the same for an incoming
//     handshake (peer.AcceptHandshake). Incoming connections for an
//     unknown, paused, stopped, checking or error torrent are refused.
//   - Never more than one connection per remote address (ip:port) per
//     torrent, and never a connection to ourselves (our peer ID). After a
//     connection to an address ends, it is not redialled for RedialBackoff
//     (doubling per consecutive failure, capped at 16x).
//   - Right after the handshake we send our bitfield (if we have a piece),
//     then interested/not-interested per Picker.Interesting, kept up to
//     date. Every verified piece is announced with have to every peer that
//     lacks it. In seeding, a connection to a peer that is a seed is
//     closed.
//   - Downloading: each unchoked peer gets piece.PipelineDepth(rate) blocks
//     outstanding (rate is its choke.Meter download rate). Choke from the
//     peer -> Picker.Release. Picker.Expire runs at least every second;
//     expired blocks get a cancel. Endgame duplicates get a cancel when the
//     block arrives from someone else (Receipt.Cancel).
//   - Uploading: requests are honoured only from peers we unchoked, for
//     pieces we have, with Length <= piece.BlockSize and within the piece;
//     a request with Length > BlockSize closes the connection (WIRE-03);
//     other invalid requests are ignored. At most MaxUploadQueue requests
//     are queued per peer (more are dropped) and at most UploadReadAhead
//     blocks per peer are read and queued in the peer.Conn at once (freed
//     by peer.Config.OnPieceSent). Cancel removes a queued request. Choking
//     a peer drops its queue. UploadedTotal counts OnPieceSent bytes.
//   - Choking: choke.Choker per torrent, Rechoke every
//     choke.RechokeInterval, Fill on connect/disconnect/interest changes.
//   - Bans (G10, ADR-0006): every origin returned by Picker.PieceFailed
//     gets a strike; at BanStrikes strikes the IP is banned for
//     BanDuration (Store.SaveBan), every connection from it (all torrents)
//     is closed, it is never dialled and its incoming connections are
//     refused before the handshake. Strikes are process-wide, in memory.
//     Bans are loaded from the Store at startup. IPv4-mapped IPv6
//     addresses are unmapped before comparison; zones are ignored.
//   - Counters: DownloadedTotal counts every payload byte received in piece
//     messages; WastedTotal counts payload bytes discarded (rejected by the
//     picker, or a whole piece whose hash failed); CompletedBytes is the sum
//     of storage.WantedBytes over verified pieces.
//
// # Concurrency and data path (ADR-0015, ADR-0017)
//
//   - Each torrent actor is one goroutine owning all torrent state (picker,
//     choker, peers, counters). Peer read goroutines, storage callbacks,
//     timers and the tracker loop talk to it only through its event
//     channel. The actor never blocks on I/O or on another goroutine.
//   - A received block travels: peer read goroutine -> actor (Picker
//     Received decides) -> back to that peer's read goroutine, which writes
//     it with storage.WriteBlock (blocking itself on the shared disk pool:
//     back-pressure on that one TCP connection) and reports the write to
//     the actor. A piece is hashed (VerifyPieceAsync) only once it is
//     pending and all of its accepted block writes have completed. So the
//     process holds at most about one block per connection in flight on
//     the receive path, plus one hash buffer per hash worker.
//   - One process-wide storage.Pools (Config.DiskWorkers, HashWorkers),
//     shared by every torrent's Storage. No goroutine per block or per
//     piece anywhere; goroutines per connection are bounded (reader,
//     writer, and at most one helper).
//   - Get and List never wait for an actor: they read the last Status the
//     actor published (an atomic snapshot). The actor publishes after
//     every state change, every verified piece and at least every second
//     while transferring; a published Status is internally consistent
//     (seeding or stopped implies CompletedBytes == SizeBytes).
//   - All Session methods are safe for concurrent use. Close leaves no
//     goroutine, connection or file handle behind (btest.CheckGoroutines).
//
// # Shutdown (§6.2, §16.11, ADR-0015)
//
// Close: stop accepting, then in parallel (a) every active torrent's
// stopped announce, bounded by ShutdownAnnounceTimeout and ctx, and (b)
// close every peer and cancel checking; wait for disk writes in flight;
// final flush (as above, with the fingerprint taken after the last write);
// close storages, then the pools. cmd/torrentd starts Close at the same
// moment as the HTTP drain, then writes the clean-shutdown marker after
// Close returned, then closes the DB.
package session

import (
	"context"
	"errors"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"time"

	"torrentd/internal/bt/metainfo"
	"torrentd/internal/clock"
	"torrentd/internal/store"
)

// State is a torrent state (§5.1, §16.4). The values match the TorrentState
// schema in spec/openapi.yaml. M3 produces checking, downloading, seeding,
// paused and error.
type State string

// Torrent states.
const (
	StateMetadata    State = "metadata"
	StateChecking    State = "checking"
	StateQueued      State = "queued"
	StateDownloading State = "downloading"
	StateSeeding     State = "seeding"
	StateStopped     State = "stopped"
	StatePaused      State = "paused"
	StateError       State = "error"
	StateMoving      State = "moving"
)

// ShutdownAnnounceTimeout bounds the stopped announces on Close (§6.2).
const ShutdownAnnounceTimeout = 3 * time.Second

// Engine limits and timings (§4.1, §7, G2, G10, D-table).
const (
	// FlushInterval is the persistence flush period (§7, RES-05).
	FlushInterval = 10 * time.Second
	// MaxPeersPerTorrent bounds connections per torrent.
	MaxPeersPerTorrent = 50
	// MaxHalfOpen bounds outgoing dials in flight per torrent (G2).
	MaxHalfOpen = 20
	// DialTimeout bounds dial plus handshake, both directions.
	DialTimeout = 10 * time.Second
	// RedialBackoff is the first wait before redialling an address.
	RedialBackoff = 30 * time.Second
	// MaxUploadQueue bounds the queued incoming requests per peer.
	MaxUploadQueue = 256
	// UploadReadAhead bounds the blocks read and queued per peer.
	UploadReadAhead = 4
	// BanStrikes is the number of hash-failure strikes that ban an IP.
	BanStrikes = 3
	// BanDuration is how long a ban lasts (ADR-0006).
	BanDuration = 24 * time.Hour
)

// StopReason is why a torrent is stopped (StopReason schema).
type StopReason string

// Stop reasons; the empty value means none.
const (
	StopReasonNone  StopReason = ""
	StopReasonUser  StopReason = "user"
	StopReasonRatio StopReason = "ratio_limit"
	StopReasonTime  StopReason = "time_limit"
)

// ErrorFilesMissing is the ErrorMessage of the G8 state (RES-07).
const ErrorFilesMissing = "Files missing"

// Store is the persistence the Session needs (§7). *store.Store
// implements it; btest.MemStore is the in-memory fake.
type Store interface {
	// LoadTorrents returns every persisted torrent in queue order.
	LoadTorrents(ctx context.Context) ([]store.TorrentRecord, error)
	// SaveTorrents upserts records in one transaction.
	SaveTorrents(ctx context.Context, recs []store.TorrentRecord) error
	// LoadBans returns the unexpired bans.
	LoadBans(ctx context.Context, now time.Time) ([]store.Ban, error)
	// SaveBan upserts a ban.
	SaveBan(ctx context.Context, b store.Ban) error
}

var _ Store = (*store.Store)(nil)

// Errors returned by Session methods (use errors.Is).
var (
	// ErrAlreadyExists: a torrent with the same info-hash is registered.
	ErrAlreadyExists = errors.New("session: torrent already exists")
	// ErrInvalidDownloadDir: the download dir is not absolute or cannot be
	// created.
	ErrInvalidDownloadDir = errors.New("session: invalid download dir")
	// ErrClosed: the Session has been closed.
	ErrClosed = errors.New("session: closed")
	// ErrNotFound: no torrent with that id (HTTP 404 not_found).
	ErrNotFound = errors.New("session: torrent not found")
	// ErrInvalidState: the action is not valid in the current state
	// (HTTP 409 invalid_state, G18).
	ErrInvalidState = errors.New("session: invalid state for this action")
	// ErrNotComplete: stop-seeding on an incomplete torrent (HTTP 409
	// not_complete, G18).
	ErrNotComplete = errors.New("session: torrent not complete")
)

// Config configures a Session. Zero values select defaults.
type Config struct {
	// PeerPort is the TCP port to listen on, on all interfaces; 0 picks a
	// free port.
	PeerPort int
	// DefaultDownloadDir is used when AddOptions.DownloadDir is empty.
	DefaultDownloadDir string
	// Clock is the engine's time source; nil means clock.Real().
	Clock clock.Clock
	// Rand is the engine's randomness (peer ID, tracker key, shuffles);
	// nil means a source seeded from crypto/rand.
	Rand *rand.Rand
	// HTTPClient is used for HTTP(S) tracker announces; nil means a default
	// client with a 30 s timeout.
	HTTPClient *http.Client
	// Logger is the engine logger; nil means slog.Default().
	Logger *slog.Logger
	// Store persists torrents and bans; nil means no persistence (tests).
	Store Store
	// PreviousClean reports whether the previous run shut down cleanly
	// (store.BeginSession); it selects the fast-resume rules (C3).
	PreviousClean bool
	// DiskWorkers and HashWorkers size the process-wide pools; 0 means
	// storage.DefaultDiskWorkers and storage.DefaultHashWorkers().
	DiskWorkers, HashWorkers int
}

// AddOptions are the per-torrent parameters of Add.
//
// Add returns the initial Status (state checking) and persists the torrent
// before returning (when a Store is configured).
type AddOptions struct {
	// DownloadDir is the absolute download dir; it is created with its
	// parents if missing. Empty means Config.DefaultDownloadDir.
	DownloadDir string
	// StartPaused adds the torrent with desired_state paused: it is still
	// checked (local only), then goes to paused, or to stopped with
	// StopReason user if the check found it complete. No network activity.
	StartPaused bool
}

// Status is a snapshot of one torrent, the engine-side view of the Torrent
// schema. It shares no memory with the engine.
type Status struct {
	// ID is the v1 info-hash.
	ID [20]byte
	// Name is the torrent name from the info dict.
	Name string
	// State is the current state.
	State State
	// StopReason is set in state stopped.
	StopReason StopReason
	// CheckingProgress is the fraction of the current check done (0..1),
	// meaningful in state checking only.
	CheckingProgress float64
	// Private is the BEP 27 flag.
	Private bool
	// SizeBytes is the total size of wanted (non-pad) files.
	SizeBytes int64
	// CompletedBytes is the verified bytes of wanted files.
	CompletedBytes int64
	// Progress is CompletedBytes/SizeBytes in [0, 1] (1 if SizeBytes is 0).
	Progress float64
	// DownloadedTotal and UploadedTotal are payload byte counters.
	DownloadedTotal, UploadedTotal int64
	// WastedTotal is discarded payload bytes.
	WastedTotal int64
	// DownloadRate and UploadRate are payload bytes/s over the last
	// choke.MeterWindow (M6's stats sampler may smooth them further).
	DownloadRate, UploadRate int64
	// PeersConnected is the number of handshaken peer connections.
	PeersConnected int
	// SeedsConnected is the number of connected peers that have every piece.
	SeedsConnected int
	// DownloadDir is the absolute download dir.
	DownloadDir string
	// AddedAt is when Add was called (Clock time).
	AddedAt time.Time
	// CompletedAt is when the torrent first reached 100%; zero if never.
	CompletedAt time.Time
	// ErrorMessage is set in state error.
	ErrorMessage string
}

// Session is the process-wide engine.
type Session interface {
	// Add registers m and starts its actor. Errors: ErrAlreadyExists (a
	// duplicate is detected before the download dir is validated),
	// ErrInvalidDownloadDir, ErrClosed. On success it returns the initial
	// Status (state checking) after persisting the torrent.
	Add(ctx context.Context, m *metainfo.MetaInfo, opts AddOptions) (Status, error)
	// List returns every torrent's Status in the order they were added.
	List() []Status
	// Get returns one torrent's Status.
	Get(id [20]byte) (Status, bool)
	// PeerPort returns the bound peer port (the port announced to
	// trackers), or 0 if binding failed.
	PeerPort() int
	// ListenErr returns the error from binding the peer port, if any. The
	// Session still works for outgoing connections (§16.7 G16).
	ListenErr() error
	// PeerID returns the process's peer ID (-TD0100- + 12 random bytes).
	PeerID() [20]byte
	// Pause pauses a torrent (see Torrent lifecycle). Errors: ErrNotFound,
	// ErrInvalidState, ErrClosed.
	Pause(ctx context.Context, id [20]byte) error
	// Resume resumes, resumes seeding or retries a torrent (see Torrent
	// lifecycle). Errors: ErrNotFound, ErrInvalidState, ErrClosed.
	Resume(ctx context.Context, id [20]byte) error
	// StopSeeding stops seeding a complete torrent (§5.2). Errors:
	// ErrNotFound, ErrNotComplete, ErrInvalidState, ErrClosed.
	StopSeeding(ctx context.Context, id [20]byte) error
	// Close stops every torrent: it sends the stopped announces in
	// parallel, bounded by ShutdownAnnounceTimeout and ctx, closes the
	// listener, peers and files, and waits for all engine goroutines. It is
	// idempotent. After Close, Add returns ErrClosed; List and Get still
	// return the last Status.
	Close(ctx context.Context) error
}

// New starts a Session: it binds the peer port, loads the bans and the
// persisted torrents from cfg.Store and starts their actors (see Startup),
// and returns without waiting for checking. A bind failure is not an error
// (see ListenErr); New fails on invalid Config or if the Store cannot be
// read.
func New(cfg Config) (Session, error) { return newSession(cfg) }
