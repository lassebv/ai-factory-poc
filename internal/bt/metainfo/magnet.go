// Package metainfo will hold .torrent and magnet parsing (§4.1, §16.1).
//
// Milestone 1 placeholder: only the pinned magnet API exists, so the tree
// type-checks. ParseMagnet returns ErrNotImplemented until milestone 2.
package metainfo

import "errors"

// ErrNotImplemented is returned by ParseMagnet until milestone 2.
var ErrNotImplemented = errors.New("metainfo: not implemented (milestone 2)")

// Magnet is a parsed BEP 9 magnet URI.
type Magnet struct {
	InfoHash    [20]byte // from xt=urn:btih:
	DisplayName string   // dn, URL-decoded; "" if absent
	Trackers    []string // every tr, URL-decoded, in order, duplicates removed
	Peers       []string // every x.pe as "host:port", in order
}

// ParseMagnet parses a magnet URI. Not implemented yet.
func ParseMagnet(uri string) (*Magnet, error) { return nil, ErrNotImplemented }
