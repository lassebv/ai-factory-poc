package session

// FROZEN CONTRACT TESTS for milestone 4 (ADR-0016..0019). Owned by the
// architect (the *contract_test.go suffix is frozen by the write policy).
// They reuse the fake tracker and seeder of contract_test.go.

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"torrentd/internal/bt/btest"
	"torrentd/internal/store"
)

// ---- M4 (ADR-0016..0019) ----

func startWith(t *testing.T, cfg Config) Session {
	t.Helper()
	if cfg.Logger == nil {
		cfg.Logger = quiet
	}
	if cfg.Rand == nil {
		cfg.Rand = rand.New(rand.NewPCG(3, 4))
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close(context.Background()) })
	return s
}

// writeData puts the complete data of tt under dir.
func writeData(t *testing.T, tt *testTorrent, dir string) {
	t.Helper()
	for _, f := range tt.mi.Info.Files {
		if f.Pad {
			continue
		}
		p := filepath.Join(append([]string{dir}, f.Path...)...)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, tt.flat[f.Offset:f.Offset+f.Length], 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func readFull(c net.Conn) (byte, []byte, error) {
	var lb [4]byte
	for {
		if _, err := io.ReadFull(c, lb[:]); err != nil {
			return 0, nil, err
		}
		if l := binary.BigEndian.Uint32(lb[:]); l > 0 {
			if l > 1<<20 {
				return 0, nil, errors.New("message too large")
			}
			m := make([]byte, l)
			if _, err := io.ReadFull(c, m); err != nil {
				return 0, nil, err
			}
			return m[0], m[1:], nil
		}
	}
}

// leech downloads tt from 127.0.0.1:port over the raw wire and returns the
// flat data. It fails if it is not unchoked within 15 s.
func leech(port int, tt *testTorrent) ([]byte, error) {
	c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 5*time.Second)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(30 * time.Second))
	hs := append([]byte{19}, "BitTorrent protocol"...)
	hs = append(hs, make([]byte, 8)...)
	hs = append(hs, tt.mi.InfoHash[:]...)
	hs = append(hs, "-LE0001-000000000000"...)
	if _, err := c.Write(hs); err != nil {
		return nil, err
	}
	var rh [68]byte
	if _, err := io.ReadFull(c, rh[:]); err != nil {
		return nil, fmt.Errorf("no handshake reply: %w", err)
	}
	c.Write(msg(2, nil)) // interested
	unchokeBy := time.Now().Add(15 * time.Second)
	for {
		c.SetReadDeadline(unchokeBy)
		id, _, err := readFull(c)
		if err != nil {
			return nil, fmt.Errorf("waiting for unchoke: %w", err)
		}
		if id == 1 {
			break
		}
	}
	c.SetDeadline(time.Now().Add(30 * time.Second))
	out := make([]byte, len(tt.flat))
	n := tt.mi.Info.NumPieces()
	var reqs [][3]uint32
	for i := 0; i < n; i++ {
		pl := len(tt.piece(i))
		for b := 0; b < pl; b += 16384 {
			reqs = append(reqs, [3]uint32{uint32(i), uint32(b), uint32(min(16384, pl-b))})
		}
	}
	go func() {
		for _, r := range reqs {
			p := binary.BigEndian.AppendUint32(nil, r[0])
			p = binary.BigEndian.AppendUint32(p, r[1])
			p = binary.BigEndian.AppendUint32(p, r[2])
			if _, err := c.Write(msg(6, p)); err != nil {
				return
			}
		}
	}()
	for got := 0; got < len(reqs); {
		id, m, err := readFull(c)
		if err != nil {
			return nil, fmt.Errorf("after %d/%d blocks: %w", got, len(reqs), err)
		}
		if id != 7 || len(m) < 8 {
			continue
		}
		idx, begin := binary.BigEndian.Uint32(m), binary.BigEndian.Uint32(m[4:])
		copy(out[int64(idx)*int64(tt.pieceLen)+int64(begin):], m[8:])
		got++
	}
	return out, nil
}

