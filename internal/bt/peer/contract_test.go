package peer

// FROZEN CONTRACT TESTS (ADR-0011). Owned by the architect. Every correct
// implementation of contract.go must pass these.

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"math/rand/v2"
	"net"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"torrentd/internal/clock"
)

func ih(b byte) [20]byte {
	var h [20]byte
	for i := range h {
		h[i] = b
	}
	return h
}

func TestContractHandshakeBytes(t *testing.T) {
	h := Handshake{InfoHash: ih(0xaa), PeerID: ih('p')}
	h.Reserved[7] = 0x04
	var buf bytes.Buffer
	if err := WriteHandshake(&buf, h); err != nil {
		t.Fatal(err)
	}
	b := buf.Bytes()
	if len(b) != HandshakeLen || HandshakeLen != 68 || b[0] != 19 || string(b[1:20]) != Protocol {
		t.Fatalf("handshake bytes = %q", b)
	}
	if b[27] != 0x04 || !bytes.Equal(b[28:48], h.InfoHash[:]) || !bytes.Equal(b[48:68], h.PeerID[:]) {
		t.Fatalf("handshake layout wrong: %x", b)
	}
	got, err := ReadHandshake(bytes.NewReader(b))
	if err != nil || got != h {
		t.Fatalf("ReadHandshake = %+v, %v", got, err)
	}
	bad := bytes.Clone(b)
	bad[5] = 'X'
	if _, err := ReadHandshake(bytes.NewReader(bad)); !errors.Is(err, ErrInvalidHandshake) {
		t.Errorf("wrong pstr: %v, want ErrInvalidHandshake", err)
	}
	bad = bytes.Clone(b)
	bad[0] = 200
	if _, err := ReadHandshake(bytes.NewReader(bad)); !errors.Is(err, ErrInvalidHandshake) {
		t.Errorf("wrong pstrlen: %v, want ErrInvalidHandshake", err)
	}
	if _, err := ReadHandshake(bytes.NewReader(b[:40])); err == nil {
		t.Error("short handshake must fail")
	}
}

func TestContractPeerID(t *testing.T) {
	a := NewPeerID(rand.New(rand.NewPCG(1, 2)))
	b := NewPeerID(rand.New(rand.NewPCG(3, 4)))
	if !strings.HasPrefix(string(a[:]), PeerIDPrefix) || PeerIDPrefix != "-TD0100-" {
		t.Fatalf("peer id %q lacks prefix", a)
	}
	if a == b {
		t.Fatal("peer ids from different random sources should differ")
	}
	if a != NewPeerID(rand.New(rand.NewPCG(1, 2))) {
		t.Fatal("NewPeerID must be deterministic for a given source (T2)")
	}
}

func roundTrip(t *testing.T, m Message) Message {
	t.Helper()
	var buf bytes.Buffer
	if err := WriteMessage(&buf, m); err != nil {
		t.Fatalf("WriteMessage(%+v): %v", m, err)
	}
	got, err := ReadMessage(&buf, 0)
	if err != nil {
		t.Fatalf("ReadMessage(%+v): %v", m, err)
	}
	if buf.Len() != 0 {
		t.Fatalf("%d bytes left after reading %+v", buf.Len(), m)
	}
	return got
}

