package peer

// M4 placeholder so the tree builds; the implementer replaces this file.

import (
	"context"
	"net"
)

func acceptHandshake(context.Context, net.Conn, func([20]byte) (Handshake, bool)) (Handshake, error) {
	return Handshake{}, ErrUnknownTorrent
}
