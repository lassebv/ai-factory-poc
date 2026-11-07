package tracker

// FROZEN CONTRACT TESTS (ADR-0011). Owned by the architect. Every correct
// implementation of contract.go must pass these.

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"torrentd/internal/clock"
)

// fakeTracker records announces and answers with a scripted body.
type fakeTracker struct {
	srv *httptest.Server
	mu  sync.Mutex
	got []*http.Request
	raw []string
	// reply returns the body for the n-th (0-based) announce.
	reply func(n int, q url.Values) (status int, body string)
}

func newFakeTracker(t *testing.T, reply func(n int, q url.Values) (int, string)) *fakeTracker {
	ft := &fakeTracker{reply: reply}
	ft.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ft.mu.Lock()
		n := len(ft.got)
		ft.got = append(ft.got, r)
		ft.raw = append(ft.raw, r.URL.RawQuery)
		ft.mu.Unlock()
		status, body := ft.reply(n, r.URL.Query())
		w.WriteHeader(status)
		fmt.Fprint(w, body)
	}))
	t.Cleanup(ft.srv.Close)
	return ft
}

func (ft *fakeTracker) url() string { return ft.srv.URL + "/announce" }

func (ft *fakeTracker) count() int {
	ft.mu.Lock()
	defer ft.mu.Unlock()
	return len(ft.got)
}

func (ft *fakeTracker) query(i int) url.Values {
	ft.mu.Lock()
	defer ft.mu.Unlock()
	return ft.got[i].URL.Query()
}