func TestContractMessageRoundTrip(t *testing.T) {
	block := bytes.Repeat([]byte{7}, 16384)
	cases := []Message{
		{ID: MsgChoke}, {ID: MsgUnchoke}, {ID: MsgInterested}, {ID: MsgNotInterested},
		{ID: MsgHave, Index: 77},
		{ID: MsgBitfield, Bitfield: []byte{0xf0, 0x01}},
		{ID: MsgRequest, Index: 1, Begin: 16384, Length: 16384},
		{ID: MsgPiece, Index: 3, Begin: 32768, Block: block},
		{ID: MsgCancel, Index: 1, Begin: 0, Length: 100},
		{ID: MsgPort, Port: 6881},
		{ID: 20, Payload: []byte("ext")},
	}
	for _, m := range cases {
		got := roundTrip(t, m)
		if got.ID != m.ID || got.Index != m.Index || got.Begin != m.Begin || got.Length != m.Length ||
			got.Port != m.Port || !bytes.Equal(got.Bitfield, m.Bitfield) || !bytes.Equal(got.Block, m.Block) ||
			!bytes.Equal(got.Payload, m.Payload) || got.KeepAlive {
			t.Errorf("round trip of %d: got %+v", m.ID, got)
		}
	}
	if got := roundTrip(t, Message{KeepAlive: true}); !got.KeepAlive {
		t.Error("keep-alive round trip lost KeepAlive")
	}
}

func TestContractMessageWireFormat(t *testing.T) {
	var buf bytes.Buffer
	WriteMessage(&buf, Message{ID: MsgRequest, Index: 1, Begin: 2, Length: 3})
	want := []byte{0, 0, 0, 13, 6, 0, 0, 0, 1, 0, 0, 0, 2, 0, 0, 0, 3}
	if !bytes.Equal(buf.Bytes(), want) {
		t.Fatalf("request = %v, want %v", buf.Bytes(), want)
	}
	buf.Reset()
	WriteMessage(&buf, Message{KeepAlive: true})
	if !bytes.Equal(buf.Bytes(), []byte{0, 0, 0, 0}) {
		t.Fatalf("keep-alive = %v", buf.Bytes())
	}
	buf.Reset()
	WriteMessage(&buf, Message{ID: MsgInterested})
	if !bytes.Equal(buf.Bytes(), []byte{0, 0, 0, 1, 2}) {
		t.Fatalf("interested = %v", buf.Bytes())
	}
}

func frame(id byte, payload []byte) []byte {
	b := make([]byte, 5+len(payload))
	binary.BigEndian.PutUint32(b, uint32(1+len(payload)))
	b[4] = id
	copy(b[5:], payload)
	return b
}

// headerOnly yields a length prefix and id byte, then fails: a reader that
// would make an implementation reading the payload first return an I/O error
// instead of ErrMessageTooLarge.
type headerOnly struct{ b []byte }

func (h *headerOnly) Read(p []byte) (int, error) {
	if len(h.b) == 0 {
		return 0, errors.New("payload must not be read")
	}
	n := copy(p, h.b)
	h.b = h.b[n:]
	return n, nil
}

func TestContractMessageLimits(t *testing.T) {
	hdr := func(l uint32, id byte) io.Reader {
		b := make([]byte, 5)
		binary.BigEndian.PutUint32(b, l)
		b[4] = id
		return &headerOnly{b}
	}
	for _, tc := range []struct {
		name string
		r    io.Reader
		max  int
	}{
		{"unknown id over limit", hdr(MaxMessageLen+1, 99), 0},
		{"piece over limit", hdr(MaxMessageLen+1, byte(MsgPiece)), 0},
		{"huge length", hdr(0xffffffff, byte(MsgHave)), 0},
		{"bitfield over max", hdr(1+11, byte(MsgBitfield)), 10},
		{"bitfield over global max", hdr(1+MaxBitfieldLen+1, byte(MsgBitfield)), 0},
	} {
		if _, err := ReadMessage(tc.r, tc.max); !errors.Is(err, ErrMessageTooLarge) {
			t.Errorf("%s: %v, want ErrMessageTooLarge", tc.name, err)
		}
	}
	// At the limits: accepted.
	if _, err := ReadMessage(bytes.NewReader(frame(byte(MsgBitfield), make([]byte, 10))), 10); err != nil {
		t.Errorf("bitfield of exactly maxBitfieldLen: %v", err)
	}
	if _, err := ReadMessage(bytes.NewReader(frame(99, make([]byte, MaxMessageLen-1))), 0); err != nil {
		t.Errorf("unknown message of exactly MaxMessageLen: %v", err)
	}
	big := frame(byte(MsgBitfield), make([]byte, 5000))
	if _, err := ReadMessage(bytes.NewReader(big), 5000); err != nil {
		t.Errorf("a bitfield larger than MaxMessageLen is allowed when within maxBitfieldLen: %v", err)
	}
}

