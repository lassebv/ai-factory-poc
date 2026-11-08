// Package piece holds the piece bitfield and the piece picker (§4.1 Piece
// selection, §16.2 A5, D1).
//
// FROZEN CONTRACT (ADR-0011, ADR-0012, ADR-0018). This file is owned by the
// architect. Implementers provide the unexported functions and the Bitfield
// methods listed below in their own files and must not change anything
// declared here.
//
// # What the implementer provides
//
//	func newPicker(l Layout, opts Options) Picker
//	func pipelineDepth(rate int64) int
//	func (l Layout) PieceSize(i int) int64
//	func (l Layout) NumBlocks(i int) int
//
//	func NewBitfield(n int) *Bitfield
//	func FromBytes(b []byte, n int) (*Bitfield, error)
//	func (b *Bitfield) Len() int
//	func (b *Bitfield) Has(i int) bool
//	func (b *Bitfield) Set(i int)
//	func (b *Bitfield) Clear(i int)
//	func (b *Bitfield) Count() int
//	func (b *Bitfield) Full() bool
//	func (b *Bitfield) Bytes() []byte
//	func (b *Bitfield) Clone() *Bitfield
//
// # Concurrency
//
// Neither Bitfield nor Picker is safe for concurrent use. A Picker is owned
// by exactly one torrent actor goroutine (§4.5); peer goroutines never call
// it. This package starts no goroutines and does no I/O; it reads the time
// only through Options.Clock.
//
// # Cost (CPU-01, ADR-0018)
//
// Pick, Received, PeerHave and Expire must not scan or sort every piece of
// the torrent on each call. Amortized cost per call is O(blocks returned +
// pieces of the peer's candidate set examined), with rarest-first served
// from availability buckets (or an equivalent incremental structure), so
// that a 4096-piece and a 65536-piece torrent cost about the same per
// block. The contract test TestContractPickerCost guards this.
package piece

import (
	"errors"
	"math/rand/v2"
	"net/netip"
	"time"

	"torrentd/internal/clock"
)

// BlockSize is the size of a block request: 16 KiB. Only the last block of a
// piece may be shorter (WIRE-05).
const BlockSize = 16 << 10

// Pipeline depth bounds (D1): the number of outstanding requests per peer.
const (
	// MinPipelineDepth is the depth used for a peer with no measured rate.
	MinPipelineDepth = 5
	// MaxPipelineDepth is the hard cap on outstanding requests per peer.
	MaxPipelineDepth = 250
)

// RandomFirstPieces is the number of verified pieces below which new
// pieces are chosen at random instead of rarest-first (§4.1).
const RandomFirstPieces = 4

// DefaultRequestTimeout is how long a block may stay outstanding for one
// peer before Expire takes it back (ADR-0018).
const DefaultRequestTimeout = 20 * time.Second

// ErrInvalidBitfield is returned by FromBytes when the byte length is not
// ceil(n/8) or a spare bit after the last piece is set (BEP 3).
var ErrInvalidBitfield = errors.New("piece: invalid bitfield")

// Bitfield is a fixed-length set of piece indices in BEP 3 wire order: piece
// 0 is the high bit of byte 0. The zero value is an empty bitfield of length
// 0. Methods (provided by the implementer):
//
//   - NewBitfield(n) returns an all-clear bitfield of n pieces (n >= 0).
//   - FromBytes(b, n) copies a wire bitfield. It returns ErrInvalidBitfield
//     if len(b) != ceil(n/8) or any spare trailing bit is set.
//   - Len returns n. Count returns the number of set bits. Full reports
//     Count() == Len() (true for n == 0).
//   - Has(i) reports whether i is set; it returns false for i out of range.
//   - Set(i) and Clear(i) are no-ops for i out of range; they never panic.
//   - Bytes returns a fresh copy in wire format (ceil(n/8) bytes, spare bits
//     zero). Clone returns an independent deep copy.
type Bitfield struct {
	n    int
	bits []byte
}

// Block identifies a block request within a piece. Field types match the
// peer wire format.
type Block struct {
	// Index is the piece index.
	Index uint32
	// Begin is the byte offset within the piece, a multiple of BlockSize.
	Begin uint32
	// Length is BlockSize, or less for the last block of a piece.
	Length uint32
}

