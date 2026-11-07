package peer

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"torrentd/internal/clock"
)

func TestMessageRoundTrip(t *testing.T) {
	msgs := []*Message{
		{ID: MsgChoke}, {ID: MsgUnchoke}, {ID: MsgInterested}, {ID: MsgNotInterested},
		{ID: MsgHave, Index: 7},
		{ID: MsgBitfield, Payload: []byte{0xf0}},
		{ID: MsgRequest, Index: 1, Begin: 16384, Length: 16384},
		{ID: MsgCancel, Index: 1, Begin: 0, Length: 100},
		{ID: MsgPiece, Index: 2, Begin: 32, Payload: []byte("data")},
		{ID: MsgPort, Port: 6881},
		{ID: 20, Payload: []byte("ext")},
	}
	var buf bytes.Buffer
	buf.Write((*Message)(nil).Marshal())
	for _, m := range msgs {
		buf.Write(m.Marshal())
	}
	if m, err := ReadMessage(&buf, 10); err != nil || m != nil {
		t.Fatalf("keep-alive: %v %v", m, err)
	}
	for _, want := range msgs {
		got, err := ReadMessage(&buf, 10)
		if err != nil {
			t.Fatalf("%d: %v", want.ID, err)
		}
		if got.ID != want.ID || got.Index != want.Index || got.Begin != want.Begin ||
			got.Length != want.Length || got.Port != want.Port || !bytes.Equal(got.Payload, want.Payload) {
			t.Errorf("got %+v, want %+v", got, want)
		}
	}
	if _, err := ReadMessage(&buf, 10); err != io.EOF {
		t.Errorf("at end: %v", err)
	}
}

func frame(n uint32, id byte, payload []byte) []byte {
	b := binary.BigEndian.AppendUint32(nil, n)
	b = append(b, id)
	return append(b, payload...)
}

func TestReadMessageLimits(t *testing.T) {
	// A full 16 KiB piece message is allowed.
	piece := (&Message{ID: MsgPiece, Payload: make([]byte, MaxBlock)}).Marshal()
	if _, err := ReadMessage(bytes.NewReader(piece), 1); err != nil {
		t.Fatalf("16 KiB block: %v", err)
	}
	// One byte more is not.
	big := (&Message{ID: MsgPiece, Payload: make([]byte, MaxBlock+1)}).Marshal()
	if _, err := ReadMessage(bytes.NewReader(big), 1); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("oversize: %v", err)
	}
	// Unknown IDs are limited too.
	if _, err := ReadMessage(bytes.NewReader(frame(MaxMessageLen+200, 99, nil)), 1); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("oversize unknown: %v", err)
	}
	// A bitfield may exceed the limit up to its expected size.
	bf := (&Message{ID: MsgBitfield, Payload: make([]byte, 20000)}).Marshal()
	if _, err := ReadMessage(bytes.NewReader(bf), 20000); err != nil {
		t.Fatalf("big bitfield: %v", err)
	}
	if _, err := ReadMessage(bytes.NewReader(bf), 19999); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("too big bitfield: %v", err)
	}
}

func TestReadMessageMalformed(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   []byte
		want error
	}{
		{"choke with payload", frame(2, MsgChoke, []byte{1}), ErrMalformed},
		{"short have", frame(3, MsgHave, []byte{1, 2}), ErrMalformed},
		{"short request", frame(5, MsgRequest, []byte{1, 2, 3, 4}), ErrMalformed},
		{"short piece", frame(5, MsgPiece, []byte{1, 2, 3, 4}), ErrMalformed},
		{"short port", frame(2, MsgPort, []byte{1}), ErrMalformed},
		{"truncated payload", frame(10, MsgPiece, []byte{1}), io.ErrUnexpectedEOF},
		{"truncated id", []byte{0, 0, 0, 5}, io.ErrUnexpectedEOF},
		{"truncated length", []byte{0, 0}, io.ErrUnexpectedEOF},
	} {
		if _, err := ReadMessage(bytes.NewReader(tc.in), 1); !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", tc.name, err, tc.want)
		}
	}
}

func TestHandshakeEncoding(t *testing.T) {
	h := Handshake{InfoHash: [20]byte{1}, PeerID: [20]byte{2}, Reserved: [8]byte{0, 0, 0, 0, 0, 0x10}}
	b := h.Marshal()
	if len(b) != HandshakeLen || b[0] != 19 || string(b[1:20]) != Protocol {
		t.Fatalf("Marshal = %x", b)
	}
	got, err := ReadHandshake(bytes.NewReader(b))
	if err != nil || got != h {
		t.Fatalf("ReadHandshake = %+v, %v", got, err)
	}
	bad := append([]byte(nil), b...)
	bad[0] = 18
	if _, err := ReadHandshake(bytes.NewReader(bad)); err != ErrBadHandshake {
		t.Errorf("pstrlen: %v", err)
	}
	bad[0] = 19
	bad[1] = 'b'
	if _, err := ReadHandshake(bytes.NewReader(bad)); err != ErrBadHandshake {
		t.Errorf("pstr: %v", err)
	}
	if _, err := ReadHandshake(bytes.NewReader(b[:30])); err != io.ErrUnexpectedEOF {
		t.Errorf("short: %v", err)
	}
	if _, err := ReadHandshake(bytes.NewReader(nil)); err != io.EOF {
		t.Errorf("empty: %v", err)
	}
}

// remote runs fn as the other side of one connection accepted on a
// loopback listener and returns the listener's address.
func remote(t *testing.T, fn func(net.Conn)) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		fn(c)
	}()
	return ln.Addr().String()
}

var ih = [20]byte{9, 9, 9}

