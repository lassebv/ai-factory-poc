// Package tracker is the HTTP/HTTPS tracker client (BEP 3, BEP 23), the
// BEP 12 tiered announcer and the per-torrent announce loop (§4.1, §6.2,
// §16.2 A4, D7). UDP trackers (BEP 15) and scrape are not in M3.
//
// FROZEN CONTRACT (ADR-0011, ADR-0014). This file is owned by the architect.
// The implementer provides, in their own files (replacing stub.go):
//
//	func newHTTPClient(hc *http.Client) Client
//	func newAnnouncer(tiers [][]string, c Client, rnd *rand.Rand) Announcer
//	func startLoop(cfg LoopConfig) Loop
//	func (e Event) String() string
//	func (e *FailureError) Error() string
//
// # HTTP announce wire format
//
// The query is appended to the announce URL (with "&" if it already has a
// query). Parameters: info_hash, peer_id (raw 20 bytes, every byte outside
// [A-Za-z0-9.-_~] percent-encoded as %XX; never "+"), port, uploaded,
// downloaded, left, compact=1, numwant (NumWant), key, event (omitted for
// EventNone) and trackerid (only when the tracker gave one). The ipv6=
// parameter (D7) is not sent at M3.
//
// The response is bencode, decoded leniently (§16.2 A1). "failure reason"
// gives a *FailureError. Otherwise: interval, min interval, tracker id,
// warning message, complete, incomplete, and peers in either form: compact
// (6 bytes per IPv4 peer) or a list of dicts with "ip" (an IP literal;
// hostnames are skipped at M3), "port" and optional "peer id". A compact
// "peers6" (18 bytes per peer, BEP 7) is also read. Peers with port 0 are
// dropped. A non-200 status or a body that is not a bencoded dict is an
// error.
package tracker

import (
	"context"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/netip"
	"time"

	"torrentd/internal/clock"
)

// NumWant is the numwant parameter of every announce (D7).
const NumWant = 50

// Loop timing.
const (
	// DefaultInterval is used when a response has no (or a non-positive)
	// interval.
	DefaultInterval = 30 * time.Minute
	// MinRetry is the first retry delay after a failed announce; each
	// further consecutive failure doubles it, capped at MaxRetry.
	MinRetry = 15 * time.Second
	// MaxRetry caps the retry delay.
	MaxRetry = 30 * time.Minute
)

// Event is the announce event.
type Event int

// Announce events. String returns "", "started", "completed", "stopped".
const (
	EventNone Event = iota
	EventStarted
	EventCompleted
	EventStopped
)

// AnnounceRequest holds the parameters of one announce.
type AnnounceRequest struct {
	// InfoHash is the torrent's v1 info-hash.
	InfoHash [20]byte
	// PeerID is our peer ID (D13); the same for every announce of a process.
	PeerID [20]byte
	// Port is our bound peer port.
	Port uint16
	// Uploaded, Downloaded and Left are payload byte counts. Left is the
	// wanted bytes not yet verified (sizeBytes - completedBytes).
	Uploaded, Downloaded, Left int64
	// Event is the announce event.
	Event Event
	// Key is the per-process random key (D7).
	Key string
	// TrackerID is sent as trackerid when non-empty. The Announcer fills it
	// per tracker URL; callers of Announcer leave it empty.
	TrackerID string
}

// AnnounceResponse is a successful tracker response.
type AnnounceResponse struct {
	// Interval is the regular announce interval (0 if absent).
	Interval time.Duration
	// MinInterval is the minimum announce interval (0 if absent).
	MinInterval time.Duration
	// TrackerID is the tracker id to echo back ("" if absent).
	TrackerID string
	// Warning is the warning message ("" if absent).
	Warning string
	// Seeders and Leechers are complete/incomplete, -1 if absent.
	Seeders, Leechers int
	// Peers are the returned peers in response order (IPv4, then peers6).
	Peers []netip.AddrPort
}

// FailureError is a tracker response carrying "failure reason".
type FailureError struct {
	// Reason is the failure reason text.
	Reason string
}