// Layout describes the piece geometry of a torrent.
//
// PieceSize(i) is PieceLength for every piece but the last, which is
// TotalLength - (NumPieces-1)*PieceLength. NumBlocks(i) is
// ceil(PieceSize(i)/BlockSize). Both return 0 for i out of range.
type Layout struct {
	// NumPieces is the number of pieces (ceil(TotalLength/PieceLength)).
	NumPieces int
	// PieceLength is the nominal piece length from the info dict.
	PieceLength int64
	// TotalLength is the torrent's total length including BEP 47 pad files.
	TotalLength int64
}

// PeerKey is an opaque, caller-chosen identifier for one peer connection.
// A key must not be reused for another connection during the Picker's
// lifetime (the session allocates keys from a counter).
type PeerKey uint64

// Priority is a piece priority class (§4.3, A5). The zero value is
// PrioritySkip, but every piece starts at PriorityNormal.
type Priority int8

// Priority classes, in increasing order.
const (
	// PrioritySkip pieces are never requested and do not count for Done.
	PrioritySkip Priority = iota
	// PriorityLow pieces are requested only when no normal or high piece
	// can be requested from that peer.
	PriorityLow
	// PriorityNormal is the default.
	PriorityNormal
	// PriorityHigh pieces are always requested first.
	PriorityHigh
)

// Options configures a Picker.
type Options struct {
	// Rand drives random-first selection and tie-breaking. nil means a
	// fixed-seed source (deterministic).
	Rand *rand.Rand
	// Clock timestamps requests for Expire; nil means clock.Real().
	Clock clock.Clock
	// RequestTimeout is the Expire threshold; 0 means DefaultRequestTimeout.
	RequestTimeout time.Duration
}

// Receipt is the result of Picker.Received.
type Receipt struct {
	// Accepted is true if the block was taken: the caller must write it to
	// storage. A block that is not accepted is wasted data and must not be
	// written.
	Accepted bool
	// PieceDone is true if this block was the last missing block of its
	// piece: the piece is now pending and the caller must hash it (after
	// its block writes have completed) and report PieceVerified or
	// PieceFailed.
	PieceDone bool
	// Cancel lists the other peers that still had this exact block
	// outstanding (endgame duplicates). The block has been removed from
	// their outstanding sets; the caller sends them a cancel message.
	Cancel []PeerKey
}

// Expired is one block taken back from a peer by Expire.
type Expired struct {
	// Peer is the peer the block was outstanding for.
	Peer PeerKey
	// Block is the block; the caller sends Peer a cancel for it.
	Block Block
}