func answer(c net.Conn, infoHash [20]byte) (Handshake, error) {
	h, err := ReadHandshake(c)
	if err != nil {
		return h, err
	}
	_, err = c.Write(Handshake{InfoHash: infoHash, PeerID: [20]byte{'x'}}.Marshal())
	return h, err
}

func TestDialExchangesMessages(t *testing.T) {
	got := make(chan Handshake, 1)
	addr := remote(t, func(c net.Conn) {
		h, err := answer(c, ih)
		if err != nil {
			return
		}
		got <- h
		c.Write((&Message{ID: MsgUnchoke}).Marshal())
		c.Write((*Message)(nil).Marshal())
		c.Write((&Message{ID: MsgHave, Index: 3}).Marshal())
		m, err := ReadMessage(c, 1)
		if err == nil && m.ID == MsgInterested {
			c.Write((&Message{ID: MsgChoke}).Marshal())
		}
		io.Copy(io.Discard, c)
	})
	conn, err := Dial(context.Background(), addr, Config{InfoHash: ih, PeerID: [20]byte{'m', 'e'}})
	if err != nil {
		t.Fatal(err)
	}
	if h := <-got; h.InfoHash != ih || h.PeerID[0] != 'm' {
		t.Fatalf("remote saw %+v", h)
	}
	if conn.Remote().PeerID[0] != 'x' || conn.RemoteAddr() == nil || conn.String() == "" {
		t.Fatal("Remote/RemoteAddr/String")
	}
	recv := make(chan *Message, 10)
	done := make(chan error, 1)
	conn.Start(context.Background(), func(m *Message) { recv <- m }, func(err error) { done <- err })
	if m := <-recv; m.ID != MsgUnchoke {
		t.Fatalf("first message %d", m.ID)
	}
	if m := <-recv; m.ID != MsgHave || m.Index != 3 {
		t.Fatalf("second message %+v", m)
	}
	if !conn.Send(&Message{ID: MsgInterested}) {
		t.Fatal("Send failed")
	}
	if m := <-recv; m.ID != MsgChoke {
		t.Fatalf("reply %+v", m)
	}
	if conn.Err() != nil {
		t.Fatal("Err while open")
	}
	conn.Close()
	if err := <-done; err != ErrClosed {
		t.Fatalf("done = %v", err)
	}
	if conn.Send(&Message{ID: MsgInterested}) {
		t.Fatal("Send after Close succeeded")
	}
}

func TestDialErrors(t *testing.T) {
	wrong := remote(t, func(c net.Conn) { answer(c, [20]byte{1}) })
	if _, err := Dial(context.Background(), wrong, Config{InfoHash: ih}); err != ErrInfoHash {
		t.Errorf("wrong info-hash: %v", err)
	}
	garbage := remote(t, func(c net.Conn) {
		ReadHandshake(c)
		c.Write([]byte("HTTP/1.1 400 Bad Request\r\n\r\n" + string(make([]byte, 60))))
	})
	if _, err := Dial(context.Background(), garbage, Config{InfoHash: ih}); err != ErrBadHandshake {
		t.Errorf("garbage handshake: %v", err)
	}
	silent := remote(t, func(c net.Conn) { io.Copy(io.Discard, c) })
	if _, err := Dial(context.Background(), silent, Config{InfoHash: ih, HandshakeTimeout: 100 * time.Millisecond}); err == nil {
		t.Error("silent peer: no error")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Dial(ctx, silent, Config{InfoHash: ih}); err == nil {
		t.Error("cancelled context: no error")
	}
}

func TestOversizeClosesConn(t *testing.T) {
	addr := remote(t, func(c net.Conn) {
		answer(c, ih)
		c.Write(frame(MaxMessageLen+200, 99, make([]byte, MaxMessageLen+199)))
		io.Copy(io.Discard, c)
	})
	conn, err := Dial(context.Background(), addr, Config{InfoHash: ih})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	conn.Start(context.Background(), func(*Message) { t.Error("message delivered") }, func(err error) { done <- err })
	if err := <-done; !errors.Is(err, ErrTooLarge) {
		t.Fatalf("done = %v", err)
	}
}

func TestKeepAliveAndCancel(t *testing.T) {
	fake := clock.NewFake(clock.Real().Now())
	keepalive := make(chan bool, 1)
	addr := remote(t, func(c net.Conn) {
		answer(c, ih)
		m, err := ReadMessage(c, 1)
		keepalive <- err == nil && m == nil
		io.Copy(io.Discard, c)
	})
	conn, err := Dial(context.Background(), addr, Config{InfoHash: ih, Clock: fake, KeepAlive: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	conn.Start(ctx, func(*Message) {}, func(err error) { done <- err })
	for fake.Waiters() == 0 {
		<-clock.Real().After(time.Millisecond)
	}
	fake.Advance(time.Minute)
	if !<-keepalive {
		t.Fatal("no keep-alive")
	}
	cancel()
	if err := <-done; err != context.Canceled {
		t.Fatalf("done = %v", err)
	}
}

func TestSendQueueFull(t *testing.T) {
	block := make(chan struct{})
	addr := remote(t, func(c net.Conn) {
		answer(c, ih)
		<-block
	})
	defer close(block)
	conn, err := Dial(context.Background(), addr, Config{InfoHash: ih})
	if err != nil {
		t.Fatal(err)
	}
	// Without Start nothing drains the queue.
	for i := 0; i < sendQueueLen; i++ {
		if !conn.Send(&Message{ID: MsgInterested}) {
			t.Fatalf("Send %d failed", i)
		}
	}
	if conn.Send(&Message{ID: MsgInterested}) || conn.Err() != ErrSendQueueFull {
		t.Fatalf("overflow: err %v", conn.Err())
	}
}
