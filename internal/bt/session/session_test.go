package session

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"testing"
	"time"

	"torrentd/internal/bt/btest"
	"torrentd/internal/clock"
)

// rawPeer handshakes with the session on port for tt and returns the conn.
func rawPeer(t *testing.T, port int, tt *testTorrent) net.Conn {
	t.Helper()
	c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	c.SetDeadline(time.Now().Add(20 * time.Second))
	hs := append([]byte{19}, "BitTorrent protocol"...)
	hs = append(hs, make([]byte, 8)...)
	hs = append(hs, tt.mi.InfoHash[:]...)
	hs = append(hs, "-UT0001-000000000000"...)
	c.Write(hs)
	var rh [68]byte
	if _, err := io.ReadFull(c, rh[:]); err != nil {
		t.Fatalf("handshake: %v", err)
	}
	return c
}

func req(id byte, idx, begin, n uint32) []byte {
	p := binary.BigEndian.AppendUint32(nil, idx)
	p = binary.BigEndian.AppendUint32(p, begin)
	p = binary.BigEndian.AppendUint32(p, n)
	return msg(id, p)
}

// A seeding torrent ignores invalid requests, honours cancel and closes
// the connection on an oversized request (WIRE-03).
func TestSeedingProtocolEdges(t *testing.T) {
	btest.CheckGoroutines(t)
	tr := newTracker(t)
	tt, dir := seedingTorrent(t, tr, "edges")
	s := startWith(t, Config{})
	s.Add(context.Background(), tt.mi, AddOptions{DownloadDir: dir})
	waitState(t, s, tt.mi.InfoHash, StateSeeding, 15*time.Second)

	c := rawPeer(t, s.PeerPort(), tt)
	c.Write(msg(4, binary.BigEndian.AppendUint32(nil, 0))) // have 0
	c.Write(req(6, 0, 0, 16384))                           // choked: ignored
	c.Write(msg(2, nil))                                   // interested
	for {
		id, _, err := readFull(c)
		if err != nil {
			t.Fatal(err)
		}
		if id == 1 {
			break
		}
	}
	c.Write(msg(3, nil)) // not interested
	c.Write(msg(2, nil))
	c.Write(req(6, 999, 0, 16384))      // no such piece: ignored
	c.Write(req(6, 0, 32760, 100))      // beyond the piece: ignored
	c.Write(req(6, 1, 0, 16384))        // queued...
	c.Write(req(8, 1, 0, 16384))        // ...and maybe cancelled
	c.Write(msg(0, nil))                // choke (ignored by a seed)
	c.Write(msg(1, nil))                // unchoke
	c.Write(msg(5, []byte{0xff}))       // bad bitfield length: closes
	time.Sleep(1200 * time.Millisecond) // let a tick run
	for {
		if _, _, err := readFull(c); err != nil {
			break
		}
	}

	c2 := rawPeer(t, s.PeerPort(), tt)
	c2.Write(msg(2, nil))
	c2.Write(req(6, 0, 0, 32768)) // oversized
	c2.SetDeadline(time.Now().Add(5 * time.Second))
	for {
		if _, _, err := readFull(c2); err != nil {
			if errors.Is(err, os.ErrDeadlineExceeded) {
				t.Fatal("an oversized request did not close the connection")
			}
			break
		}
	}
	if st, _ := s.Get(tt.mi.InfoHash); st.State != StateSeeding {
		t.Fatalf("state %s", st.State)
	}
}

func TestActionsOnErrorAndClosed(t *testing.T) {
	btest.CheckGoroutines(t)
	ctx := context.Background()
	tr := newTracker(t)
	ms := btest.NewMemStore()
	tt, dir := seedingTorrent(t, tr, "err")
	s := startWith(t, Config{Store: ms, PreviousClean: true})
	s.Add(ctx, tt.mi, AddOptions{DownloadDir: dir})
	waitState(t, s, tt.mi.InfoHash, StateSeeding, 15*time.Second)
	s.Close(ctx)
	if err := s.Pause(ctx, tt.mi.InfoHash); !errors.Is(err, ErrClosed) {
		t.Fatalf("pause after close = %v", err)
	}
	if _, err := s.Add(ctx, tt.mi, AddOptions{DownloadDir: dir}); !errors.Is(err, ErrClosed) {
		t.Fatalf("add after close = %v", err)
	}
	os.RemoveAll(dir)
	s2 := startWith(t, Config{Store: ms, PreviousClean: false})
	id := tt.mi.InfoHash
	waitState(t, s2, id, StateError, 10*time.Second)
	if err := s2.Pause(ctx, id); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("pause in error = %v", err)
	}
	if err := s2.StopSeeding(ctx, id); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("stop-seeding in error = %v", err)
	}
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if err := s2.Resume(cctx, id); err == nil {
		t.Fatal("a cancelled context must abort the action")
	}
	writeData(t, tt, dir)
	if err := s2.Resume(ctx, id); err != nil {
		t.Fatal(err)
	}
	waitState(t, s2, id, StateSeeding, 15*time.Second)
}

func TestIsSelfAndBans(t *testing.T) {
	s := &session{log: quiet, port: 4242, banned: map[netip.Addr]time.Time{}, strikes: map[netip.Addr]int{}}
	tor := &torrent{s: s}
	for _, tc := range []struct {
		a    string
		want bool
	}{{"127.0.0.1:4242", true}, {"0.0.0.0:4242", true}, {"127.0.0.1:1", false}, {"192.0.2.1:4242", false}} {
		if got := tor.isSelf(netip.MustParseAddrPort(tc.a)); got != tc.want {
			t.Errorf("isSelf(%s) = %v", tc.a, got)
		}
	}
	s.clk = clock.Real()
	ip := netip.MustParseAddr("::ffff:10.0.0.1")
	for i := 1; i < BanStrikes; i++ {
		if s.strike(ip) {
			t.Fatal("banned too early")
		}
	}
	if !s.strike(ip) || !s.isBanned(netip.MustParseAddr("10.0.0.1")) {
		t.Fatal("not banned after BanStrikes strikes")
	}
	s.banned[ip.Unmap()] = time.Now().Add(-time.Second)
	if s.isBanned(ip) || s.strike(netip.Addr{}) {
		t.Fatal("expired ban or invalid IP")
	}
}