func seedingTorrent(t *testing.T, tr *fakeTracker, name string) (*testTorrent, string) {
	tt := makeTorrent(t, name, 32768, tr.url(), tfile{[]string{"a.bin"}, 100000}, tfile{[]string{"b.bin"}, 70000})
	dir := t.TempDir()
	writeData(t, tt, dir)
	return tt, dir
}

func TestContractSessionSeedFromDisk(t *testing.T) {
	btest.CheckGoroutines(t)
	tr := newTracker(t)
	tt, dir := seedingTorrent(t, tr, "seed")
	s := startWith(t, Config{Store: btest.NewMemStore()})
	if _, err := s.Add(context.Background(), tt.mi, AddOptions{DownloadDir: dir}); err != nil {
		t.Fatal(err)
	}
	st := waitState(t, s, tt.mi.InfoHash, StateSeeding, 15*time.Second)
	if st.CompletedBytes != st.SizeBytes || st.CompletedAt.IsZero() || st.DownloadedTotal != 0 {
		t.Fatalf("seeding from disk: %+v", st)
	}
	tr.waitEvent(t, "started", 5*time.Second)
	got, err := leech(s.PeerPort(), tt)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, tt.flat) {
		t.Fatal("leecher received wrong data")
	}
	deadline := time.Now().Add(5 * time.Second)
	for st, _ = s.Get(tt.mi.InfoHash); st.UploadedTotal < int64(len(tt.flat)) && time.Now().Before(deadline); st, _ = s.Get(tt.mi.InfoHash) {
		time.Sleep(20 * time.Millisecond)
	}
	if st.UploadedTotal < int64(len(tt.flat)) {
		t.Fatalf("UploadedTotal = %d after serving %d bytes", st.UploadedTotal, len(tt.flat))
	}
	if err := s.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	tr.waitEvent(t, "stopped", time.Second)
}

func TestContractSessionLifecycleActions(t *testing.T) {
	btest.CheckGoroutines(t)
	ctx := context.Background()
	tr := newTracker(t)
	tt, dir := seedingTorrent(t, tr, "life")
	s := startWith(t, Config{Store: btest.NewMemStore()})
	var unknown [20]byte
	for name, f := range map[string]func(context.Context, [20]byte) error{"pause": s.Pause, "resume": s.Resume, "stop": s.StopSeeding} {
		if err := f(ctx, unknown); !errors.Is(err, ErrNotFound) {
			t.Errorf("%s on unknown id = %v, want ErrNotFound", name, err)
		}
	}
	s.Add(ctx, tt.mi, AddOptions{DownloadDir: dir})
	id := tt.mi.InfoHash
	waitState(t, s, id, StateSeeding, 15*time.Second)
	if err := s.Pause(ctx, id); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("pause on a complete torrent = %v, want ErrInvalidState", err)
	}
	if err := s.Resume(ctx, id); err != nil {
		t.Fatalf("resume on an active torrent = %v, want nil (no-op)", err)
	}
	if err := s.StopSeeding(ctx, id); err != nil {
		t.Fatal(err)
	}
	st, _ := s.Get(id)
	if st.State != StateStopped || st.StopReason != StopReasonUser || st.CompletedBytes != st.SizeBytes {
		t.Fatalf("after StopSeeding: state %s reason %q", st.State, st.StopReason)
	}
	if err := s.StopSeeding(ctx, id); err != nil {
		t.Fatalf("StopSeeding on stopped = %v, want nil", err)
	}
	tr.waitEvent(t, "stopped", 4*time.Second)
	if _, err := leech(s.PeerPort(), tt); err == nil {
		t.Fatal("a stopped torrent served a leecher")
	}
	if err := s.Resume(ctx, id); err != nil {
		t.Fatal(err)
	}
	if st, _ := s.Get(id); st.State != StateSeeding || st.StopReason != StopReasonNone {
		t.Fatalf("after Resume: %s %q", st.State, st.StopReason)
	}
	if got, err := leech(s.PeerPort(), tt); err != nil || !bytes.Equal(got, tt.flat) {
		t.Fatalf("leech after resume seeding: %v", err)
	}

	// An incomplete torrent (no peers): pause/resume, not stop-seeding.
	inc := makeTorrent(t, "inc", 32768, tr.url(), tfile{[]string{"x.bin"}, 50000})
	s.Add(ctx, inc.mi, AddOptions{DownloadDir: t.TempDir()})
	waitState(t, s, inc.mi.InfoHash, StateDownloading, 10*time.Second)
	if err := s.StopSeeding(ctx, inc.mi.InfoHash); !errors.Is(err, ErrNotComplete) {
		t.Fatalf("stop-seeding on incomplete = %v, want ErrNotComplete", err)
	}
	if err := s.Pause(ctx, inc.mi.InfoHash); err != nil {
		t.Fatal(err)
	}
	if st, _ := s.Get(inc.mi.InfoHash); st.State != StatePaused {
		t.Fatalf("after Pause: %s", st.State)
	}
	if err := s.Pause(ctx, inc.mi.InfoHash); err != nil {
		t.Fatalf("pause on paused = %v, want nil", err)
	}
	if err := s.Resume(ctx, inc.mi.InfoHash); err != nil {
		t.Fatal(err)
	}
	if st, _ := s.Get(inc.mi.InfoHash); st.State != StateDownloading {
		t.Fatalf("after Resume: %s", st.State)
	}
}