// Client performs single announces to one tracker URL. It is safe for
// concurrent use. Announce honors ctx for cancellation and deadlines.
type Client interface {
	// Announce sends req to announceURL and parses the response. Only
	// http and https URLs are supported; others return an error.
	Announce(ctx context.Context, announceURL string, req AnnounceRequest) (*AnnounceResponse, error)
}

// NewHTTPClient returns a Client using hc (nil means a client with a 30 s
// timeout). TLS verification follows hc.
func NewHTTPClient(hc *http.Client) Client { return newHTTPClient(hc) }

// Announcer announces through a torrent's tracker tiers following BEP 12.
// It is safe for concurrent use.
//
//   - At construction each tier is shuffled once with rnd.
//   - Announce tries tier 0 in order, then tier 1, etc. The first tracker
//     that succeeds is moved to the front of its tier and its response is
//     returned. A *FailureError counts as a failure of that tracker.
//   - If every tracker fails, Announce returns an error that wraps the
//     individual errors (errors.Join), so errors.As finds a *FailureError.
//   - A tracker id returned by a URL is stored and sent to that URL as
//     req.TrackerID from then on.
//   - With no trackers at all, Announce returns an error immediately.
type Announcer interface {
	// Announce sends req as described above.
	Announce(ctx context.Context, req AnnounceRequest) (*AnnounceResponse, error)
	// Tiers returns the current tier order (a copy).
	Tiers() [][]string
}

// NewAnnouncer returns an Announcer for tiers (empty URLs and empty tiers
// are dropped). rnd nil means no shuffling.
func NewAnnouncer(tiers [][]string, c Client, rnd *rand.Rand) Announcer {
	return newAnnouncer(tiers, c, rnd)
}

// Stats are the counters sent with an announce.
type Stats struct {
	// Uploaded, Downloaded and Left: see AnnounceRequest.
	Uploaded, Downloaded, Left int64
}

// LoopConfig configures a per-torrent announce loop.
type LoopConfig struct {
	// Announcer is the torrent's tiered announcer. Required.
	Announcer Announcer
	// InfoHash, PeerID, Port and Key fill every AnnounceRequest.
	InfoHash, PeerID [20]byte
	// Port is our bound peer port.
	Port uint16
	// Key is the per-process key (D7).
	Key string
	// Stats returns the current counters. It is called from the loop
	// goroutine right before each announce and must be safe for that.
	Stats func() Stats
	// OnPeers receives the peers of every successful announce that returned
	// at least one. It is called from the loop goroutine; it should hand the
	// peers off quickly (e.g. a non-blocking send to the torrent actor).
	OnPeers func([]netip.AddrPort)
	// Clock drives the intervals and retries; nil means clock.Real(). The
	// loop holds at most one Clock timer at a time, and only while it waits
	// for the next announce (tests rely on this to know when it is idle).
	// HTTP timeouts come from ctx or the http.Client, not from Clock.
	Clock clock.Clock
	// Logger receives warnings and errors; nil means slog.Default().
	Logger *slog.Logger
}

// Loop is a running per-torrent announce loop. All methods are safe for
// concurrent use.
//
// Behavior:
//   - StartLoop immediately announces EventStarted from the loop goroutine.
//   - After a failure the same announce (same event) is retried after
//     MinRetry, doubling up to MaxRetry, by the Clock.
//   - After a success the next regular announce (EventNone) is sent after
//     max(Interval, MinInterval), or DefaultInterval if Interval <= 0.
//   - Completed: as soon as the started announce has succeeded and no
//     announce is in flight, one EventCompleted announce is sent without
//     waiting for the interval (retried like any other on failure). Further
//     calls are no-ops.
//   - Stop(ctx): halts the loop (abandoning an in-flight or pending
//     announce), then makes exactly one EventStopped announce through the
//     Announcer, bounded by ctx, and returns its error. It waits for the
//     loop goroutine to exit and leaves no goroutines behind. Later calls
//     return nil without announcing; Completed after Stop is a no-op.
type Loop interface {
	// Completed requests the completed announce (see above).
	Completed()
	// Stop stops the loop and sends the stopped announce (see above).
	Stop(ctx context.Context) error
}

// StartLoop starts the announce loop for cfg and returns at once.
func StartLoop(cfg LoopConfig) Loop { return startLoop(cfg) }
