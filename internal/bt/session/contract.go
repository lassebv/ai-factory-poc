// Package session orchestrates the engine: one Session per process and one
// torrent actor per torrent (§4.4, §4.5, §5, §6.1, §6.2, ADR-0011).
//
// FROZEN CONTRACT (ADR-0011, ADR-0015, ADR-0017). This file is owned by the
// architect. The integrator provides, in their own files:
//
//	func newSession(cfg Config) (Session, error)
//
// # Milestone 4 behavior (ADR-0016, ADR-0017)
//
//   - Add validates and registers a torrent and starts its actor in state
//     checking (or paused with StartPaused). If any of its files already
//     exists in the download dir, every piece is verified (checking);
//     otherwise checking only opens storage. The torrent then goes to
//     seeding if every wanted piece is verified (completedAt set, no
//     completed announce, §16.2 G22), else downloading.
//   - downloading and seeding torrents run a tracker.Loop (started; on
//     completion through downloading, completed), dial tracker peers (max
//     50 peers, 20 half-open, 10 s dial+handshake timeout) and accept
//     incoming connections on the peer port. Requests are served from
//     storage; have is sent for every newly verified piece.
//   - Choking (§4.1): 4 regular unchoke slots by rate (download rate from
//     the peer while downloading, upload rate to it while seeding),
//     re-evaluated every 10 s, plus one optimistic unchoke rotated every
//     30 s. Free slots are filled as soon as a peer becomes interested. A
//     peer that sends no data for 60 s while we wait for it is snubbed: it
//     only gets the optimistic slot.
//   - Requests (D1): piece.PipelineDepth(rate) outstanding per peer, rate
//     being the peer's smoothed download rate. A peer that leaves requests
//     unanswered for 30 s has them released (and cancelled) and gets a
//     depth of 1 until it delivers. Endgame duplicates are cancelled when
//     the block arrives elsewhere. A request for more than 16 KiB closes
//     the connection (no fast extension yet, §16.2 T6).
//   - Hash failures (G10, ADR-0006): every IP that contributed a block to
//     a failed piece gets a strike; at 3 strikes the IP is banned for 24 h
//     (persisted through Store), its connections are closed, in-progress
//     pieces it contributed to are discarded, and it is never dialled or
//     accepted while banned.
//   - Lifecycle (ADR-0016, §16.4 G18): Pause, Resume and StopSeeding. A
//     paused or stopped torrent has no network activity: its tracker loop
//     sends stopped, its peers are closed. Status.StopReason is set in
//     stopped.
//   - Persistence (§7, §16.3 C3, G8, ADR-0002, ADR-0004): with a Store,
//     torrents are saved on Add, after lifecycle actions, every
//     FlushInterval and on Close; data is fsynced before a bitfield that
//     contains it is saved. Load restores them: desired state, stop reason,
//     lifetime totals and the bitfield. Before an active torrent's storage
//     is opened, its files are compared with the saved fingerprint: after
//     a clean shutdown size and mtime must match, after an unclean one only
//     the size; otherwise every piece is rechecked. If the bitfield is not
//     empty and every file is missing, the torrent goes to error with
//     "Files missing".
//   - Counters: DownloadedTotal counts every payload byte received in piece
//     messages; UploadedTotal every payload byte sent; WastedTotal payload
//     bytes discarded (unrequested or duplicate blocks, or a whole piece
//     whose hash failed); CompletedBytes is the sum of storage.WantedBytes
//     over verified pieces. All three totals are lifetime values.
//
// # Concurrency (ADR-0015, §4.5)
//
//   - Each torrent actor is one goroutine owning all torrent state (picker,
//     peers, counters). Peer read goroutines, storage jobs and the tracker
//     loop talk to it only through its event channel.
//   - Disk I/O (block reads and writes, opening storage) runs on one
//     process-wide pool of Config.DiskWorkers goroutines, hashing on one
//     pool of Config.HashWorkers goroutines. Received data waiting to be
//     written is bounded per torrent; the request pipeline pauses when the
//     bound is reached.
//   - Get and List never wait for an actor: they read the last Status the
//     actor published (an atomic snapshot). A published Status is
//     internally consistent (e.g. state seeding implies CompletedBytes ==
//     SizeBytes).
//   - All Session methods are safe for concurrent use.
package session

import (
	"context"
	"errors"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/netip"
	"time"

	"torrentd/internal/bt/metainfo"
	"torrentd/internal/clock"
)

// State is a torrent state (§5.1, §16.4). The values match the TorrentState
// schema in spec/openapi.yaml. M4 produces checking, downloading, seeding,
// stopped, paused and error.
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

// StopReason says why a torrent is stopped (§5.2).
type StopReason string

// Stop reasons (the StopReason schema).
const (
	StopReasonUser       StopReason = "user"
	StopReasonRatioLimit StopReason = "ratio_limit"
	StopReasonTimeLimit  StopReason = "time_limit"
)

// Desired states, as stored in torrents.desired_state (§7.2).
const (
	DesiredActive  = "active"
	DesiredPaused  = "paused"
	DesiredStopped = "stopped"
)

// ShutdownAnnounceTimeout bounds the stopped announces on Close (§6.2) and
// on pause or stop seeding.
const ShutdownAnnounceTimeout = 3 * time.Second

// FlushInterval is how often torrents are saved to the Store (§6).
const FlushInterval = 10 * time.Second

// Ban policy (ADR-0006).
const (
	// BanStrikes is the number of strikes that bans an IP.
	BanStrikes = 3
	// BanDuration is how long a ban lasts.
	BanDuration = 24 * time.Hour
)

// FilesMissingMessage is the error message when every file of a torrent
// with verified pieces is gone (G8, ADR-0004).
const FilesMissingMessage = "Files missing"