// Picker decides which blocks to request from which peer. It tracks peer
// availability, per-peer block ownership, received blocks, verified pieces
// and who contributed to each piece.
//
// # Block states
//
// A block of a wanted piece is *needed*, *outstanding* (for one or more
// peers, each recorded with its request time) or *received*. A piece whose
// blocks are all received is *pending* until the caller reports the hash
// result: PieceVerified makes it *have*; PieceFailed makes all its blocks
// needed again and clears its contributors.
//
// # Selection rules for Pick (ADR-0018)
//
// Pick(p, max) only returns blocks of pieces p has (per PeerBitfield and
// PeerHave), that are not have, not pending, not PrioritySkip, and not
// already outstanding for p. Never more than max blocks. Order:
//
//  1. Higher priority class first (strict; A5). Within the chosen class:
//  2. Pieces p already owns (p has outstanding or received blocks in it),
//     to finish them.
//  3. Needed blocks of pieces nobody has started, chosen at random while
//     fewer than RandomFirstPieces pieces are have, then rarest-first
//     (fewest registered peers having the piece; ties broken by Rand).
//  4. Needed blocks of pieces started by other peers (only when 2 and 3
//     yield nothing for p; this keeps one piece mostly from one peer, which
//     keeps innocent peers out of hash-failure strikes).
//  5. Endgame: when no needed block of a wanted piece remains anywhere
//     (every remaining block has been requested at least once, A5), blocks
//     outstanding for other peers but not for p, fewest current owners
//     first. A block is outstanding for at most EndgameMaxOwners peers.
//
// Within a piece, blocks are returned in ascending Begin order.
//
// # Single-source retry after a hash failure
//
// After PieceFailed(i) the piece is *exclusive*: rule 2 still applies, but
// rules 3-5 hand its blocks only to the first peer that picks from it
// (until that peer is removed, released or expired). A second failure then
// has a single contributor.
//
// # Ownership
//
// Received accepts a block only from a peer it was outstanding for, or
// from a peer it was taken back from by Release or Expire while the block
// is still needed or outstanding for others (a late answer is still a good
// answer). Anything else — unrequested, already received, of a have or
// pending piece, not exactly a layout block — is rejected (POISON-01).
type Picker interface {
	// AddPeer registers p, connected from origin (its IP; the zone is
	// ignored), with no pieces. Adding a registered peer is a no-op. Other
	// methods ignore unregistered peers.
	AddPeer(p PeerKey, origin netip.Addr)
	// RemovePeer forgets p's availability and makes the blocks only it had
	// outstanding needed again. Its contributions to pieces still count
	// (PieceFailed reports its origin). Unknown peers are ignored.
	RemovePeer(p PeerKey)
	// PeerBitfield replaces p's availability with bf. A bf whose Len
	// differs from Layout.NumPieces is ignored.
	PeerBitfield(p PeerKey, bf *Bitfield)
	// PeerHave adds piece index to p's availability. Out-of-range indexes
	// are ignored.
	PeerHave(p PeerKey, index int)
	// Interesting reports whether p has at least one piece that is wanted
	// (not skip) and not have. O(1).
	Interesting(p PeerKey) bool
	// IsSeed reports whether p has every piece.
	IsSeed(p PeerKey) bool
	// Pick returns up to max blocks to request from p (see the selection
	// rules), marks them outstanding for p with the current time, and
	// returns nil if there is nothing to request.
	Pick(p PeerKey, max int) []Block
	// Received records a block from p (see Ownership and Receipt). When
	// accepted, p is recorded as a contributor of the piece.
	Received(p PeerKey, b Block) Receipt
	// Release takes back every block outstanding for p but keeps its
	// availability, e.g. when p chokes us. Late arrivals remain acceptable.
	Release(p PeerKey)
	// Expire takes back every block that has been outstanding for its peer
	// for at least the request timeout and returns them (the caller sends
	// cancels). A peer that has expired blocks gets no new blocks from Pick
	// until it delivers a block or until the expiry is RequestTimeout old.
	Expire() []Expired
	// Outstanding returns the number of blocks outstanding for p.
	Outstanding(p PeerKey) int
	// SetPriority sets the priority class of piece index. Lowering a piece
	// to PrioritySkip does not discard received blocks. Out-of-range
	// indexes are ignored.
	SetPriority(index int, prio Priority)
	// PieceVerified marks a piece as have (from any state, e.g. verified
	// during checking) and forgets its contributors.
	PieceVerified(index int)
	// PieceFailed makes every block of a pending piece needed again, makes
	// the piece exclusive, and returns the distinct origins of the peers
	// that contributed blocks to it (each gets one strike, G10).
	PieceFailed(index int) []netip.Addr
	// Have returns a copy of the verified pieces.
	Have() *Bitfield
	// Done reports whether every piece that is not PrioritySkip is have.
	Done() bool
	// Endgame reports whether endgame mode is active (rule 5).
	Endgame() bool
}

// EndgameMaxOwners bounds how many peers one block may be outstanding for
// in endgame mode.
const EndgameMaxOwners = 3

// New returns a Picker for the layout with nothing verified, every piece
// PriorityNormal and no peers.
func New(l Layout, opts Options) Picker { return newPicker(l, opts) }

// NewPicker is New(l, Options{Rand: rnd}). Kept for M3 callers.
func NewPicker(l Layout, rnd *rand.Rand) Picker { return newPicker(l, Options{Rand: rnd}) }

// PipelineDepth returns the number of requests to keep outstanding for a
// peer that delivers rate payload bytes per second (D1):
// clamp(rate*2/BlockSize, MinPipelineDepth, MaxPipelineDepth). A rate <= 0
// gives MinPipelineDepth.
func PipelineDepth(rate int64) int { return pipelineDepth(rate) }
