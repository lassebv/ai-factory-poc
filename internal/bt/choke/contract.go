// Package choke is the choking algorithm (§4.1 Choking, BEP 3) and the
// per-peer transfer-rate meter it is fed with.
//
// FROZEN CONTRACT (ADR-0017). This file is owned by the architect. The
// implementer provides, in their own files (replacing stub.go):
//
//	func newChoker(rnd *rand.Rand) Choker
//	func newMeter(clk clock.Clock) Meter
//
// # The algorithm (pure, deterministic for a given Rand)
//
// The torrent actor calls Rechoke every RechokeInterval with the current
// peers and applies the returned unchoke set (sending choke/unchoke only for
// peers whose state changes). Between rechokes it calls Fill when a peer
// connects, disconnects or changes interest, so that a free slot is used at
// once without reshuffling the slots already given out.
//
//   - Only interested peers are unchoked. At most RegularSlots regular
//     slots plus one optimistic slot.
//   - Regular slots go to the interested, non-snubbed peers with the
//     highest rate: DownloadRate (what they send us) while downloading,
//     UploadRate (what we send them) while seeding. Ties are broken by
//     Rand, but a peer that currently holds a slot keeps it on a tie.
//   - The optimistic slot goes to a random interested peer that did not
//     get a regular slot. It is rotated at most every OptimisticInterval:
//     the current optimistic peer is kept until OptimisticInterval has
//     passed since it was chosen (or until it leaves or loses interest),
//     then a different eligible peer is chosen if one exists. Snubbed
//     peers are eligible for the optimistic slot only.
//   - If fewer than RegularSlots non-snubbed peers are interested, the
//     unused regular slots go to further interested peers (snubbed ones
//     last), so with N interested peers min(N, RegularSlots+1) are
//     unchoked (CHOKE-01: 6 interested leechers of a seed -> exactly 5).
//   - Snubbed (anti-snubbing, leeching only): we are interested in the
//     peer and it has sent us no block for SnubTimeout (LastBlock, or
//     ConnectedAt if it never sent one). Seeding mode never snubs.
//
// The Choker keeps only the optimistic peer, its choice time and the last
// result; it starts no goroutines, does no I/O and is not safe for
// concurrent use (one per torrent actor).
package choke

import (
	"math/rand/v2"
	"time"

	"torrentd/internal/bt/piece"
	"torrentd/internal/clock"
)

// Algorithm constants (§4.1).
const (
	// RegularSlots is the number of tit-for-tat unchoke slots.
	RegularSlots = 4
	// RechokeInterval is how often the actor calls Rechoke.
	RechokeInterval = 10 * time.Second
	// OptimisticInterval is the optimistic-unchoke rotation period.
	OptimisticInterval = 30 * time.Second
	// SnubTimeout is the anti-snubbing threshold.
	SnubTimeout = 60 * time.Second
	// MeterWindow is the sliding window of a Meter.
	MeterWindow = 5 * time.Second
)

// Peer is the choker's view of one connection, filled by the actor.
type Peer struct {
	// Key identifies the connection.
	Key piece.PeerKey
	// Interested is true if the peer is interested in us.
	Interested bool
	// AmInterested is true if we are interested in the peer.
	AmInterested bool
	// DownloadRate is payload bytes/s received from the peer.
	DownloadRate int64
	// UploadRate is payload bytes/s sent to the peer.
	UploadRate int64
	// ConnectedAt is when the connection was established.
	ConnectedAt time.Time
	// LastBlock is when the peer last sent us an accepted block; zero if
	// never.
	LastBlock time.Time
}

// Choker computes unchoke sets.
type Choker interface {
	// Rechoke re-evaluates every slot at now (see the package doc) and
	// returns the keys to unchoke; every other peer is choked. seeding
	// selects the seeding-mode rate. The result has no duplicates and
	// only contains keys from peers.
	Rechoke(now time.Time, peers []Peer, seeding bool) []piece.PeerKey
	// Fill keeps the peers of the last result that are still present and
	// interested, and fills free slots (regular first, then optimistic)
	// following the same rules. It never chokes a peer that is still
	// eligible.
	Fill(now time.Time, peers []Peer, seeding bool) []piece.PeerKey
	// Optimistic returns the current optimistic peer, if any.
	Optimistic() (piece.PeerKey, bool)
}

// NewChoker returns a Choker; rnd must not be nil.
func NewChoker(rnd *rand.Rand) Choker { return newChoker(rnd) }

// Meter measures a byte rate over the last MeterWindow by the clock. It is
// safe for concurrent use (peer goroutines may Add while the actor reads).
type Meter interface {
	// Add records n bytes now.
	Add(n int64)
	// Rate returns bytes/s averaged over the last MeterWindow (or over the
	// meter's lifetime if shorter, but at least 1 s).
	Rate() int64
	// Total returns every byte ever added.
	Total() int64
}

// NewMeter returns a Meter on clk (nil means clock.Real()).
func NewMeter(clk clock.Clock) Meter { return newMeter(clk) }
