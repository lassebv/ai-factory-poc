package peer

import (
	"context"
	"io"
	"math/rand/v2"
	"net"
)

// STUB: placeholders so the tree compiles before the implementation exists.
// The implementer deletes this file and provides real versions (see the
// list in contract.go).

const errStub = "peer: not implemented"

func writeHandshake(io.Writer, Handshake) error { panic(errStub) }
func readHandshake(io.Reader) (Handshake, error) { panic(errStub) }
func exchangeHandshake(context.Context, net.Conn, Handshake) (Handshake, error) {
	panic(errStub)
}
func readMessage(io.Reader, int) (Message, error) { panic(errStub) }
func writeMessage(io.Writer, Message) error      { panic(errStub) }
func newConn(net.Conn, Handshake, Config) Conn    { panic(errStub) }
func newPeerID(*rand.Rand) [20]byte               { panic(errStub) }
