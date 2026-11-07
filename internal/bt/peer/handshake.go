package peer

import (
	"bytes"
	"errors"
	"io"
)

// Protocol is the BEP 3 protocol string.
const Protocol = "BitTorrent protocol"

// HandshakeLen is the size of a BEP 3 handshake.
const HandshakeLen = 1 + len(Protocol) + 8 + 20 + 20

// ErrBadHandshake is returned for a handshake with the wrong protocol string.
var ErrBadHandshake = errors.New("peer: bad handshake")

// Handshake is the BEP 3 handshake.
type Handshake struct {
	Reserved [8]byte
	InfoHash [20]byte
	PeerID   [20]byte
}

// Marshal returns the wire encoding of h.
func (h Handshake) Marshal() []byte {
	b := make([]byte, 0, HandshakeLen)
	b = append(b, byte(len(Protocol)))
	b = append(b, Protocol...)
	b = append(b, h.Reserved[:]...)
	b = append(b, h.InfoHash[:]...)
	return append(b, h.PeerID[:]...)
}

// ReadHandshake reads and validates the protocol string of a handshake.
func ReadHandshake(r io.Reader) (Handshake, error) {
	var b [HandshakeLen]byte
	var h Handshake
	if _, err := io.ReadFull(r, b[:1]); err != nil {
		return h, err
	}
	if int(b[0]) != len(Protocol) {
		return h, ErrBadHandshake
	}
	if _, err := io.ReadFull(r, b[1:]); err != nil {
		return h, unexpected(err)
	}
	if !bytes.Equal(b[1:20], []byte(Protocol)) {
		return h, ErrBadHandshake
	}
	copy(h.Reserved[:], b[20:28])
	copy(h.InfoHash[:], b[28:48])
	copy(h.PeerID[:], b[48:68])
	return h, nil
}
