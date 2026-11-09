// Package peer implements the BitTorrent peer wire protocol (BEP 3): the
// handshake, the core messages and a connection with separate read and
// write goroutines (§4.1 Peer wire, §4.5, §11 robustness, D13).
//
// FROZEN CONTRACT (ADR-0011, ADR-0012, ADR-0017). This file is owned by the architect.
// The implementer provides, in their own files (replacing stub.go):
//
//	func writeHandshake(w io.Writer, h Handshake) error
//	func readHandshake(r io.Reader) (Handshake, error)
//	func exchangeHandshake(ctx context.Context, nc net.Conn, ours Handshake) (Handshake, error)
//	func acceptHandshake(ctx context.Context, nc net.Conn, lookup func(infoHash [20]byte) (Handshake, bool)) (Handshake, error)
//	func readMessage(r io.Reader, maxBitfieldLen int) (Message, error)
//	func writeMessage(w io.Writer, m Message) error
//	func newConn(nc net.Conn, remote Handshake, cfg Config) Conn
//	func newPeerID(rnd *rand.Rand) [20]byte
//
// Out of scope at M3: the fast extension (BEP 6), the extension protocol
// (BEP 10, 11), MSE and uTP. Reserved bytes we send are all zero.
//
// # Robustness
//
// No input from the network may cause a panic, an unbounded allocation or a
// hang. Every length is checked before the payload is allocated.
package peer

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"math/rand/v2"
	"net"
	"time"

	"torrentd/internal/clock"
)

// Protocol is the BEP 3 protocol string.
const Protocol = "BitTorrent protocol"

// HandshakeLen is the length of a BEP 3 handshake on the wire.
const HandshakeLen = 1 + len(Protocol) + 8 + 20 + 20

// PeerIDPrefix is the Azureus-style client prefix of our peer ID (D13).
const PeerIDPrefix = "-TD0100-"

// MaxMessageLen is the largest allowed length prefix (message id + payload)
// for every message except bitfield: a piece message carrying a 16 KiB block
// is 9+16384 bytes; anything above 16 KiB + 13 bytes is a protocol violation
// and closes the connection (WIRE-02).
const MaxMessageLen = 16<<10 + 13

// MaxBitfieldLen is the largest bitfield payload ever accepted: 2^21 pieces
// (§16.2 T6) / 8 = 256 KiB.
const MaxBitfieldLen = (1 << 21) / 8

// Timing (by the Config clock).
const (
	// KeepAliveInterval: the write goroutine sends a keep-alive when nothing
	// has been written for this long.
	KeepAliveInterval = 2 * time.Minute
	// IdleTimeout: the connection is closed with ErrIdleTimeout when nothing
	// (not even a keep-alive) has been received for this long. The idle
	// deadline is pushed back when a message is read, before it is handed
	// to OnMessage.
	IdleTimeout = 3 * time.Minute
)

// SendQueueLimit is the maximum number of queued outgoing messages. A Send
// that would exceed it closes the connection with ErrSendQueueFull.
const SendQueueLimit = 1024

// MessageID is the BEP 3 message id byte.
type MessageID uint8

// Core message ids (BEP 3).
const (
	MsgChoke         MessageID = 0 // no payload
	MsgUnchoke       MessageID = 1 // no payload
	MsgInterested    MessageID = 2 // no payload
	MsgNotInterested MessageID = 3 // no payload
	MsgHave          MessageID = 4 // Index
	MsgBitfield      MessageID = 5 // Bitfield
	MsgRequest       MessageID = 6 // Index, Begin, Length
	MsgPiece         MessageID = 7 // Index, Begin, Block
	MsgCancel        MessageID = 8 // Index, Begin, Length
	MsgPort          MessageID = 9 // Port (BEP 5)
)

// Errors. Wrapped errors are fine; callers use errors.Is.
var (
	// ErrMessageTooLarge: a length prefix above MaxMessageLen (or, for a
	// bitfield, above 1+maxBitfieldLen).
	ErrMessageTooLarge = errors.New("peer: message too large")
	// ErrInvalidMessage: a known message id with a payload of the wrong
	// size (e.g. have with 3 bytes, piece with fewer than 8, choke with 1).
	ErrInvalidMessage = errors.New("peer: invalid message")
	// ErrInvalidHandshake: wrong protocol string length or text.
	ErrInvalidHandshake = errors.New("peer: invalid handshake")
	// ErrInfoHashMismatch: the remote handshake names another torrent.
	ErrInfoHashMismatch = errors.New("peer: info-hash mismatch")
	// ErrIdleTimeout: nothing received for IdleTimeout.
	ErrIdleTimeout = errors.New("peer: idle timeout")
	// ErrSendQueueFull: more than SendQueueLimit messages queued.
	ErrSendQueueFull = errors.New("peer: send queue full")
	// ErrUnknownTorrent: an incoming handshake names a torrent the lookup
	// refused (unknown, paused or stopped).
	ErrUnknownTorrent = errors.New("peer: unknown torrent")
	// ErrClosed: Send on a closed Conn.
	ErrClosed = errors.New("peer: connection closed")
)

// Handshake is the BEP 3 handshake after the protocol string.
type Handshake struct {
	// Reserved holds the 8 extension bits; zero at M3.
	Reserved [8]byte
	// InfoHash is the torrent's v1 info-hash.
	InfoHash [20]byte
	// PeerID is the sender's peer ID.
	PeerID [20]byte
}

