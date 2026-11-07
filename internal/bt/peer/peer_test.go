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

// FuzzDecodeMessage: arbitrary bytes never panic, and anything that decodes
// re-encodes to the same frame (WIRE-06 arrives at M4).
func FuzzDecodeMessage(f *testing.F) {
	for _, m := range []Message{
		{KeepAlive: true}, {ID: MsgChoke}, {ID: MsgHave, Index: 9},
		{ID: MsgBitfield, Bitfield: []byte{0xff}}, {ID: MsgRequest, Index: 1, Begin: 2, Length: 3},
		{ID: MsgPiece, Index: 1, Begin: 2, Block: []byte("xy")}, {ID: MsgPort, Port: 1},
		{ID: 42, Payload: []byte{1, 2}},
	} {
		var buf bytes.Buffer
		if err := writeMessage(&buf, m); err != nil {
			f.Fatal(err)
		}
		f.Add(buf.Bytes(), 4)
	}
	f.Add([]byte{0xff, 0xff, 0xff, 0xff, 5}, 0)
	f.Fuzz(func(t *testing.T, b []byte, max int) {
		r := bytes.NewReader(b)
		m, err := readMessage(r, max)
		if err != nil {
			return
		}
		consumed := len(b) - r.Len()
		var out bytes.Buffer
		if err := writeMessage(&out, m); err != nil {
			t.Fatalf("re-encode %+v: %v", m, err)
		}
		if !bytes.Equal(out.Bytes(), b[:consumed]) {
			t.Fatalf("re-encode mismatch: %x vs %x", out.Bytes(), b[:consumed])
		}
	})
}

func TestReadMessageTruncatedHeaderAndID(t *testing.T) {
	if _, err := readMessage(bytes.NewReader([]byte{0, 0}), 0); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("partial length: %v", err)
	}
	if _, err := readMessage(bytes.NewReader([]byte{0, 0, 0, 1}), 0); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("missing id: %v", err)
	}
	if _, err := readMessage(bytes.NewReader([]byte{0, 0, 0, 5, 4, 0}), 0); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("short have: %v", err)
	}
	// An over-global maxBitfieldLen is clamped.
	if _, err := readMessage(&headerOnly{frame(byte(MsgBitfield), nil)[:4]}, 0); err == nil {
		t.Error("headerOnly without id must fail")
	}
	b := make([]byte, 5)
	binary.BigEndian.PutUint32(b, 1+MaxBitfieldLen+1)
	b[4] = byte(MsgBitfield)
	if _, err := readMessage(&headerOnly{b}, MaxBitfieldLen*4); !errors.Is(err, ErrMessageTooLarge) {
		t.Errorf("clamped bitfield: %v", err)
	}
}

func TestReadMessageEmptyPayloads(t *testing.T) {
	m, err := readMessage(bytes.NewReader(frame(byte(MsgBitfield), nil)), 0)
	if err != nil || m.Bitfield == nil || len(m.Bitfield) != 0 {
		t.Errorf("empty bitfield: %+v %v", m, err)
	}
	m, err = readMessage(bytes.NewReader(frame(77, nil)), 0)
	if err != nil || m.ID != 77 || m.Payload == nil {
		t.Errorf("empty unknown: %+v %v", m, err)
	}
	m, err = readMessage(bytes.NewReader(frame(byte(MsgPiece), make([]byte, 8))), 0)
	if err != nil || len(m.Block) != 0 {
		t.Errorf("empty piece block: %+v %v", m, err)
	}
}

type failWriter struct{ after int }

func (w *failWriter) Write(p []byte) (int, error) {
	if w.after <= 0 {
		return 0, errors.New("boom")
	}
	w.after--
	return len(p), nil
}

func TestWriteMessageErrors(t *testing.T) {
	for _, m := range []Message{{KeepAlive: true}, {ID: MsgHave}, {ID: MsgCancel}, {ID: MsgPort}} {
		if err := writeMessage(&failWriter{}, m); err == nil {
			t.Errorf("%+v: no error", m)
		}
	}
	if err := writeMessage(&failWriter{after: 1}, Message{ID: MsgPiece, Block: []byte("x")}); err == nil {
		t.Error("block write error lost")
	}
	if err := writeHandshake(&failWriter{}, Handshake{}); err == nil {
		t.Error("handshake write error lost")
	}
}