func TestContractSessionPersistence(t *testing.T) {
	btest.CheckGoroutines(t)
	ctx := context.Background()
	tr := newTracker(t)
	ms := btest.NewMemStore()
	a, dirA := seedingTorrent(t, tr, "persist-a")
	b := makeTorrent(t, "persist-b", 32768, tr.url(), tfile{[]string{"y.bin"}, 50000})
	s, err := New(Config{Logger: quiet, Store: ms, PreviousClean: true})
	if err != nil {
		t.Fatal(err)
	}
	s.Add(ctx, a.mi, AddOptions{DownloadDir: dirA})
	s.Add(ctx, b.mi, AddOptions{DownloadDir: t.TempDir(), StartPaused: true})
	waitState(t, s, a.mi.InfoHash, StateSeeding, 15*time.Second)
	waitState(t, s, b.mi.InfoHash, StatePaused, 10*time.Second)
	if _, ok := ms.Torrent(b.mi.InfoHash); !ok {
		t.Fatal("Add must persist the torrent before returning")
	}
	if err := s.StopSeeding(ctx, a.mi.InfoHash); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(ctx); err != nil {
		t.Fatal(err)
	}
	ra, _ := ms.Torrent(a.mi.InfoHash)
	rb, _ := ms.Torrent(b.mi.InfoHash)
	if ra.DesiredState != store.DesiredStopped || ra.StopReason != "user" || len(ra.Bitfield) == 0 || ra.ResumeFingerprint == nil || ra.CompletedAt.IsZero() {
		t.Fatalf("record A after Close: %+v", ra)
	}
	if rb.DesiredState != store.DesiredPaused || !bytes.Equal(ra.RawInfo, a.mi.RawInfo) || len(ra.Trackers) == 0 {
		t.Fatalf("record B %+v / A raw info or trackers missing", rb)
	}

	// Change one byte of A behind our back, keeping size and mtime: a clean
	// restart trusts the bitfield (RES-02).
	p := filepath.Join(append([]string{dirA}, a.mi.Info.Files[0].Path...)...)
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(p)
	data[10] ^= 0xff
	os.WriteFile(p, data, 0o644)
	os.Chtimes(p, fi.ModTime(), fi.ModTime())

	s2 := startWith(t, Config{Store: ms, PreviousClean: true})
	if len(s2.List()) != 2 {
		t.Fatalf("after restart %d torrents listed, want 2", len(s2.List()))
	}
	sa := waitState(t, s2, a.mi.InfoHash, StateStopped, 15*time.Second)
	if sa.StopReason != StopReasonUser || sa.CompletedBytes != sa.SizeBytes || sa.UploadedTotal != ra.UploadedTotal {
		t.Fatalf("restored A: %+v", sa)
	}
	waitState(t, s2, b.mi.InfoHash, StatePaused, 10*time.Second)
	if st, _ := s2.Get(a.mi.InfoHash); st.CompletedBytes != st.SizeBytes {
		t.Fatal("fast resume rechecked although size and mtime were unchanged")
	}
}

