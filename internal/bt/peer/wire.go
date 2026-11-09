package peer

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"net"
	"time"
)

// writeHandshake encodes h as a 68-byte BEP 3 handshake in a single Write.
func writeHandshake(w io.Writer, h Handshake) error {
	var b [HandshakeLen]byte
	b[0] = byte(len(Protocol))
	n := 1 + copy(b[1:], Protocol)
	n += copy(b[n:], h.Reserved[:])
	n += copy(b[n:], h.InfoHash[:])
	copy(b[n:], h.PeerID[:])
	_, err := w.Write(b[:])
	return err
}

// readHandshake reads exactly HandshakeLen bytes. The protocol-length byte
// is checked first so a peer speaking something else is rejected without
// waiting for the rest.
func readHandshake(r io.Reader) (Handshake, error) {
	var h Handshake
	var b [HandshakeLen]byte
	if _, err := io.ReadFull(r, b[:1]); err != nil {
		return h, fmt.Errorf("peer: reading handshake: %w", err)
	}
	if int(b[0]) != len(Protocol) {
		return h, fmt.Errorf("%w: protocol string length %d", ErrInvalidHandshake, b[0])
	}
	if _, err := io.ReadFull(r, b[1:]); err != nil {
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		return h, fmt.Errorf("peer: reading handshake: %w", err)
	}
	if string(b[1:1+len(Protocol)]) != Protocol {
		return h, fmt.Errorf("%w: protocol string %q", ErrInvalidHandshake, b[1:1+len(Protocol)])
	}
	n := 1 + len(Protocol)
	n += copy(h.Reserved[:], b[n:])
	n += copy(h.InfoHash[:], b[n:])
	copy(h.PeerID[:], b[n:])
	return h, nil
}

// exchangeHandshake writes ours, reads the remote handshake and checks the
// info-hash. Cancellation is implemented by setting an already-expired
// deadline on nc from a context.AfterFunc; on return the AfterFunc is either
// stopped or waited for (and the deadline cleared), so no goroutine or stray
// deadline is left behind.
func exchangeHandshake(ctx context.Context, nc net.Conn, ours Handshake) (Handshake, error) {
	fired := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		defer close(fired)
		_ = nc.SetDeadline(time.Unix(1, 0)) // in the past: unblocks I/O now
	})
	theirs, err := doExchange(nc, ours)
	if !stop() {
		<-fired
		_ = nc.SetDeadline(time.Time{})
		if err == nil {
			err = errors.New("peer: handshake aborted")
		}
		return Handshake{}, fmt.Errorf("peer: handshake: %w (%w)", ctx.Err(), err)
	}
	return theirs, err
}

func acceptHandshake(ctx context.Context, nc net.Conn, lookup func(infoHash [20]byte) (Handshake, bool)) (Handshake, error) {
	fired := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		defer close(fired)
		_ = nc.SetDeadline(time.Unix(1, 0))
	})
	theirs, err := doAccept(nc, lookup)
	if !stop() {
		<-fired
		_ = nc.SetDeadline(time.Time{})
		if err == nil {
			err = errors.New("peer: handshake aborted")
		}
		return Handshake{}, fmt.Errorf("peer: handshake: %w (%w)", ctx.Err(), err)
	}
	return theirs, err
}

func doAccept(nc net.Conn, lookup func(infoHash [20]byte) (Handshake, bool)) (Handshake, error) {
	theirs, err := readHandshake(nc)
	if err != nil {
		return Handshake{}, err
	}
	ours, ok := lookup(theirs.InfoHash)
	if !ok {
		return Handshake{}, fmt.Errorf("%w: %x", ErrUnknownTorrent, theirs.InfoHash)
	}
	if err := writeHandshake(nc, ours); err != nil {
		return Handshake{}, fmt.Errorf("peer: writing handshake: %w", err)
	}
	return theirs, nil
}

func doExchange(nc net.Conn, ours Handshake) (Handshake, error) {
	if err := writeHandshake(nc, ours); err != nil {
		return Handshake{}, fmt.Errorf("peer: writing handshake: %w", err)
	}
	theirs, err := readHandshake(nc)
	if err != nil {
		return Handshake{}, err
	}
	if theirs.InfoHash != ours.InfoHash {
		return Handshake{}, fmt.Errorf("%w: got %x", ErrInfoHashMismatch, theirs.InfoHash)
	}
	return theirs, nil
}

// payloadLen returns the exact payload size of a fixed-size message, or -1
// for messages whose size is variable (bitfield, piece, unknown ids).
func payloadLen(id MessageID) int {
	switch id {
	case MsgChoke, MsgUnchoke, MsgInterested, MsgNotInterested:
		return 0
	case MsgHave:
		return 4
	case MsgRequest, MsgCancel:
		return 12
	case MsgPort:
		return 2
	}
	return -1
}

