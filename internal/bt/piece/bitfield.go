package piece

import "math/bits"

// NewBitfield returns an all-clear bitfield of n pieces. A negative n is
// treated as 0.
func NewBitfield(n int) *Bitfield {
	if n < 0 {
		n = 0
	}
	return &Bitfield{n: n, bits: make([]byte, (n+7)/8)}
}

// FromBytes copies a wire-format bitfield of n pieces. It returns
// ErrInvalidBitfield if len(b) != ceil(n/8) or a spare trailing bit is set.
func FromBytes(b []byte, n int) (*Bitfield, error) {
	if n < 0 || len(b) != (n+7)/8 {
		return nil, ErrInvalidBitfield
	}
	if rem := n % 8; rem != 0 {
		spare := byte(0xff) >> rem
		if b[len(b)-1]&spare != 0 {
			return nil, ErrInvalidBitfield
		}
	}
	return &Bitfield{n: n, bits: append([]byte(nil), b...)}, nil
}

// Len returns the number of pieces.
func (b *Bitfield) Len() int {
	if b == nil {
		return 0
	}
	return b.n
}

// Has reports whether piece i is set. It returns false for i out of range.
func (b *Bitfield) Has(i int) bool {
	if b == nil || i < 0 || i >= b.n {
		return false
	}
	return b.bits[i>>3]&(0x80>>(i&7)) != 0
}

// Set sets piece i. It is a no-op for i out of range.
func (b *Bitfield) Set(i int) {
	if b == nil || i < 0 || i >= b.n {
		return
	}
	b.bits[i>>3] |= 0x80 >> (i & 7)
}

// Clear clears piece i. It is a no-op for i out of range.
func (b *Bitfield) Clear(i int) {
	if b == nil || i < 0 || i >= b.n {
		return
	}
	b.bits[i>>3] &^= 0x80 >> (i & 7)
}

// Count returns the number of set pieces.
func (b *Bitfield) Count() int {
	if b == nil {
		return 0
	}
	c := 0
	for _, x := range b.bits {
		c += bits.OnesCount8(x)
	}
	return c
}

// Full reports whether every piece is set (true for a zero-length bitfield).
func (b *Bitfield) Full() bool { return b.Count() == b.Len() }

// Bytes returns a fresh copy in wire format: ceil(n/8) bytes, spare bits
// zero.
func (b *Bitfield) Bytes() []byte {
	if b == nil {
		return []byte{}
	}
	return append([]byte{}, b.bits...)
}

// Clone returns an independent deep copy.
func (b *Bitfield) Clone() *Bitfield {
	if b == nil {
		return NewBitfield(0)
	}
	return &Bitfield{n: b.n, bits: append([]byte(nil), b.bits...)}
}
