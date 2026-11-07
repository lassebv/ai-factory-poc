// Package bencode implements the BitTorrent bencoding (BEP 3) with
// reflection-based struct mapping, in two decoding modes (§16.1 A1):
//
//   - Unmarshal (lenient) accepts unsorted or duplicate dict keys and
//     non-canonical integers and lengths, but still rejects malformed input
//     and trailing data. It is meant for tracker, DHT and extension messages.
//   - UnmarshalStrict additionally rejects every non-canonical encoding, so
//     a value that decodes strictly re-encodes to exactly the same bytes. It
//     is used for the info dict and the ut_metadata payload.
//
// Both modes cap nesting at MaxDepth lists/dicts and check every string
// length against the remaining input before allocating (§16.2 T6).
//
// Struct fields map to dict keys through `bencode:"name"`,
// `bencode:"name,omitempty"` and `bencode:"-"` tags; untagged exported fields
// use the Go field name.
package bencode

import (
	"errors"
	"fmt"
)

// MaxDepth is the deepest nesting of lists and dicts that decoding accepts.
const MaxDepth = 64

// RawMessage holds the exact encoded bytes of a value. Decoding stores the
// input bytes verbatim; Marshal writes them back unchanged.
type RawMessage []byte

// SyntaxError reports malformed or (in strict mode) non-canonical input.
type SyntaxError struct {
	// Offset is the byte offset in the input where the problem was found.
	Offset int
	// Msg describes the problem.
	Msg string
}

// Error implements error.
func (e *SyntaxError) Error() string {
	return fmt.Sprintf("bencode: %s at offset %d", e.Msg, e.Offset)
}

// TypeError reports a bencode value that cannot be stored in, or a Go value
// that cannot be encoded as, the requested type.
type TypeError struct {
	// Msg describes the mismatch.
	Msg string
}

// Error implements error.
func (e *TypeError) Error() string { return "bencode: " + e.Msg }

// ErrNilTarget is returned when Unmarshal is given a nil or non-pointer
// target.
var ErrNilTarget = errors.New("bencode: Unmarshal target must be a non-nil pointer")