// Message is one decoded peer wire message. Only the fields listed for its
// ID (see the Msg* constants) are meaningful; the others are zero.
type Message struct {
	// KeepAlive marks a zero-length keep-alive (ReadMessage only; ID is 0
	// and must be ignored). Conn never delivers keep-alives.
	KeepAlive bool
	// ID is the message id.
	ID MessageID
	// Index is the piece index (have, request, piece, cancel).
	Index uint32
	// Begin is the offset within the piece (request, piece, cancel).
	Begin uint32
	// Length is the requested length (request, cancel).
	Length uint32
	// Bitfield is the raw bitfield payload; its length is not validated
	// against the torrent here (the session does that).
	Bitfield []byte
	// Block is the data of a piece message.
	Block []byte
	// Port is the DHT port (port message).
	Port uint16
	// Payload is the raw payload of a message with an unknown id. Unknown
	// ids within the size limit are not an error (forward compatibility).
	Payload []byte
}

// WriteHandshake writes the 68-byte handshake for h.
func WriteHandshake(w io.Writer, h Handshake) error { return writeHandshake(w, h) }

// ReadHandshake reads a 68-byte handshake. It returns ErrInvalidHandshake if
// the protocol string is not Protocol, and an error wrapping
// io.ErrUnexpectedEOF (or io.EOF) for short input.
func ReadHandshake(r io.Reader) (Handshake, error) { return readHandshake(r) }

// ExchangeHandshake performs the outgoing side of the handshake on nc: it
// writes ours, reads the remote handshake and returns it. It returns
// ErrInfoHashMismatch if the remote info-hash differs from ours.InfoHash.
// When ctx is cancelled or its deadline passes, the exchange is aborted
// promptly with an error (implementations may close nc or set a deadline
// from ctx.Deadline). It does not close nc on success.
func ExchangeHandshake(ctx context.Context, nc net.Conn, ours Handshake) (Handshake, error) {
	return exchangeHandshake(ctx, nc, ours)
}

// AcceptHandshake performs the incoming side of the handshake on nc: it
// reads the remote handshake, calls lookup with its info-hash and, if
// lookup returns ok, writes our handshake (lookup's result) and returns the
// remote one. If lookup refuses, nothing is written and ErrUnknownTorrent is
// returned. ctx bounds the whole exchange like ExchangeHandshake. It does
// not close nc (on success or failure); the caller does.
func AcceptHandshake(ctx context.Context, nc net.Conn, lookup func(infoHash [20]byte) (Handshake, bool)) (Handshake, error) {
	return acceptHandshake(ctx, nc, lookup)
}

// ReadMessage reads one length-prefixed message. maxBitfieldLen bounds a
// bitfield payload (values <= 0 or above MaxBitfieldLen mean
// MaxBitfieldLen). Oversized messages fail with ErrMessageTooLarge before
// any payload is allocated; malformed known messages fail with
// ErrInvalidMessage. Returned slices are freshly allocated.
func ReadMessage(r io.Reader, maxBitfieldLen int) (Message, error) {
	return readMessage(r, maxBitfieldLen)
}

// WriteMessage writes m with its length prefix; a KeepAlive message writes
// four zero bytes.
func WriteMessage(w io.Writer, m Message) error { return writeMessage(w, m) }

// Config configures a Conn.
type Config struct {
	// Clock drives keep-alives and the idle timeout; nil means clock.Real().
	// All timers are created before NewConn returns.
	Clock clock.Clock
	// MaxBitfieldLen bounds an incoming bitfield (see ReadMessage). Callers
	// pass ceil(numPieces/8).
	MaxBitfieldLen int
	// OnMessage receives every incoming message except keep-alives, in wire
	// order, on the read goroutine, never concurrently. It may block (that
	// is the back-pressure) and may call Send or Close. Required.
	OnMessage func(Message)
	// OnClose, if set, is called exactly once, after Done is closed, with
	// the same value as Err. It runs on a Conn goroutine and may call any
	// Conn method.
	OnClose func(error)
	// OnPieceSent, if set, is called on the write goroutine after a piece
	// message has been fully written to nc, with its Block length. The
	// session uses it to count uploadedTotal (bytes actually sent, not
	// queued) and to bound the blocks it keeps queued per peer. It must not
	// block for long (a non-blocking send to the actor).
	OnPieceSent func(index, begin uint32, n int)
	// Logger receives debug logs; nil means slog.Default().
	Logger *slog.Logger
}

// Conn is an established peer connection (after the handshake) with one
// read goroutine and one write goroutine. All methods are safe for
// concurrent use.
//
// The Conn closes itself, and nc, on a read or write error, a protocol
// violation (ErrMessageTooLarge, ErrInvalidMessage), ErrIdleTimeout or
// ErrSendQueueFull. After Done is closed both goroutines have exited and nc
// is closed.
type Conn interface {
	// Send queues m for the write goroutine and never blocks. It returns
	// ErrClosed after the Conn is closed. Messages are written in Send
	// order.
	Send(m Message) error
	// Close closes the connection. It is idempotent, does not wait for the
	// goroutines (use Done), and is safe to call from OnMessage.
	Close() error
	// Done is closed once the Conn is fully shut down.
	Done() <-chan struct{}
	// Err returns why the Conn closed: nil if Close was called first, an
	// error wrapping io.EOF if the remote closed, or one of the errors
	// above. It returns nil before Done is closed.
	Err() error
	// Remote returns the remote handshake passed to NewConn.
	Remote() Handshake
	// RemoteAddr returns the remote network address.
	RemoteAddr() net.Addr
}

// NewConn starts the read and write goroutines on an already-handshaken nc
// and returns immediately. The Conn owns nc from now on.
func NewConn(nc net.Conn, remote Handshake, cfg Config) Conn { return newConn(nc, remote, cfg) }

// NewPeerID returns PeerIDPrefix followed by 12 bytes from rnd (D13).
func NewPeerID(rnd *rand.Rand) [20]byte { return newPeerID(rnd) }