func TestReadHandshakeEOF(t *testing.T) {
	if _, err := readHandshake(bytes.NewReader(nil)); !errors.Is(err, io.EOF) {
		t.Errorf("empty: %v", err)
	}
	if _, err := readHandshake(bytes.NewReader([]byte{19, 'B'})); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("short: %v", err)
	}
}

func TestExchangeHandshakeErrors(t *testing.T) {
	ours := Handshake{InfoHash: ih(1)}

	// Already-cancelled context: fails, and the deadline is cleared again.
	local, remote := tcpPair(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := exchangeHandshake(ctx, local, ours); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled ctx: %v", err)
	}
	go remote.Write([]byte{1})
	local.SetReadDeadline(time.Time{})
	var b [1]byte
	if _, err := local.Read(b[:]); err != nil {
		t.Errorf("deadline left behind after cancel: %v", err)
	}

	// Write fails on a closed conn.
	local2, _ := tcpPair(t)
	local2.Close()
	if _, err := exchangeHandshake(context.Background(), local2, ours); err == nil {
		t.Error("closed conn: no error")
	}

	// Remote answers with garbage.
	local3, remote3 := tcpPair(t)
	go func() {
		readHandshake(remote3)
		remote3.Write(bytes.Repeat([]byte{7}, HandshakeLen))
	}()
	if _, err := exchangeHandshake(context.Background(), local3, ours); !errors.Is(err, ErrInvalidHandshake) {
		t.Errorf("garbage reply: %v", err)
	}
}

func TestConnWriteErrorCloses(t *testing.T) {
	local, remote := tcpPair(t)
	rec := newRecorder()
	c := newConn(local, Handshake{}, rec.cfg(nil))
	remote.(*net.TCPConn).SetLinger(0)
	remote.Close()
	// Keep sending until a write fails or the read side notices the close.
	for i := 0; i < 1000 && c.Send(Message{ID: MsgInterested}) == nil; i++ {
		time.Sleep(time.Millisecond)
	}
	waitDone(t, c)
	if c.Err() == nil {
		t.Fatal("Err = nil after remote reset")
	}
	if err := <-rec.errc; err == nil {
		t.Fatal("OnClose(nil)")
	}
}

func TestConnErrNilBeforeDone(t *testing.T) {
	local, _ := tcpPair(t)
	block := make(chan struct{})
	c := newConn(local, Handshake{}, Config{OnMessage: func(Message) {}, OnClose: func(error) { <-block }})
	if c.Err() != nil {
		t.Fatal("Err before Done")
	}
	c.Close()
	waitDone(t, c)
	close(block)
}

func TestConnKeepAliveSkippedWhenBusy(t *testing.T) {
	fake := clock.NewFake(time.Unix(1_000_000, 0))
	local, remote := tcpPair(t)
	c := newConn(local, Handshake{}, Config{Clock: fake, OnMessage: func(Message) {}})
	defer c.Close()
	// A write a minute in pushes the keep-alive back: at 2m nothing is due.
	fake.Advance(time.Minute)
	if err := c.Send(Message{ID: MsgInterested}); err != nil {
		t.Fatal(err)
	}
	if m := readMsg(t, remote); m.ID != MsgInterested {
		t.Fatalf("got %+v", m)
	}
	// Keep the idle timer happy.
	WriteMessage(remote, Message{KeepAlive: true})
	fake.Advance(KeepAliveInterval - time.Minute + time.Second)
	remote.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	var b [1]byte
	if n, _ := remote.Read(b[:]); n != 0 {
		t.Fatal("keep-alive sent although a message was written recently")
	}
	fake.Advance(time.Minute)
	if m := readMsg(t, remote); !m.KeepAlive {
		t.Fatalf("got %+v, want keep-alive", m)
	}
}

func TestEnqueueKeepAliveClosed(t *testing.T) {
	local, _ := tcpPair(t)
	c := newConn(local, Handshake{}, Config{OnMessage: func(Message) {}}).(*conn)
	c.Close()
	c.enqueueKeepAlive()
	waitDone(t, c)
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.queue) != 0 {
		t.Fatal("keep-alive queued on a closed conn")
	}
}
