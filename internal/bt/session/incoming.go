package session

import (
	"errors"
	"net"
	"net/netip"
	"time"

	"torrentd/internal/bt/peer"
)

// handshakeTimeout bounds an incoming handshake.
const handshakeTimeout = 10 * time.Second

// acceptLoop accepts incoming peer connections; each handshake runs in
// its own goroutine, bounded by handshakeTimeout.
func (s *session) acceptLoop() {
	defer s.wg.Done()
	for {
		c, err := s.ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			s.log.Debug("accept failed", "err", err)
			return
		}
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			_ = c.Close()
			continue
		}
		s.incoming[c] = struct{}{}
		s.wg.Add(1)
		s.mu.Unlock()
		go s.handshake(c)
	}
}

// remoteAddr returns c's remote address, unmapped.
func remoteAddr(c net.Conn) netip.AddrPort {
	if ta, ok := c.RemoteAddr().(*net.TCPAddr); ok {
		a := ta.AddrPort()
		return netip.AddrPortFrom(a.Addr().Unmap(), a.Port())
	}
	return netip.AddrPort{}
}

// handshake answers an incoming handshake for an active torrent and hands
// the connection to its actor.
func (s *session) handshake(c net.Conn) {
	defer s.wg.Done()
	handed := false
	defer func() {
		s.mu.Lock()
		delete(s.incoming, c)
		s.mu.Unlock()
		if !handed {
			_ = c.Close()
		}
	}()
	addr := remoteAddr(c)
	if !addr.IsValid() || s.banned(addr.Addr()) {
		return
	}
	_ = c.SetDeadline(s.clk.Now().Add(handshakeTimeout))
	hs, err := peer.ReadHandshake(c)
	if err != nil || hs.PeerID == s.peerID {
		return
	}
	t := s.lookup(hs.InfoHash)
	if t == nil || !t.accepting.Load() {
		return
	}
	if err := peer.WriteHandshake(c, peer.Handshake{InfoHash: hs.InfoHash, PeerID: s.peerID}); err != nil {
		return
	}
	_ = c.SetDeadline(time.Time{})
	handed = t.send(evIncoming{addr: addr, nc: c, hs: hs})
}
