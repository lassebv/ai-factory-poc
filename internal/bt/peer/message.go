// Package peer implements the BitTorrent peer wire protocol (BEP 3): the
// handshake, the core messages and a connection with separate read and
// write goroutines (§4.5).
package peer

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// Message IDs (BEP 3).
const (
	MsgChoke         byte = 0
	MsgUnchoke       byte = 1
	MsgInterested    byte = 2
	MsgNotInterested byte = 3
	MsgHave          byte = 4
	MsgBitfield      byte = 5
	MsgRequest       byte = 6
	MsgPiece         byte = 7
	MsgCancel        byte = 8
	MsgPort          byte = 9
)

// MaxBlock is the largest block a peer may send or request.
const MaxBlock = 16 * 1024

// MaxMessageLen is the largest message (length prefix excluded) other than
// a bitfield: a piece message with a 16 KiB block (ID, index, begin and the
// block), which is 16 KiB + 13 bytes on the wire with its prefix (§11).
const MaxMessageLen = MaxBlock + 9

// MaxBitfieldLen is the largest bitfield payload ever accepted (T6: at most
// 2^21 pieces).
const MaxBitfieldLen = 1 << 18

// ErrTooLarge is returned when a peer sends an oversized message.
var ErrTooLarge = errors.New("peer: message too large")

// ErrMalformed is returned for a message whose payload doesn't fit its ID.
var ErrMalformed = errors.New("peer: malformed message")

// Message is one peer wire message. A nil *Message is a keep-alive.
type Message struct {
	ID byte
	// Index is the piece index (have, request, piece, cancel).
	Index uint32
	// Begin is the byte offset in the piece (request, piece, cancel).
	Begin uint32
	// Length is the block length (request, cancel).
	Length uint32
	// Payload is the bitfield, the block of a piece message, or the raw
	// payload of messages this package doesn't interpret.
	Payload []byte
	// Port is the DHT port of a port message.
	Port uint16
}

// Marshal returns the wire encoding of m, including the length prefix. A
// nil m encodes a keep-alive.
func (m *Message) Marshal() []byte {
	if m == nil {
		return make([]byte, 4)
	}
	var body []byte
	switch m.ID {
	case MsgHave:
		body = binary.BigEndian.AppendUint32(nil, m.Index)
	case MsgRequest, MsgCancel:
		body = binary.BigEndian.AppendUint32(nil, m.Index)
		body = binary.BigEndian.AppendUint32(body, m.Begin)
		body = binary.BigEndian.AppendUint32(body, m.Length)
	case MsgPiece:
		body = binary.BigEndian.AppendUint32(nil, m.Index)
		body = binary.BigEndian.AppendUint32(body, m.Begin)
		body = append(body, m.Payload...)
	case MsgPort:
		body = binary.BigEndian.AppendUint16(nil, m.Port)
	default:
		body = m.Payload
	}
	out := make([]byte, 5, 5+len(body))
	binary.BigEndian.PutUint32(out, uint32(1+len(body)))
	out[4] = m.ID
	return append(out, body...)
}

// ReadMessage reads one message. maxBitfield bounds a bitfield payload; any
// other message longer than MaxMessageLen is ErrTooLarge. It returns nil for
// a keep-alive. Messages with unknown IDs are returned with their raw
// payload so the caller can ignore them.
func ReadMessage(r io.Reader, maxBitfield int) (*Message, error) {
	var hdr [5]byte
	if _, err := io.ReadFull(r, hdr[:4]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(hdr[:4])
	if n == 0 {
		return nil, nil
	}
	if _, err := io.ReadFull(r, hdr[4:5]); err != nil {
		return nil, unexpected(err)
	}
	id := hdr[4]
	limit := uint32(MaxMessageLen)
	if id == MsgBitfield {
		limit = uint32(min(maxBitfield, MaxBitfieldLen)) + 1
	}
	if n > limit {
		return nil, fmt.Errorf("%w: id %d, %d bytes", ErrTooLarge, id, n)
	}
	payload := make([]byte, n-1)
	if _, err := io.ReadFull(r, payload); err != nil {
		return nil, unexpected(err)
	}
	return parse(id, payload)
}

func unexpected(err error) error {
	if err == io.EOF {
		return io.ErrUnexpectedEOF
	}
	return err
}

func parse(id byte, p []byte) (*Message, error) {
	m := &Message{ID: id}
	bad := func() (*Message, error) {
		return nil, fmt.Errorf("%w: id %d with %d-byte payload", ErrMalformed, id, len(p))
	}
	switch id {
	case MsgChoke, MsgUnchoke, MsgInterested, MsgNotInterested:
		if len(p) != 0 {
			return bad()
		}
	case MsgHave:
		if len(p) != 4 {
			return bad()
		}
		m.Index = binary.BigEndian.Uint32(p)
	case MsgRequest, MsgCancel:
		if len(p) != 12 {
			return bad()
		}
		m.Index = binary.BigEndian.Uint32(p)
		m.Begin = binary.BigEndian.Uint32(p[4:])
		m.Length = binary.BigEndian.Uint32(p[8:])
	case MsgPiece:
		if len(p) < 8 {
			return bad()
		}
		m.Index = binary.BigEndian.Uint32(p)
		m.Begin = binary.BigEndian.Uint32(p[4:])
		m.Payload = p[8:]
	case MsgPort:
		if len(p) != 2 {
			return bad()
		}
		m.Port = binary.BigEndian.Uint16(p)
	default:
		m.Payload = p
	}
	return m, nil
}
