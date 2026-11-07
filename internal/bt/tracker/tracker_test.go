package tracker

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"torrentd/internal/clock"
)

func TestPercentEncode(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"", ""},
		{"azAZ09.-_~", "azAZ09.-_~"},
		{" ", "%20"},
		{"+", "%2B"},
		{"%&=?#/", "%25%26%3D%3F%23%2F"},
		{"\x00\xff\x7f", "%00%FF%7F"},
	} {
		if got := percentEncode([]byte(tc.in)); got != tc.want {
			t.Errorf("percentEncode(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestBuildAnnounceURL(t *testing.T) {
	req := AnnounceRequest{Event: EventStopped, Port: 1}
	u, err := buildAnnounceURL("http://h/a?x=1#frag", req)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(u, "http://h/a?x=1&info_hash=") || strings.Contains(u, "#") {
		t.Errorf("url = %s", u)
	}
	if !strings.Contains(u, "&compact=1&numwant=50") || !strings.HasSuffix(u, "&event=stopped") {
		t.Errorf("url = %s", u)
	}
	if strings.Contains(u, "key=") {
		t.Errorf("empty key must be omitted: %s", u)
	}
	for _, bad := range []string{"udp://h:1/a", "ftp://h/a", "http:///a", "http://%zz/", "wss://h/"} {
		if _, err := buildAnnounceURL(bad, req); err == nil {
			t.Errorf("%q: want error", bad)
		}
	}
	if _, err := buildAnnounceURL("HTTPS://h/a", req); err != nil {
		t.Errorf("upper-case scheme: %v", err)
	}
}

func TestParseResponseMalformed(t *testing.T) {
	for _, body := range []string{
		"", "  ", "le", "d", "d8:intervali60e", "d8:intervali60ee trailing", "3:abc",
	} {
		if _, err := parseResponse([]byte(body)); err == nil {
			t.Errorf("%q: want error", body)
		}
	}
	// Non-string failure reason is still a failure.
	_, err := parseResponse([]byte("d14:failure reasoni1ee"))
	var fe *FailureError
	if !errors.As(err, &fe) {
		t.Errorf("non-string failure reason: %v", err)
	}
}

func TestParseResponseLenient(t *testing.T) {
	// Unsorted keys, wrong-typed fields, negative values, odd compact length.
	body := "d5:peers7:\x01\x02\x03\x04\x00\x50X8:intervali-5e12:min interval3:abc" +
		"8:completei-1e10:incomplete1:x6:peers63:abce"
	r, err := parseResponse([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if r.Interval != 0 || r.MinInterval != 0 || r.Seeders != -1 || r.Leechers != -1 {
		t.Errorf("response = %+v", r)
	}
	if len(r.Peers) != 1 || r.Peers[0] != netip.MustParseAddrPort("1.2.3.4:80") {
		t.Errorf("peers = %v", r.Peers)
	}
	// Dict peers with bad entries and a non-list/non-string peers value.
	body = "d5:peersl" + "i1e" + "d4:porti80ee" + "d2:ip7:1.2.3.44:porti70000ee" +
		"d2:ip9:fe80::1%14:porti5ee" + "d2:ip7:1.2.3.44:port1:xe" + "d2:ip7:1.2.3.44:porti9eee" + "e"
	r, err = parseResponse([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Peers) != 0 {
		t.Errorf("a list with a non-dict element is ignored: %v", r.Peers)
	}
	body = "d5:peersl" + "d4:porti80ee" + "d2:ip7:1.2.3.44:porti70000ee" +
		"d2:ip10:fe80::1%254:porti5ee" + "d2:ip7:1.2.3.44:port1:xe" + "d2:ip7:1.2.3.44:porti9eee" + "e"
	r, err = parseResponse([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Peers) != 1 || r.Peers[0] != netip.MustParseAddrPort("1.2.3.4:9") {
		t.Errorf("peers = %v", r.Peers)
	}
	r, err = parseResponse([]byte("d5:peersi3e8:intervali99999999999999ee"))
	if err != nil || r.Peers != nil || r.Interval != maxIntervalSeconds*time.Second {
		t.Errorf("r=%+v err=%v", r, err)
	}
}

func TestClientStatusAndSize(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/big" {
			w.Write([]byte("d5:peers"))
			w.Write([]byte("99999999:"))
			w.Write(make([]byte, maxResponseBytes))
			return
		}
		http.Error(w, "x", http.StatusNotFound)
	}))
	defer srv.Close()
	c := NewHTTPClient(srv.Client())
	if _, err := c.Announce(context.Background(), srv.URL+"/x", AnnounceRequest{}); err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("404: %v", err)
	}
	if _, err := c.Announce(context.Background(), srv.URL+"/big", AnnounceRequest{}); err == nil {
		t.Error("oversized body must fail")
	}
	if _, err := c.Announce(context.Background(), "http://127.0.0.1:1/a", AnnounceRequest{}); err == nil {
		t.Error("connection refused must fail")
	}
}

func TestRetryDelayAndInterval(t *testing.T) {
	for n, want := range map[int]time.Duration{1: 15 * time.Second, 2: 30 * time.Second, 3: time.Minute, 8: 30 * time.Minute, 100: 30 * time.Minute} {
		if got := retryDelay(n); got != want {
			t.Errorf("retryDelay(%d) = %s, want %s", n, got, want)
		}
	}
	for _, tc := range []struct {
		r    AnnounceResponse
		want time.Duration
	}{
		{AnnounceResponse{}, DefaultInterval},
		{AnnounceResponse{Interval: time.Minute, MinInterval: 2 * time.Minute}, 2 * time.Minute},
		{AnnounceResponse{MinInterval: time.Hour}, time.Hour},
		{AnnounceResponse{Interval: time.Minute}, time.Minute},
	} {
		if got := nextInterval(&tc.r); got != tc.want {
			t.Errorf("nextInterval(%+v) = %s, want %s", tc.r, got, tc.want)
		}
	}
}

func TestEventStringUnknown(t *testing.T) {
	if Event(42).String() != "" {
		t.Error("unknown event must be empty")
	}
	if !strings.Contains((&FailureError{Reason: "r"}).Error(), "r") {
		t.Error("FailureError must include the reason")
	}
}

// scriptAnnouncer is an in-memory Announcer for loop tests.
type scriptAnnouncer struct {
	mu     sync.Mutex
	events []Event
	fail   func(n int, ev Event) error
	resp   *AnnounceResponse
	calls  chan Event
}

func (s *scriptAnnouncer) Announce(ctx context.Context, req AnnounceRequest) (*AnnounceResponse, error) {
	s.mu.Lock()
	n := len(s.events)
	s.events = append(s.events, req.Event)
	s.mu.Unlock()
	if s.calls != nil {
		s.calls <- req.Event
	}
	if s.fail != nil {
		if err := s.fail(n, req.Event); err != nil {
			return nil, err
		}
	}
	return s.resp, nil
}

func (s *scriptAnnouncer) Tiers() [][]string { return nil }

func waitIdle(t *testing.T, f *clock.Fake) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for f.Waiters() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("loop never idle")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestLoopCompletedReplacesRegularRetry(t *testing.T) {
	fake := clock.NewFake(time.Unix(0, 0))
	sa := &scriptAnnouncer{
		resp:  &AnnounceResponse{Interval: time.Minute},
		calls: make(chan Event, 16),
		fail: func(n int, ev Event) error {
			if ev == EventNone {
				return errors.New("down")
			}
			return nil
		},
	}
	l := StartLoop(LoopConfig{Announcer: sa, Clock: fake}) // nil Stats, OnPeers, Logger
	if ev := <-sa.calls; ev != EventStarted {
		t.Fatalf("first = %v", ev)
	}
	waitIdle(t, fake)
	fake.Advance(time.Minute)
	if ev := <-sa.calls; ev != EventNone {
		t.Fatalf("second = %v", ev)
	}
	waitIdle(t, fake) // retry pending
	l.Completed()
	if ev := <-sa.calls; ev != EventCompleted {
		t.Fatalf("third = %v, want completed instead of the retry", ev)
	}
	waitIdle(t, fake)
	if err := l.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if ev := <-sa.calls; ev != EventStopped {
		t.Fatalf("last = %v", ev)
	}
}

func TestLoopCompletedRetriedAndStartedRetryIgnoresCompleted(t *testing.T) {
	fake := clock.NewFake(time.Unix(0, 0))
	sa := &scriptAnnouncer{
		resp:  &AnnounceResponse{Interval: time.Hour, Peers: []netip.AddrPort{netip.MustParseAddrPort("1.1.1.1:1")}},
		calls: make(chan Event, 16),
		fail: func(n int, ev Event) error {
			if n == 0 || n == 2 { // first started and first completed fail
				return errors.New("down")
			}
			return nil
		},
	}
	var peers int
	l := StartLoop(LoopConfig{Announcer: sa, Clock: fake, OnPeers: func([]netip.AddrPort) { peers++ }})
	<-sa.calls
	waitIdle(t, fake)
	l.Completed() // started not yet succeeded: must keep waiting
	time.Sleep(20 * time.Millisecond)
	if len(sa.calls) != 0 {
		t.Fatal("completed sent before started succeeded")
	}
	fake.Advance(MinRetry)
	if ev := <-sa.calls; ev != EventStarted {
		t.Fatalf("retry = %v", ev)
	}
	if ev := <-sa.calls; ev != EventCompleted {
		t.Fatalf("after started = %v", ev)
	}
	waitIdle(t, fake)
	fake.Advance(MinRetry)
	if ev := <-sa.calls; ev != EventCompleted {
		t.Fatalf("completed retry = %v", ev)
	}
	waitIdle(t, fake)
	l.Stop(context.Background())
	if peers != 2 {
		t.Errorf("OnPeers calls = %d, want 2", peers)
	}
	l.Completed() // after Stop: no-op
	if err := l.Stop(context.Background()); err != nil {
		t.Error(err)
	}
}

func TestAnnouncerCancelledCtx(t *testing.T) {
	a := NewAnnouncer([][]string{{"http://a/"}, {"http://b/"}}, NewHTTPClient(nil), nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := a.Announce(ctx, AnnounceRequest{})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v", err)
	}
}
