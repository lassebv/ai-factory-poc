package piece

import "math/rand/v2"

// STUB: placeholders so the tree compiles before the implementation exists.
// The implementer deletes this file and provides real versions (see the
// list in contract.go).

var errStub = "piece: not implemented"

func newPicker(Layout, *rand.Rand) Picker { panic(errStub) }
func pipelineDepth(int64) int             { panic(errStub) }

// PieceSize returns the size of piece i (see Layout).
func (l Layout) PieceSize(i int) int64 { panic(errStub) }

// NumBlocks returns the number of blocks in piece i (see Layout).
func (l Layout) NumBlocks(i int) int { panic(errStub) }

// NewBitfield returns an all-clear bitfield of n pieces.
func NewBitfield(n int) *Bitfield { panic(errStub) }

// FromBytes copies a wire-format bitfield of n pieces.
func FromBytes(b []byte, n int) (*Bitfield, error) { panic(errStub) }

// Len returns the number of pieces.
func (b *Bitfield) Len() int { panic(errStub) }

// Has reports whether piece i is set.
func (b *Bitfield) Has(i int) bool { panic(errStub) }

// Set sets piece i.
func (b *Bitfield) Set(i int) { panic(errStub) }

// Clear clears piece i.
func (b *Bitfield) Clear(i int) { panic(errStub) }

// Count returns the number of set pieces.
func (b *Bitfield) Count() int { panic(errStub) }

// Full reports whether every piece is set.
func (b *Bitfield) Full() bool { panic(errStub) }

// Bytes returns the wire format.
func (b *Bitfield) Bytes() []byte { panic(errStub) }

// Clone returns a deep copy.
func (b *Bitfield) Clone() *Bitfield { panic(errStub) }
