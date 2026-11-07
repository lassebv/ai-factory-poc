// Package piece holds the piece bitfield and the piece picker: which blocks
// to request next from which peer (§4.1 Piece selection).
package piece

import (
	"errors"
	"math/bits"
)

// ErrBadBitfield is returned for a bitfield of the wrong length or with
// spare bits set.
var ErrBadBitfield = errors.New("piece: malformed bitfield")

// Bitfield is a fixed-size set of piece indexes, stored in the BEP 3 wire
// format (the high bit of byte 0 is piece 0).
type Bitfield struct {
	b []byte
	n int
}

// NewBitfield returns an empty bitfield for n pieces.
func NewBitfield(n int) *Bitfield {
	return &Bitfield{b: make([]byte, (n+7)/8), n: n}
}

// FromBytes parses a wire bitfield for n pieces. It must be exactly
// ceil(n/8) bytes long and its spare bits must be zero.
func FromBytes(b []byte, n int) (*Bitfield, error) {
	if len(b) != (n+7)/8 {
		return nil, ErrBadBitfield
	}
	if r := n % 8; r != 0 && b[len(b)-1]&(0xff>>r) != 0 {
		return nil, ErrBadBitfield
	}
	return &Bitfield{b: append([]byte(nil), b...), n: n}, nil
}

// Len returns the number of pieces the bitfield covers.
func (f *Bitfield) Len() int { return f.n }

// Has reports whether piece i is set. Out-of-range indexes are never set.
func (f *Bitfield) Has(i int) bool {
	if i < 0 || i >= f.n {
		return false
	}
	return f.b[i/8]&(0x80>>(i%8)) != 0
}

// Set marks piece i. Out-of-range indexes are ignored.
func (f *Bitfield) Set(i int) {
	if i >= 0 && i < f.n {
		f.b[i/8] |= 0x80 >> (i % 8)
	}
}

// Clear unmarks piece i. Out-of-range indexes are ignored.
func (f *Bitfield) Clear(i int) {
	if i >= 0 && i < f.n {
		f.b[i/8] &^= 0x80 >> (i % 8)
	}
}

// Count returns the number of set pieces.
func (f *Bitfield) Count() int {
	c := 0
	for _, x := range f.b {
		c += bits.OnesCount8(x)
	}
	return c
}

// Full reports whether every piece is set.
func (f *Bitfield) Full() bool { return f.Count() == f.n }

// Bytes returns a copy of the wire encoding.
func (f *Bitfield) Bytes() []byte { return append([]byte(nil), f.b...) }
