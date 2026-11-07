// Package session orchestrates the engine: one Session per process and one
// torrent actor per torrent (§4.4, §4.5, §5, §6.1, §6.2, ADR-0011).
//
// FROZEN CONTRACT (ADR-0011, ADR-0015). This file is owned by the architect.
// The integrator provides, in their own files (replacing stub.go):
//
//	func newSession(cfg Config) (Session, error)
//
// # Milestone 3 behavior
//
//   - Add validates and registers a torrent and starts its actor. The actor
//     goes checking -> downloading -> seeding (or paused with StartPaused).
//     At M3 checking only opens storage (sparse .part preallocation); there
//     is no resume data yet.
//   - On entering downloading, the actor starts a tracker.Loop (event
//     started). On completion through downloading it calls Loop.Completed
//     and moves to seeding (completedAt set). Close sends stopped.
//   - Tracker peers are dialled over TCP (max 50 peers per torrent, at most
//     20 half-open dials per torrent, 10 s dial+handshake timeout). Each
//     peer gets a bitfield check, interested, and a request pipeline of
//     piece.PipelineDepth(rate) blocks.
//   - Incoming connections on the peer port may be accepted and closed (M4
//     adds seeding).
//   - Counters: DownloadedTotal counts every payload byte received in piece
//     messages; WastedTotal counts payload bytes discarded (rejected by the
//     picker, or a whole piece whose hash failed); CompletedBytes is the sum
//     of storage.WantedBytes over verified pieces.
//
// # Concurrency (ADR-0015)
//
//   - Each torrent actor is one goroutine owning all torrent state (picker,
//     peers, counters). Peer read goroutines, storage jobs and the tracker
//     loop talk to it only through its event channel.
//   - Get and List never wait for an actor: they read the last Status the
//     actor published (an atomic snapshot), so they return promptly even
//     while the actor is busy. The actor publishes after every state change
//     and every verified piece; a published Status is internally consistent
//     (e.g. state seeding implies CompletedBytes == SizeBytes).
//   - All Session methods are safe for concurrent use.
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

// Errors returned by Session methods (use errors.Is).
var (
	// ErrAlreadyExists: a torrent with the same info-hash is registered.
	ErrAlreadyExists = errors.New("session: torrent already exists")
	// ErrInvalidDownloadDir: the download dir is not absolute or cannot be
	// created.
	ErrInvalidDownloadDir = errors.New("session: invalid download dir")
	// ErrClosed: the Session has been closed.
	ErrClosed = errors.New("session: closed")
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
	// DownloadRate and UploadRate are payload bytes/s (may be 0 at M3; the
	// smoothed rates arrive with the stats sampler in M6).
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
	// Close stops every torrent: it sends the stopped announces in
	// parallel, bounded by ShutdownAnnounceTimeout and ctx, closes the
	// listener, peers and files, and waits for all engine goroutines. It is
	// idempotent. After Close, Add returns ErrClosed; List and Get still
	// return the last Status.
	Close(ctx context.Context) error
}

// New starts a Session: it binds the peer port and returns. A bind failure
// is not an error (see ListenErr); New fails only on invalid Config.
func New(cfg Config) (Session, error) { return newSession(cfg) }