func TestContractSessionFilesMissing(t *testing.T) {
	btest.CheckGoroutines(t)
	ctx := context.Background()
	tr := newTracker(t)
	ms := btest.NewMemStore()
	tt, dir := seedingTorrent(t, tr, "missing")
	s, _ := New(Config{Logger: quiet, Store: ms, PreviousClean: true})
	s.Add(ctx, tt.mi, AddOptions{DownloadDir: dir})
	waitState(t, s, tt.mi.InfoHash, StateSeeding, 15*time.Second)
	s.Close(ctx)
	os.RemoveAll(dir)
	s2 := startWith(t, Config{Store: ms, PreviousClean: true})
	deadline := time.Now().Add(10 * time.Second)
	var st Status
	for st, _ = s2.Get(tt.mi.InfoHash); st.State != StateError && time.Now().Before(deadline); st, _ = s2.Get(tt.mi.InfoHash) {
		time.Sleep(20 * time.Millisecond)
	}
	if st.State != StateError || st.ErrorMessage != ErrorFilesMissing {
		t.Fatalf("all files missing: state %s error %q", st.State, st.ErrorMessage)
	}
	if _, err := os.Stat(dir); err == nil {
		t.Fatal("files or the download dir were recreated for a Files missing torrent")
	}
}

func TestContractSessionBansCorruptPeer(t *testing.T) {
	ln6, err := net.Listen("tcp", "[::1]:0")
	if err != nil {
		t.Skip("no IPv6 loopback")
	}
	ln6.Close()
	btest.CheckGoroutines(t)
	tr := newTracker(t)
	tt := makeTorrent(t, "ban", 32768, tr.url(), tfile{[]string{"z.bin"}, 32768 * 24})
	bad := newSeeder(t, tt)
	bad.mu.Lock()
	bad.corruptAll = true
	bad.mu.Unlock()
	good := newSeederAt(t, "[::1]:0", tt)
	tr.mu.Lock()
	tr.dict = []string{bad.ln.Addr().String(), good.ln.Addr().String()}
	tr.mu.Unlock()
	ms := btest.NewMemStore()
	s := startWith(t, Config{Store: ms})
	s.Add(context.Background(), tt.mi, AddOptions{DownloadDir: t.TempDir()})
	st := waitState(t, s, tt.mi.InfoHash, StateSeeding, 30*time.Second)
	if st.WastedTotal == 0 {
		t.Log("the corrupt peer was never used")
	}
	bad.mu.Lock()
	conns := bad.conns
	bad.mu.Unlock()
	if conns > 2 {
		t.Errorf("reconnected %d times to a peer whose pieces always fail", conns)
	}
	if st.WastedTotal > 0 {
		found := false
		for _, b := range ms.Bans() {
			found = found || b.IP == "127.0.0.1"
			if b.IP == "::1" {
				t.Error("the good peer was banned")
			}
		}
		if !found {
			t.Errorf("no ban stored for the corrupt peer: %+v", ms.Bans())
		}
	}
}

func TestContractSessionStalledPeerDoesNotHang(t *testing.T) {
	btest.CheckGoroutines(t)
	tr := newTracker(t)
	tt := makeTorrent(t, "stall", 32768, tr.url(), tfile{[]string{"s.bin"}, 32768 * 16})
	staller := newSeeder(t, tt)
	staller.mu.Lock()
	staller.stall = true
	staller.mu.Unlock()
	good := newSeeder(t, tt)
	tr.setPeers(staller.port(), good.port())
	s := startWith(t, Config{})
	s.Add(context.Background(), tt.mi, AddOptions{DownloadDir: t.TempDir()})
	waitState(t, s, tt.mi.InfoHash, StateSeeding, 30*time.Second)
	staller.mu.Lock()
	cancels := staller.cancels
	staller.mu.Unlock()
	if cancels == 0 {
		t.Error("no cancel sent to the peer holding blocks that arrived from another peer")
	}
}