// readMessage reads one message. The length prefix and id byte are read and
// validated before the payload is allocated or read.
func readMessage(r io.Reader, maxBitfieldLen int) (Message, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return Message{}, err // io.EOF on a clean boundary, else ErrUnexpectedEOF
	}
	length := binary.BigEndian.Uint32(hdr[:])
	if length == 0 {
		return Message{KeepAlive: true}, nil
	}
	var idb [1]byte
	if _, err := io.ReadFull(r, idb[:]); err != nil {
		return Message{}, unexpected(err)
	}
	id := MessageID(idb[0])
	plen := uint64(length) - 1
	if id == MsgBitfield {
		if maxBitfieldLen <= 0 || maxBitfieldLen > MaxBitfieldLen {
			maxBitfieldLen = MaxBitfieldLen
		}
		if plen > uint64(maxBitfieldLen) {
			return Message{}, fmt.Errorf("%w: bitfield of %d bytes (max %d)", ErrMessageTooLarge, plen, maxBitfieldLen)
		}
	} else if length > MaxMessageLen {
		return Message{}, fmt.Errorf("%w: id %d, length %d", ErrMessageTooLarge, id, length)
	}
	if want := payloadLen(id); want >= 0 && plen != uint64(want) {
		return Message{}, fmt.Errorf("%w: id %d with %d-byte payload, want %d", ErrInvalidMessage, id, plen, want)
	}
	if id == MsgPiece && plen < 8 {
		return Message{}, fmt.Errorf("%w: piece with %d-byte payload", ErrInvalidMessage, plen)
	}
	var p []byte
	if plen > 0 {
		p = make([]byte, plen)
		if _, err := io.ReadFull(r, p); err != nil {
			return Message{}, unexpected(err)
		}
	}
	return decode(id, p), nil
}

// decode builds a Message from a payload whose size was already validated.
func decode(id MessageID, p []byte) Message {
	m := Message{ID: id}
	be := binary.BigEndian
	switch id {
	case MsgChoke, MsgUnchoke, MsgInterested, MsgNotInterested:
	case MsgHave:
		m.Index = be.Uint32(p)
	case MsgBitfield:
		m.Bitfield = nonNil(p)
	case MsgRequest, MsgCancel:
		m.Index, m.Begin, m.Length = be.Uint32(p), be.Uint32(p[4:]), be.Uint32(p[8:])
	case MsgPiece:
		m.Index, m.Begin = be.Uint32(p), be.Uint32(p[4:])
		m.Block = p[8:]
	case MsgPort:
		m.Port = be.Uint16(p)
	default:
		m.Payload = nonNil(p)
	}
	return m
}

func nonNil(p []byte) []byte {
	if p == nil {
		return []byte{}
	}
	return p
}

// unexpected turns an io.EOF in the middle of a message into
// io.ErrUnexpectedEOF.
func unexpected(err error) error {
	if errors.Is(err, io.EOF) {
		return io.ErrUnexpectedEOF
	}
	return err
}

// errMessageEncode is returned when a message cannot be framed (its payload
// does not fit a 32-bit length prefix).
var errMessageEncode = errors.New("peer: message too large to encode")

// writeMessage frames m. Fixed-size fields go into one header buffer; a
// piece block or variable payload follows in a second Write so it is not
// copied.
func writeMessage(w io.Writer, m Message) error {
	if m.KeepAlive {
		_, err := w.Write([]byte{0, 0, 0, 0})
		return err
	}
	var hdr [4 + 1 + 12]byte
	be := binary.BigEndian
	hdr[4] = byte(m.ID)
	n := 5
	var tail []byte
	switch m.ID {
	case MsgChoke, MsgUnchoke, MsgInterested, MsgNotInterested:
	case MsgHave:
		be.PutUint32(hdr[5:], m.Index)
		n += 4
	case MsgBitfield:
		tail = m.Bitfield
	case MsgRequest, MsgCancel:
		be.PutUint32(hdr[5:], m.Index)
		be.PutUint32(hdr[9:], m.Begin)
		be.PutUint32(hdr[13:], m.Length)
		n += 12
	case MsgPiece:
		be.PutUint32(hdr[5:], m.Index)
		be.PutUint32(hdr[9:], m.Begin)
		n += 8
		tail = m.Block
	case MsgPort:
		be.PutUint16(hdr[5:], m.Port)
		n += 2
	default:
		tail = m.Payload
	}
	total := uint64(n-4) + uint64(len(tail))
	if total > math.MaxUint32 {
		return errMessageEncode
	}
	be.PutUint32(hdr[:], uint32(total))
	if _, err := w.Write(hdr[:n]); err != nil {
		return err
	}
	if len(tail) > 0 {
		if _, err := w.Write(tail); err != nil {
			return err
		}
	}
	return nil
}

// newPeerID returns PeerIDPrefix followed by 12 bytes drawn from rnd.
func newPeerID(rnd *rand.Rand) [20]byte {
	var id [20]byte
	n := copy(id[:], PeerIDPrefix)
	for i := n; i < len(id); i += 4 {
		var b [4]byte
		binary.LittleEndian.PutUint32(b[:], rnd.Uint32())
		copy(id[i:], b[:])
	}
	return id
}
