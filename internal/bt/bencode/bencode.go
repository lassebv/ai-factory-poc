// Package bencode will hold the bencode encoder and decoder (§4.1, §16.1 A1).
//
// Milestone 1 placeholder: only the pinned API signatures exist, so the tree
// type-checks. Every function returns ErrNotImplemented until milestone 2.
package bencode

import "errors"

// ErrNotImplemented is returned by every function until milestone 2.
var ErrNotImplemented = errors.New("bencode: not implemented (milestone 2)")

// RawMessage holds the exact encoded bytes of a value.
type RawMessage []byte

// Marshal encodes v canonically. Not implemented yet.
func Marshal(v any) ([]byte, error) { return nil, ErrNotImplemented }

// Unmarshal decodes data in lenient mode. Not implemented yet.
func Unmarshal(data []byte, v any) error { return ErrNotImplemented }

// UnmarshalStrict decodes data in strict mode. Not implemented yet.
func UnmarshalStrict(data []byte, v any) error { return ErrNotImplemented }