func (ft *fakeTracker) waitCount(t *testing.T, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for ft.count() < n {
		if time.Now().After(deadline) {
			t.Fatalf("tracker got %d announces, want %d", ft.count(), n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func ok(interval int) func(int, url.Values) (int, string) {
	return func(int, url.Values) (int, string) {
		return 200, fmt.Sprintf("d8:intervali%de5:peers6:\x7f\x00\x00\x01\x1a\xe1e", interval)
	}
}

func bstr(s string) string { return fmt.Sprintf("%d:%s", len(s), s) }

var testHash = [20]byte{0x00, ' ', '+', '%', '&', 0xff, 'a', '~', '.', '-', '_', 1, 2, 3, 4, 5, 6, 7, 8, 9}

func baseReq() AnnounceRequest {
	return AnnounceRequest{
		InfoHash: testHash, PeerID: [20]byte([]byte("-TD0100-abcdefghijkl")), Port: 6881,
		Uploaded: 1, Downloaded: 2, Left: 3, Event: EventStarted, Key: "k3y",
	}
}

func TestContractEventString(t *testing.T) {
	for e, want := range map[Event]string{EventNone: "", EventStarted: "started", EventCompleted: "completed", EventStopped: "stopped"} {
		if e.String() != want {
			t.Errorf("Event(%d).String() = %q, want %q", e, e.String(), want)
		}
	}
	if (&FailureError{Reason: "nope"}).Error() == "" {
		t.Error("FailureError.Error must not be empty")
	}
}

func TestContractAnnounceQuery(t *testing.T) {
	ft := newFakeTracker(t, ok(60))
	c := NewHTTPClient(nil)
	req := baseReq()
	req.TrackerID = "tid"
	if _, err := c.Announce(context.Background(), ft.url()+"?passkey=abc", req); err != nil {
		t.Fatal(err)
	}
	q := ft.query(0)
	if q.Get("info_hash") != string(testHash[:]) {
		t.Errorf("info_hash = %x, want %x", q.Get("info_hash"), testHash)
	}
	if strings.Contains(ft.raw[0], "+") {
		t.Errorf("query must percent-encode, never use '+': %s", ft.raw[0])
	}
	want := map[string]string{
		"peer_id": "-TD0100-abcdefghijkl", "port": "6881", "uploaded": "1", "downloaded": "2", "left": "3",
		"compact": "1", "numwant": "50", "key": "k3y", "event": "started", "trackerid": "tid", "passkey": "abc",
	}
	for k, v := range want {
		if q.Get(k) != v {
			t.Errorf("%s = %q, want %q", k, q.Get(k), v)
		}
	}
	req.Event, req.TrackerID = EventNone, ""
	c.Announce(context.Background(), ft.url(), req)
	if q := ft.query(1); q.Has("event") || q.Has("trackerid") {
		t.Errorf("EventNone / empty tracker id must omit event and trackerid: %v", ft.raw[1])
	}
}

func TestContractAnnounceResponse(t *testing.T) {
	compact := "d8:completei5e10:incompletei7e8:intervali1800e12:min intervali900e" +
		"5:peers12:\x0a\x00\x00\x01\x1a\xe1\x0a\x00\x00\x02\x00\x00" +
		"6:peers618:\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x01\x1f\x90" +
		"10:tracker id3:xyz15:warning message4:warne"
	dict := "d8:intervali60e5:peersl" +
		"d2:ip" + bstr("10.0.0.3") + "7:peer id20:-XX0001-000000000000" + "4:porti6882ee" +
		"d2:ip" + bstr("tracker.example") + "4:porti1ee" +
		"d2:ip" + bstr("::1") + "4:porti6883ee" +
		"ee"
	ft := newFakeTracker(t, func(n int, _ url.Values) (int, string) {
		return 200, []string{compact, dict}[n]
	})
	c := NewHTTPClient(nil)
	r, err := c.Announce(context.Background(), ft.url(), baseReq())
	if err != nil {
		t.Fatal(err)
	}
	wantPeers := []netip.AddrPort{netip.MustParseAddrPort("10.0.0.1:6881"), netip.MustParseAddrPort("[::1]:8080")}
	if fmt.Sprint(r.Peers) != fmt.Sprint(wantPeers) {
		t.Errorf("compact peers = %v, want %v (port 0 dropped, peers6 read)", r.Peers, wantPeers)
	}
	if r.Interval != 1800*time.Second || r.MinInterval != 900*time.Second || r.TrackerID != "xyz" ||
		r.Warning != "warn" || r.Seeders != 5 || r.Leechers != 7 {
		t.Errorf("response = %+v", r)
	}
	r, err = c.Announce(context.Background(), ft.url(), baseReq())
	if err != nil {
		t.Fatal(err)
	}
	wantPeers = []netip.AddrPort{netip.MustParseAddrPort("10.0.0.3:6882"), netip.MustParseAddrPort("[::1]:6883")}
	if fmt.Sprint(r.Peers) != fmt.Sprint(wantPeers) {
		t.Errorf("dict peers = %v, want %v (hostnames skipped)", r.Peers, wantPeers)
	}
	if r.Seeders != -1 || r.Leechers != -1 || r.MinInterval != 0 {
		t.Errorf("absent counts must be -1 and absent min interval 0: %+v", r)
	}
}

func TestContractAnnounceErrors(t *testing.T) {
	ft := newFakeTracker(t, func(n int, _ url.Values) (int, string) {
		switch n {
		case 0:
			return 200, "d14:failure reason11:not allowede"
		case 1:
			return 500, "d8:intervali60e5:peers0:e"
		case 2:
			return 200, "<html>nope</html>"
		default:
			return 200, "i42e"
		}
	})
	c := NewHTTPClient(nil)
	_, err := c.Announce(context.Background(), ft.url(), baseReq())
	var fe *FailureError
	if !errors.As(err, &fe) || fe.Reason != "not allowed" {
		t.Fatalf("failure reason: %v, want *FailureError{not allowed}", err)
	}
	for i := 1; i <= 3; i++ {
		if _, err := c.Announce(context.Background(), ft.url(), baseReq()); err == nil {
			t.Errorf("announce %d should fail", i)
		}
	}
	if _, err := c.Announce(context.Background(), "udp://example.com:80/announce", baseReq()); err == nil {
		t.Error("udp:// is not supported at M3 and must return an error")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.Announce(ctx, ft.url(), baseReq()); err == nil {
		t.Error("a cancelled ctx must fail")
	}
}

func TestContractAnnouncerTiers(t *testing.T) {
	bad := newFakeTracker(t, func(int, url.Values) (int, string) { return 200, "d14:failure reason3:bade" })
	good := newFakeTracker(t, func(n int, q url.Values) (int, string) {
		return 200, "d8:intervali60e5:peers0:10:tracker id2:t1e"
	})
	other := newFakeTracker(t, ok(60))
	a := NewAnnouncer([][]string{{bad.url(), good.url()}, {}, {other.url(), ""}}, NewHTTPClient(nil), nil)
	if tiers := a.Tiers(); len(tiers) != 2 || len(tiers[1]) != 1 {
		t.Fatalf("Tiers = %v, want empty tiers and URLs dropped", tiers)
	}
	if _, err := a.Announce(context.Background(), baseReq()); err != nil {
		t.Fatal(err)
	}
	if bad.count() != 1 || good.count() != 1 || other.count() != 0 {
		t.Fatalf("counts bad=%d good=%d other=%d, want 1 1 0", bad.count(), good.count(), other.count())
	}
	if tiers := a.Tiers(); tiers[0][0] != good.url() {
		t.Fatalf("the responding tracker should be promoted: %v", tiers)
	}
	a.Announce(context.Background(), baseReq())
	if bad.count() != 1 || good.count() != 2 {
		t.Fatal("after promotion the good tracker is tried first")
	}
	if good.query(1).Get("trackerid") != "t1" {
		t.Errorf("tracker id not echoed: %v", good.query(1))
	}

	onlyBad := NewAnnouncer([][]string{{bad.url()}, {bad.url() + "?x=1"}}, NewHTTPClient(nil), nil)
	_, err := onlyBad.Announce(context.Background(), baseReq())
	var fe *FailureError
	if !errors.As(err, &fe) {
		t.Fatalf("all trackers failing: %v, want an error wrapping *FailureError", err)
	}
	if _, err := NewAnnouncer(nil, NewHTTPClient(nil), nil).Announce(context.Background(), baseReq()); err == nil {
		t.Error("no trackers must be an error")
	}
	if _, err := onlyBad.Announce(context.Background(), baseReq()); err == nil {
		t.Error("still failing")
	}
}

func TestContractAnnouncerShuffle(t *testing.T) {
	urls := []string{"http://a/1", "http://b/2", "http://c/3", "http://d/4", "http://e/5", "http://f/6"}
	a := NewAnnouncer([][]string{urls}, NewHTTPClient(nil), rand.New(rand.NewPCG(9, 9)))
	got := a.Tiers()[0]
	if len(got) != len(urls) {
		t.Fatalf("shuffled tier = %v", got)
	}
	seen := map[string]bool{}
	for _, u := range got {
		seen[u] = true
	}
	if len(seen) != len(urls) {
		t.Fatalf("shuffle lost URLs: %v", got)
	}
	if fmt.Sprint(got) == fmt.Sprint(urls) {
		t.Error("tier not shuffled (BEP 12)")
	}
	got[0] = "mutated"
	if a.Tiers()[0][0] == "mutated" {
		t.Error("Tiers must return a copy")
	}
}

type loopHarness struct {
	ft    *fakeTracker
	fake  *clock.Fake
	loop  Loop
	peers chan []netip.AddrPort
}

func startTestLoop(t *testing.T, reply func(int, url.Values) (int, string)) *loopHarness {
	h := &loopHarness{ft: newFakeTracker(t, reply), fake: clock.NewFake(time.Unix(1_000_000, 0)), peers: make(chan []netip.AddrPort, 16)}
	h.loop = StartLoop(LoopConfig{
		Announcer: NewAnnouncer([][]string{{h.ft.url()}}, NewHTTPClient(nil), nil),
		InfoHash:  testHash, PeerID: baseReq().PeerID, Port: 7000, Key: "kk",
		Stats:   func() Stats { return Stats{Uploaded: 0, Downloaded: 10, Left: 90} },
		OnPeers: func(p []netip.AddrPort) { h.peers <- p },
		Clock:   h.fake,
	})
	return h
}

// idle waits until the loop holds its single interval/retry timer.
func (h *loopHarness) idle(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for h.fake.Waiters() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the loop never started waiting on the clock")
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func TestContractLoopLifecycle(t *testing.T) {
	base := runtime.NumGoroutine()
	h := startTestLoop(t, func(int, url.Values) (int, string) {
		return 200, "d8:intervali60e12:min intervali30e5:peers6:\x7f\x00\x00\x01\x1a\xe1e"
	})
	h.ft.waitCount(t, 1)
	q := h.ft.query(0)
	if q.Get("event") != "started" || q.Get("left") != "90" || q.Get("downloaded") != "10" || q.Get("port") != "7000" || q.Get("key") != "kk" {
		t.Fatalf("first announce = %v", q)
	}
	select {
	case p := <-h.peers:
		if len(p) != 1 || p[0] != netip.MustParseAddrPort("127.0.0.1:6881") {
			t.Fatalf("OnPeers(%v)", p)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("OnPeers not called")
	}
	h.idle(t)
	h.fake.Advance(59 * time.Second)
	time.Sleep(50 * time.Millisecond)
	if h.ft.count() != 1 {
		t.Fatal("re-announced before the interval")
	}
	h.fake.Advance(time.Second)
	h.ft.waitCount(t, 2)
	if q := h.ft.query(1); q.Has("event") {
		t.Fatalf("regular announce carries event=%q", q.Get("event"))
	}
	h.idle(t)
	h.loop.Completed()
	h.ft.waitCount(t, 3) // no clock advance needed
	if q := h.ft.query(2); q.Get("event") != "completed" {
		t.Fatalf("third announce event=%q, want completed", q.Get("event"))
	}
	h.loop.Completed()
	h.idle(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := h.loop.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if h.ft.count() != 4 || h.ft.query(3).Get("event") != "stopped" {
		t.Fatalf("Stop must send exactly one stopped announce; got %d announces", h.ft.count())
	}
	if err := h.loop.Stop(ctx); err != nil || h.ft.count() != 4 {
		t.Fatal("a second Stop must not announce again")
	}
	h.loop.Completed()
	h.fake.Advance(time.Hour)
	time.Sleep(50 * time.Millisecond)
	if h.ft.count() != 4 {
		t.Fatal("no announces after Stop")
	}
	h.ft.srv.CloseClientConnections()
	deadline := time.Now().Add(3 * time.Second)
	for runtime.NumGoroutine() > base+2 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if n := runtime.NumGoroutine(); n > base+2 {
		t.Errorf("goroutines after Stop: %d, before: %d", n, base)
	}
}

func TestContractLoopRetry(t *testing.T) {
	h := startTestLoop(t, func(n int, _ url.Values) (int, string) {
		if n < 2 {
			return 200, "d14:failure reason4:busye"
		}
		return 200, "d8:intervali600e5:peers0:e"
	})
	defer h.loop.Stop(context.Background())
	h.ft.waitCount(t, 1)
	h.idle(t)
	h.fake.Advance(MinRetry - time.Second)
	time.Sleep(50 * time.Millisecond)
	if h.ft.count() != 1 {
		t.Fatal("retried before MinRetry")
	}
	h.fake.Advance(time.Second)
	h.ft.waitCount(t, 2)
	h.idle(t)
	h.fake.Advance(2*MinRetry - time.Second)
	time.Sleep(50 * time.Millisecond)
	if h.ft.count() != 2 {
		t.Fatal("second retry must wait 2*MinRetry")
	}
	h.fake.Advance(time.Second)
	h.ft.waitCount(t, 3)
	for i := 0; i < 3; i++ {
		if e := h.ft.query(i).Get("event"); e != "started" {
			t.Fatalf("announce %d event=%q; retries keep event=started", i, e)
		}
	}
	// Completed requested now goes out right away.
	h.idle(t)
	h.loop.Completed()
	h.ft.waitCount(t, 4)
	if e := h.ft.query(3).Get("event"); e != "completed" {
		t.Fatalf("event=%q, want completed", e)
	}
}

func TestContractLoopCompletedBeforeStarted(t *testing.T) {
	release := make(chan struct{})
	h := startTestLoop(t, func(n int, _ url.Values) (int, string) {
		if n == 0 {
			<-release
		}
		return 200, "d8:intervali600e5:peers0:e"
	})
	defer h.loop.Stop(context.Background())
	h.ft.waitCount(t, 1)
	h.loop.Completed() // while started is in flight
	close(release)
	h.ft.waitCount(t, 2)
	if h.ft.query(0).Get("event") != "started" || h.ft.query(1).Get("event") != "completed" {
		t.Fatalf("events %q then %q, want started then completed", h.ft.query(0).Get("event"), h.ft.query(1).Get("event"))
	}
}

func TestContractLoopStopBounded(t *testing.T) {
	block := make(chan struct{})
	defer close(block)
	h := startTestLoop(t, func(n int, _ url.Values) (int, string) {
		if n > 0 {
			<-block // the stopped announce hangs
		}
		return 200, "d8:intervali600e5:peers0:e"
	})
	h.ft.waitCount(t, 1)
	h.idle(t)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := h.loop.Stop(ctx); err == nil {
		t.Error("a hanging stopped announce must return the ctx error")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("Stop took %s; it must be bounded by ctx", d)
	}
}