func TestContractMessageInvalid(t *testing.T) {
	for _, tc := range []struct {
		name string
		b    []byte
	}{
		{"choke with payload", frame(byte(MsgChoke), []byte{1})},
		{"have with 3 bytes", frame(byte(MsgHave), []byte{0, 0, 1})},
		{"request with 11 bytes", frame(byte(MsgRequest), make([]byte, 11))},
		{"cancel with 13 bytes", frame(byte(MsgCancel), make([]byte, 13))},
		{"piece with 5 bytes", frame(byte(MsgPiece), make([]byte, 5))},
		{"port with 3 bytes", frame(byte(MsgPort), make([]byte, 3))},
	} {
		if _, err := ReadMessage(bytes.NewReader(tc.b), 0); !errors.Is(err, ErrInvalidMessage) {
			t.Errorf("%s: %v, want ErrInvalidMessage", tc.name, err)
		}
	}
	if _, err := ReadMessage(bytes.NewReader([]byte{0, 0, 0, 5, 7}), 0); err == nil {
		t.Error("truncated message must fail")
	}
	if _, err := ReadMessage(bytes.NewReader(nil), 0); !errors.Is(err, io.EOF) {
		t.Errorf("empty input = %v, want io.EOF", err)
	}
}

func TestContractGarbageNeverPanics(t *testing.T) {
	rng := rand.New(rand.NewPCG(42, 42))
	for i := 0; i < 20000; i++ {
		n := rng.IntN(64)
		b := make([]byte, n)
		for j := range b {
			b[j] = byte(rng.Uint32())
		}
		if rng.IntN(2) == 0 && n >= 4 { // plausible small length prefix
			binary.BigEndian.PutUint32(b, uint32(rng.IntN(n)))
		}
		r := bytes.NewReader(b)
		for k := 0; k < 8; k++ {
			if _, err := ReadMessage(r, rng.IntN(16)); err != nil {
				break
			}
		}
		ReadHandshake(bytes.NewReader(b))
	}
}

// tcpPair returns a connected loopback TCP pair.
func tcpPair(t *testing.T) (local, remote net.Conn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	ch := make(chan net.Conn, 1)
	go func() { c, _ := ln.Accept(); ch <- c }()
	local, err = net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	remote = <-ch
	t.Cleanup(func() { local.Close(); remote.Close() })
	return local, remote
}

func TestContractExchangeHandshake(t *testing.T) {
	local, remote := tcpPair(t)
	ours := Handshake{InfoHash: ih(1), PeerID: NewPeerID(rand.New(rand.NewPCG(1, 1)))}
	theirs := Handshake{InfoHash: ih(1), PeerID: ih('r')}
	go func() {
		ReadHandshake(remote)
		WriteHandshake(remote, theirs)
	}()
	got, err := ExchangeHandshake(context.Background(), local, ours)
	if err != nil || got != theirs {
		t.Fatalf("ExchangeHandshake = %+v, %v", got, err)
	}

	local2, remote2 := tcpPair(t)
	go func() {
		ReadHandshake(remote2)
		WriteHandshake(remote2, Handshake{InfoHash: ih(2)})
	}()
	if _, err := ExchangeHandshake(context.Background(), local2, ours); !errors.Is(err, ErrInfoHashMismatch) {
		t.Fatalf("mismatched info-hash: %v, want ErrInfoHashMismatch", err)
	}

	local3, _ := tcpPair(t) // the remote never answers
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := ExchangeHandshake(ctx, local3, ours); err == nil {
		t.Fatal("a silent remote must fail when ctx expires")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("ExchangeHandshake took %s after ctx expired", d)
	}
}