// Errors returned by Session methods (use errors.Is).
var (
	// ErrAlreadyExists: a torrent with the same info-hash is registered.
	ErrAlreadyExists = errors.New("session: torrent already exists")
	// ErrInvalidDownloadDir: the download dir is not absolute or cannot be
	// created.
	ErrInvalidDownloadDir = errors.New("session: invalid download dir")
	// ErrClosed: the Session has been closed.
	ErrClosed = errors.New("session: closed")
	// ErrNotFound: no torrent has the given id.
	ErrNotFound = errors.New("session: no such torrent")
	// ErrInvalidState: the action is not allowed in the torrent's state
	// (G18), e.g. pausing a complete torrent.
	ErrInvalidState = errors.New("session: invalid state for this action")
	// ErrNotComplete: stop seeding on an incomplete torrent (G18).
	ErrNotComplete = errors.New("session: torrent is not complete")
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
	// Store persists torrents and bans; nil means no persistence.
	Store Store
	// PreviousShutdownClean tells Load whether the last shutdown was clean
	// (§16.3 C3): it selects the size+mtime check over the size-only check.
	PreviousShutdownClean bool
	// DiskWorkers and HashWorkers size the process-wide pools (§4.5,
	// D9); 0 means 4 and max(1, GOMAXPROCS/2).
	DiskWorkers, HashWorkers int
}

// Record is the persisted form of one torrent (the torrents row, §7.1).
type Record struct {
	// InfoHash is the v1 info-hash.
	InfoHash [20]byte
	// Name is the torrent name.
	Name string
	// RawInfo is the bencoded info dict.
	RawInfo []byte
	// Trackers are the announce URLs with their tiers.
	Trackers []metainfo.Tracker
	// Private is the BEP 27 flag.
	Private bool
	// SizeBytes is the size of the wanted files; PieceLength the nominal
	// piece length.
	SizeBytes, PieceLength int64
	// DownloadDir is the absolute download dir.
	DownloadDir string
	// DesiredState is DesiredActive, DesiredPaused or DesiredStopped.
	DesiredState string
	// StopReason is set when DesiredState is DesiredStopped.
	StopReason StopReason
	// Bitfield is the verified pieces in wire format.
	Bitfield []byte
	// Fingerprint is the resume fingerprint (JSON: per-file size and
	// mtime at the last flush); empty if unknown.
	Fingerprint string
	// Lifetime payload totals.
	UploadedTotal, DownloadedTotal, WastedTotal int64
	// QueuePosition is the add order.
	QueuePosition int
	// AddedAt and CompletedAt (zero if never).
	AddedAt, CompletedAt time.Time
	// LastError is the error message in state error.
	LastError string
}

// Ban is one banned IP (banned_peers, ADR-0006).
type Ban struct {
	// IP is the banned address.
	IP netip.Addr
	// Reason is a human-readable reason.
	Reason string
	// BannedAt and ExpiresAt bound the ban.
	BannedAt, ExpiresAt time.Time
}

// Store is the persistence the Session needs; internal/store implements
// it. Methods are called from Session goroutines, never from an actor.
type Store interface {
	// LoadTorrents returns every saved torrent ordered by QueuePosition.
	LoadTorrents(ctx context.Context) ([]Record, error)
	// SaveTorrents inserts or updates the records in one transaction.
	SaveTorrents(ctx context.Context, recs []Record) error
	// LoadBans returns the bans that have not expired at now.
	LoadBans(ctx context.Context, now time.Time) ([]Ban, error)
	// SaveBan inserts or replaces a ban.
	SaveBan(ctx context.Context, b Ban) error
}

// AddOptions are the per-torrent parameters of Add.
type AddOptions struct {
	// DownloadDir is the absolute download dir; it is created with its
	// parents if missing. Empty means Config.DefaultDownloadDir.
	DownloadDir string
	// StartPaused adds the torrent in state paused (no network activity).
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
	// StopReason is set in state stopped.
	StopReason StopReason
	// DownloadRate and UploadRate are payload bytes/s, smoothed per peer
	// (the stats sampler's 5 s EMA arrives in M6).
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
	// Status (state checking, or paused with StartPaused).
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
	// Load restores the torrents saved in Config.Store (§6.1 step 4) and
	// starts them in their desired state. It is a no-op without a Store.
	// Call it once, before Add.
	Load(ctx context.Context) error
	// Pause stops an incomplete torrent (desired state paused). Allowed in
	// checking and downloading; a no-op when paused; ErrInvalidState on a
	// complete torrent or in error; ErrNotFound for an unknown id.
	Pause(id [20]byte) error
	// Resume resumes a paused torrent, resumes seeding a stopped one
	// (clearing StopReason), or retries a torrent in error (full recheck).
	// It is a no-op on an active torrent. ErrNotFound for an unknown id.
	Resume(id [20]byte) error
	// StopSeeding stops a complete torrent with StopReasonUser (§5.2).
	// ErrNotComplete on an incomplete torrent; a no-op when stopped;
	// ErrInvalidState in checking or error. ErrNotFound for an unknown id.
	StopSeeding(id [20]byte) error
	// Close stops every torrent: it sends the stopped announces in
	// parallel, bounded by ShutdownAnnounceTimeout and ctx, closes the
	// listener, peers and files, waits for all engine goroutines, and then
	// saves every torrent to the Store. It is idempotent. After Close, Add
	// returns ErrClosed; List and Get still return the last Status.
	Close(ctx context.Context) error
}

// New starts a Session: it binds the peer port and returns. A bind failure
// is not an error (see ListenErr); New fails only on invalid Config.
func New(cfg Config) (Session, error) { return newSession(cfg) }
