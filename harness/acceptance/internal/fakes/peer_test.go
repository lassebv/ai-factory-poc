package fakes

import (
	"bytes"
	"encoding/binary"
	"io"
	"net"
	"testing"
	"time"

	"torrentd.local/harness/acceptance/internal/torrentgen"
)

// readMsg reads one message, skipping keep-alives.
func readMsg(t *testing.T, c net.Conn) (byte, []byte) {
	t.Helper()
	for {
		var l [4]byte
		if _, err := io.ReadFull(c, l[:]); err != nil {
			t.Fatal(err)
		}
		n := binary.BigEndian.Uint32(l[:])
		if n == 0 {
			continue
		}
		b := make([]byte, n)
		if _, err := io.ReadFull(c, b); err != nil {
			t.Fatal(err)
		}
		return b[0], b[1:]
	}
}

func request(c net.Conn, index, begin, length uint32) {
	b := make([]byte, 17)
	binary.BigEndian.PutUint32(b, 13)
	b[4] = 6
	binary.BigEndian.PutUint32(b[5:], index)
	binary.BigEndian.PutUint32(b[9:], begin)
	binary.BigEndian.PutUint32(b[13:], length)
	c.Write(b)
}

// TestStallHoldsOnlyStalledPieces: a request for a stalled piece must not
// block requests for other pieces, whatever order the client asks in.
func TestStallHoldsOnlyStalledPieces(t *testing.T) {
	tor, err := torrentgen.Generate(t.TempDir(), torrentgen.Spec{Name: "s.bin", PieceLength: 16384, Seed: 1,
		Announce: "http://x/announce", Files: []torrentgen.File{{Size: 6 * 16384}}})
	if err != nil {
		t.Fatal(err)
	}
	p, err := NewPeer(tor, PeerOptions{StallFrom: 4})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	c, err := net.Dial("tcp", p.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	var hs bytes.Buffer
	hs.WriteByte(19)
	hs.WriteString("BitTorrent protocol")
	hs.Write(make([]byte, 8))
	hs.Write(tor.InfoHash[:])
	hs.WriteString("-TD0100-000000000000")
	c.Write(hs.Bytes())
	io.ReadFull(c, make([]byte, 68))
	if id, _ := readMsg(t, c); id != 5 {
		t.Fatalf("want bitfield, got %d", id)
	}
	c.Write([]byte{0, 0, 0, 1, 2}) // interested
	if id, _ := readMsg(t, c); id != 1 {
		t.Fatalf("want unchoke, got %d", id)
	}
	request(c, 5, 0, 16384) // stalled
	request(c, 0, 0, 16384) // must still be served
	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	id, payload := readMsg(t, c)
	if id != 7 || binary.BigEndian.Uint32(payload) != 0 {
		t.Fatalf("piece 0 was not served while piece 5 was stalled (got id %d)", id)
	}
	p.Release()
	id, payload = readMsg(t, c)
	if id != 7 || binary.BigEndian.Uint32(payload) != 5 {
		t.Fatalf("piece 5 was not served after Release (got id %d)", id)
	}
}