type recorder struct {
	mu     sync.Mutex
	msgs   []Message
	closes atomic.Int32
	errc   chan error
	got    chan struct{}
}

func newRecorder() *recorder {
	return &recorder{errc: make(chan error, 4), got: make(chan struct{}, 1024)}
}

func (r *recorder) cfg(c clock.Clock) Config {
	return Config{
		Clock:          c,
		MaxBitfieldLen: 4,
		OnMessage: func(m Message) {
			r.mu.Lock()
			r.msgs = append(r.msgs, m)
			r.mu.Unlock()
			r.got <- struct{}{}
		},
		OnClose: func(err error) { r.closes.Add(1); r.errc <- err },
	}
}

func waitDone(t *testing.T, c Conn) {
	t.Helper()
	select {
	case <-c.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("Conn did not shut down")
	}
}

func readMsg(t *testing.T, c net.Conn) Message {
	t.Helper()
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	m, err := ReadMessage(c, 0)
	if err != nil {
		t.Fatalf("remote read: %v", err)
	}
	return m
}

func TestContractConnDeliversAndSends(t *testing.T) {
	base := runtime.NumGoroutine()
	local, remote := tcpPair(t)
	rec := newRecorder()
	hs := Handshake{InfoHash: ih(3), PeerID: ih('x')}
	c := NewConn(local, hs, rec.cfg(nil))
	if c.Remote() != hs || c.RemoteAddr().String() != local.RemoteAddr().String() {
		t.Fatalf("Remote/RemoteAddr wrong: %+v %v", c.Remote(), c.RemoteAddr())
	}
	WriteMessage(remote, Message{ID: MsgBitfield, Bitfield: []byte{0xff, 0xff, 0xff, 0xf0}})
	WriteMessage(remote, Message{KeepAlive: true})
	WriteMessage(remote, Message{ID: MsgUnchoke})
	WriteMessage(remote, Message{ID: MsgPiece, Index: 1, Begin: 0, Block: []byte("data")})
	for i := 0; i < 3; i++ {
		select {
		case <-rec.got:
		case <-time.After(5 * time.Second):
			t.Fatalf("only %d messages delivered", i)
		}
	}
	rec.mu.Lock()
	if len(rec.msgs) != 3 || rec.msgs[0].ID != MsgBitfield || rec.msgs[1].ID != MsgUnchoke ||
		rec.msgs[2].ID != MsgPiece || string(rec.msgs[2].Block) != "data" {
		t.Fatalf("delivered %+v; want bitfield, unchoke, piece (no keep-alive)", rec.msgs)
	}
	rec.mu.Unlock()

	for i := uint32(0); i < 10; i++ {
		if err := c.Send(Message{ID: MsgRequest, Index: i, Begin: 0, Length: 16384}); err != nil {
			t.Fatal(err)
		}
	}
	for i := uint32(0); i < 10; i++ {
		if m := readMsg(t, remote); m.ID != MsgRequest || m.Index != i {
			t.Fatalf("message %d = %+v, want request %d in Send order", i, m, i)
		}
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	c.Close()
	waitDone(t, c)
	if c.Err() != nil {
		t.Errorf("Err after Close = %v, want nil", c.Err())
	}
	if err := c.Send(Message{ID: MsgInterested}); !errors.Is(err, ErrClosed) {
		t.Errorf("Send after Close = %v, want ErrClosed", err)
	}
	if err := <-rec.errc; err != nil {
		t.Errorf("OnClose(%v), want nil", err)
	}
	remote.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.Copy(io.Discard, remote); err != nil {
		t.Errorf("the local side should be closed: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	if rec.closes.Load() != 1 {
		t.Errorf("OnClose called %d times", rec.closes.Load())
	}
	checkGoroutines(t, base)
}

func checkGoroutines(t *testing.T, base int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for runtime.NumGoroutine() > base+1 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if n := runtime.NumGoroutine(); n > base+1 {
		t.Errorf("goroutine leak: %d running, %d before", n, base)
	}
}

func TestContractConnProtocolViolations(t *testing.T) {
	for _, tc := range []struct {
		name string
		send []byte
		want error
	}{
		{"oversize", func() []byte {
			b := make([]byte, 4+MaxMessageLen+200)
			binary.BigEndian.PutUint32(b, uint32(MaxMessageLen+200))
			b[4] = 99
			return b
		}(), ErrMessageTooLarge},
		{"bitfield over MaxBitfieldLen", frame(byte(MsgBitfield), make([]byte, 5)), ErrMessageTooLarge},
		{"bad have", frame(byte(MsgHave), []byte{1}), ErrInvalidMessage},
	} {
		t.Run(tc.name, func(t *testing.T) {
			local, remote := tcpPair(t)
			rec := newRecorder()
			c := NewConn(local, Handshake{}, rec.cfg(nil))
			go remote.Write(tc.send)
			waitDone(t, c)
			if !errors.Is(c.Err(), tc.want) {
				t.Fatalf("Err = %v, want %v", c.Err(), tc.want)
			}
			if err := <-rec.errc; !errors.Is(err, tc.want) {
				t.Fatalf("OnClose(%v), want %v", err, tc.want)
			}
		})
	}
}

func TestContractConnRemoteClose(t *testing.T) {
	local, remote := tcpPair(t)
	rec := newRecorder()
	c := NewConn(local, Handshake{}, rec.cfg(nil))
	remote.Close()
	waitDone(t, c)
	if !errors.Is(c.Err(), io.EOF) {
		t.Fatalf("Err after remote close = %v, want io.EOF", c.Err())
	}
}

func TestContractConnCloseFromOnMessage(t *testing.T) {
	local, remote := tcpPair(t)
	var c Conn
	ready := make(chan struct{})
	c = NewConn(local, Handshake{}, Config{OnMessage: func(Message) {
		<-ready
		c.Send(Message{ID: MsgInterested})
		c.Close()
	}})
	close(ready)
	WriteMessage(remote, Message{ID: MsgUnchoke})
	waitDone(t, c)
	if c.Err() != nil {
		t.Fatalf("Err = %v, want nil after Close", c.Err())
	}
}

func TestContractConnKeepAliveAndIdle(t *testing.T) {
	fake := clock.NewFake(time.Unix(1_000_000, 0))
	local, remote := tcpPair(t)
	rec := newRecorder()
	c := NewConn(local, Handshake{}, rec.cfg(fake))
	fake.Advance(KeepAliveInterval + time.Second)
	remote.SetReadDeadline(time.Now().Add(5 * time.Second))
	var b [4]byte
	if _, err := io.ReadFull(remote, b[:]); err != nil || b != [4]byte{} {
		t.Fatalf("expected a keep-alive after %s idle, got %v %v", KeepAliveInterval, b, err)
	}
	select {
	case <-c.Done():
		t.Fatal("closed before IdleTimeout")
	case <-time.After(50 * time.Millisecond):
	}
	// Nothing received at all: past IdleTimeout the Conn gives up.
	fake.Advance(IdleTimeout)
	waitDone(t, c)
	if !errors.Is(c.Err(), ErrIdleTimeout) {
		t.Fatalf("Err = %v, want ErrIdleTimeout", c.Err())
	}
}

func TestContractConnIncomingResetsIdle(t *testing.T) {
	fake := clock.NewFake(time.Unix(1_000_000, 0))
	local, remote := tcpPair(t)
	rec := newRecorder()
	c := NewConn(local, Handshake{}, rec.cfg(fake))
	defer c.Close()
	for i := 0; i < 4; i++ {
		fake.Advance(IdleTimeout - 30*time.Second)
		WriteMessage(remote, Message{ID: MsgUnchoke})
		select {
		case <-rec.got:
		case <-time.After(5 * time.Second):
			t.Fatal("message not delivered")
		}
	}
	select {
	case <-c.Done():
		t.Fatalf("closed although messages kept arriving: %v", c.Err())
	case <-time.After(50 * time.Millisecond):
	}
}

func TestContractConnSendQueueLimit(t *testing.T) {
	local, _ := tcpPair(t) // the remote never reads
	c := NewConn(local, Handshake{}, Config{OnMessage: func(Message) {}})
	block := make([]byte, 16384)
	var err error
	for i := 0; i < SendQueueLimit*64 && err == nil; i++ {
		err = c.Send(Message{ID: MsgPiece, Block: block})
	}
	waitDone(t, c)
	if !errors.Is(c.Err(), ErrSendQueueFull) {
		t.Fatalf("Err = %v, want ErrSendQueueFull", c.Err())
	}
}

// ---- M4 (ADR-0017) ----

func TestContractAcceptHandshake(t *testing.T) {
	ours := Handshake{InfoHash: ih(5), PeerID: ih('o')}
	lookup := func(h [20]byte) (Handshake, bool) { return ours, h == ih(5) }

	local, remote := tcpPair(t)
	theirs := Handshake{InfoHash: ih(5), PeerID: ih('t')}
	got := make(chan Handshake, 1)
	go func() {
		WriteHandshake(remote, theirs)
		h, _ := ReadHandshake(remote)
		got <- h
	}()
	h, err := AcceptHandshake(context.Background(), local, lookup)
	if err != nil || h != theirs {
		t.Fatalf("AcceptHandshake = %+v, %v", h, err)
	}
	if r := <-got; r != ours {
		t.Fatalf("remote read our handshake %+v, want %+v", r, ours)
	}

	// Refused torrent: nothing is written back.
	local2, remote2 := tcpPair(t)
	go WriteHandshake(remote2, Handshake{InfoHash: ih(9)})
	if _, err := AcceptHandshake(context.Background(), local2, lookup); !errors.Is(err, ErrUnknownTorrent) {
		t.Fatalf("unknown torrent: %v, want ErrUnknownTorrent", err)
	}
	remote2.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	if n, _ := remote2.Read(make([]byte, 1)); n != 0 {
		t.Fatal("a handshake was written for a refused torrent")
	}

	// A silent remote is bounded by ctx.
	local3, _ := tcpPair(t)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := AcceptHandshake(ctx, local3, lookup); err == nil || time.Since(start) > 2*time.Second {
		t.Fatalf("silent remote: err=%v after %s", err, time.Since(start))
	}
}

func TestContractConnOnPieceSent(t *testing.T) {
	local, remote := tcpPair(t)
	rec := newRecorder()
	cfg := rec.cfg(nil)
	type sent struct {
		index, begin uint32
		n            int
	}
	ch := make(chan sent, 16)
	cfg.OnPieceSent = func(index, begin uint32, n int) { ch <- sent{index, begin, n} }
	c := NewConn(local, Handshake{InfoHash: ih(6)}, cfg)
	defer func() { c.Close(); waitDone(t, c) }()
	c.Send(Message{ID: MsgHave, Index: 1})
	c.Send(Message{ID: MsgPiece, Index: 2, Begin: 16384, Block: make([]byte, 1000)})
	if m := readMsg(t, remote); m.ID != MsgHave {
		t.Fatalf("got %+v", m)
	}
	if m := readMsg(t, remote); m.ID != MsgPiece || len(m.Block) != 1000 {
		t.Fatalf("got %+v", m)
	}
	select {
	case s := <-ch:
		if s != (sent{2, 16384, 1000}) {
			t.Fatalf("OnPieceSent(%+v)", s)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("OnPieceSent not called for a written piece")
	}
	select {
	case s := <-ch:
		t.Fatalf("OnPieceSent called for a non-piece message: %+v", s)
	case <-time.After(50 * time.Millisecond):
	}
}
