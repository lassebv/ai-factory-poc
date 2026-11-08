// Package piece holds the piece bitfield and the piece picker (§4.1 Piece
// selection, §16.2 A5, D1).
//
// FROZEN CONTRACT (ADR-0011, ADR-0012, ADR-0017). This file is owned by the
// architect.
// Implementers provide the unexported functions and the Bitfield methods
// listed below in their own files (replacing stub.go) and must not change
// anything declared here.
//
// # What the implementer provides
//
//	func newPicker(l Layout, rnd *rand.Rand) Picker
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
// it. This package starts no goroutines and does no I/O.
package piece

import (
	"errors"
	"math/rand/v2"
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
// A key must not be reused while the peer is registered.
type PeerKey uint64

// Priority is a piece priority class (§4.3, A5). A piece's priority is the
// maximum priority of the wanted files it overlaps; M6 sets it from file
// priorities. Every piece starts at PriorityNormal.
type Priority int8

// Priority classes, lowest first. PrioritySkip pieces are never picked and
// do not count for Done.
const (
	PrioritySkip Priority = iota
	PriorityLow
	PriorityNormal
	PriorityHigh
)

// Picker decides which blocks to request from which peer. It tracks peer
// availability, per-peer block ownership, received blocks and verified
// pieces.
//
// Block states: a block is *needed* until it is picked (then *outstanding*
// for one peer, or several in endgame) and then *received*. A piece whose
// blocks are all received is *pending* until the caller reports the hash
// result: PieceVerified makes it *have*; PieceFailed makes all its blocks
// needed again.
//
// Selection rules for Pick (M4; ADR-0017). Only blocks of wanted pieces the
// peer has are returned, and never a block already outstanding for p.
// Order:
//
//  1. Needed blocks of in-progress pieces that no other peer has blocks
//     outstanding in (piece affinity), highest priority then lowest index.
//  2. New pieces, strictly by priority class. Within a class: while fewer
//     than 4 pieces are verified and the Picker has a random source,
//     random order (random-first); otherwise rarest first (fewest
//     registered peers having them), ties broken randomly (by lowest index
//     without a random source). Pick does not sort every piece.
//  3. Needed blocks of in-progress pieces other peers are working on.
//  4. Endgame (A5): once every wanted block has been requested at least
//     once, blocks outstanding for other peers are handed out again.
//     Received reports the other owners so the caller can cancel them.
//
// Within a piece, blocks are returned in ascending Begin order.
type Picker interface {
	// AddPeer registers p with no pieces. Adding a registered peer is a no-op.
	AddPeer(p PeerKey)
	// RemovePeer forgets p's availability and makes its outstanding blocks
	// needed again (unless still outstanding for another peer). Unknown
	// peers are ignored.
	RemovePeer(p PeerKey)
	// PeerBitfield replaces p's availability with bf (registering p if
	// needed). A bf whose Len differs from Layout.NumPieces is ignored.
	PeerBitfield(p PeerKey, bf *Bitfield)
	// PeerHave adds piece index to p's availability (registering p if
	// needed). Out-of-range indexes are ignored.
	PeerHave(p PeerKey, index int)
	// Pick returns up to max blocks to request from p, marks them
	// outstanding for p, and returns nil if there is nothing to request.
	Pick(p PeerKey, max int) []Block
	// Received records a block from p. accepted is true only if b is
	// exactly a block of the layout that is outstanding for p (requested
	// from p and neither received, cancelled nor released since); it then
	// becomes received and leaves every peer's outstanding set. cancel
	// lists the other peers it was outstanding for (endgame duplicates),
	// whose requests the caller should cancel. pieceDone is true if this
	// block made its piece pending, which means the caller must now hash
	// it. A rejected block is wasted data.
	Received(p PeerKey, b Block) (accepted, pieceDone bool, cancel []PeerKey)
	// Release makes p's outstanding blocks needed again (unless still
	// outstanding for another peer) but keeps its availability, e.g. when
	// p chokes us or a request timed out. It returns the released blocks
	// in (Index, Begin) order so the caller can cancel them.
	Release(p PeerKey) []Block
	// Outstanding returns the number of blocks outstanding for p.
	Outstanding(p PeerKey) int
	// PieceVerified marks a pending piece as have. It also accepts a piece
	// in any other state (e.g. verified during checking) and marks it have.
	PieceVerified(index int)
	// PieceFailed makes every block of a pending piece needed again.
	PieceFailed(index int)
	// Discard makes the received blocks of an in-progress piece needed
	// again (e.g. they came from a peer that was just banned). Outstanding
	// blocks stay outstanding. Other states are ignored.
	Discard(index int)
	// SetPriority sets the priority class of piece index. Out-of-range
	// indexes and priorities are ignored.
	SetPriority(index int, prio Priority)
	// Endgame reports whether every wanted block has been requested at
	// least once while some wanted piece is still missing.
	Endgame() bool
	// Have returns a copy of the verified pieces.
	Have() *Bitfield
	// HaveCount returns the number of verified pieces.
	HaveCount() int
	// Done reports whether every wanted (not PrioritySkip) piece is have.
	Done() bool
}

// NewPicker returns a Picker for the layout with nothing verified and no
// peers. rnd drives random-first and tie-breaking; nil gives a deterministic
// picker (no random-first, ties by lowest index).
func NewPicker(l Layout, rnd *rand.Rand) Picker { return newPicker(l, rnd) }

// PipelineDepth returns the number of requests to keep outstanding for a
// peer that delivers rate payload bytes per second (D1):
// clamp(rate*2/BlockSize, MinPipelineDepth, MaxPipelineDepth). A rate <= 0
// gives MinPipelineDepth.
func PipelineDepth(rate int64) int { return pipelineDepth(rate) }
