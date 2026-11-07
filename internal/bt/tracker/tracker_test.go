package tracker

import (
	"context"
	"errors"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestAnnounceURL(t *testing.T) {
	r := Request{
		InfoHash: [20]byte{0xff, ' ', '&'}, PeerID: [20]byte{'-', 'T', 'D'}, Port: 6881,
		Uploaded: 1, Downloaded: 2, Left: 3, Event: EventStarted, Key: "abc", TrackerID: "t 1",
	}
	s, err := AnnounceURL("http://tr.example/announce?passkey=x#frag", r)
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(s)
	q := u.Query()
	if q.Get("info_hash") != string(r.InfoHash[:]) || q.Get("peer_id") != string(r.PeerID[:]) {
		t.Errorf("hash/peer id not round-tripped: %s", s)
	}
	for k, v := range map[string]string{
		"passkey": "x", "port": "6881", "uploaded": "1", "downloaded": "2", "left": "3",
		"compact": "1", "numwant": "50", "key": "abc", "event": "started", "trackerid": "t 1",
	} {
		if q.Get(k) != v {
			t.Errorf("%s = %q, want %q", k, q.Get(k), v)
		}
	}
	if u.Fragment != "" {
		t.Error("fragment kept")
	}
	r.Event, r.Key, r.TrackerID, r.NumWant = EventNone, "", "", 10
	s, _ = AnnounceURL("https://tr.example/a", r)
	if strings.Contains(s, "event=") || strings.Contains(s, "key=") || !strings.Contains(s, "numwant=10") {
		t.Errorf("optional params: %s", s)
	}
	if _, err := AnnounceURL("udp://tr.example:80", r); err == nil {
		t.Error("udp accepted")
	}
	if _, err := AnnounceURL("://bad", r); err == nil {
		t.Error("bad URL accepted")
	}
}

func TestParseResponse(t *testing.T) {
	compact := "d8:completei5e10:incompletei7e8:intervali1800e12:min intervali60e" +
		"5:peers12:\x7f\x00\x00\x01\x1a\xe1\x0a\x00\x00\x02\x00\x00" +
		"6:peers618:\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x01\x1a\xe2" +
		"10:tracker id3:xyz15:warning message4:caree"
	r, err := ParseResponse([]byte(compact))
	if err != nil {
		t.Fatal(err)
	}
	want := []netip.AddrPort{netip.MustParseAddrPort("127.0.0.1:6881"), netip.MustParseAddrPort("[::1]:6882")}
	if len(r.Peers) != 2 || r.Peers[0] != want[0] || r.Peers[1] != want[1] {
		t.Errorf("peers = %v (the port-0 peer must be skipped)", r.Peers)
	}
	if r.Interval != 30*time.Minute || r.MinInterval != time.Minute || r.TrackerID != "xyz" ||
		r.Warning != "care" || r.Seeders != 5 || r.Leechers != 7 {
		t.Errorf("fields = %+v", r)
	}

	dict := "d5:peersld2:ip9:127.0.0.17:peer id20:-XX0000-0000000000004:porti6881eed2:ip6:a.host4:porti1eed2:ip3:::14:porti0eeee"
	r, err = ParseResponse([]byte(dict))
	if err != nil || len(r.Peers) != 1 || r.Peers[0] != want[0] || r.Seeders != -1 || r.Interval != 0 {
		t.Errorf("dict peers = %+v, %v", r, err)
	}

	if _, err := ParseResponse([]byte("d14:failure reason6:nope!!e")); err == nil || err.Error() != "tracker failure: nope!!" {
		t.Errorf("failure: %v", err)
	}
	for _, bad := range []string{"garbage", "d5:peers5:abcdee", "d5:peersi3ee", "d5:peerslee6:peers6i1ee", "d5:peersl3:abcee"} {
		if _, err := ParseResponse([]byte(bad)); !errors.Is(err, ErrMalformed) {
			t.Errorf("%q: %v", bad, err)
		}
	}
}

// fakeTracker serves announce responses chosen per URL path.
type fakeTracker struct {
	srv *httptest.Server
	mu  sync.Mutex
	hit []string
	ids []string
}

func newFake(t *testing.T) *fakeTracker {
	f := &fakeTracker{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.hit = append(f.hit, r.URL.Path)
		f.ids = append(f.ids, r.URL.Query().Get("trackerid"))
		f.mu.Unlock()
		switch r.URL.Path {
		case "/ok":
			w.Write([]byte("d8:intervali900e10:tracker id2:id5:peers6:\x7f\x00\x00\x01\x00\x01e"))
		case "/fail":
			w.Write([]byte("d14:failure reason4:gonee"))
		case "/500fail":
			w.WriteHeader(500)
			w.Write([]byte("d14:failure reason4:downe"))
		case "/big":
			w.Write([]byte(strings.Repeat("x", maxResponse+1)))
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeTracker) url(p string) string { return f.srv.URL + p }

func TestAnnounceHTTP(t *testing.T) {
	f := newFake(t)
	ctx := context.Background()
	r, err := AnnounceHTTP(ctx, http.DefaultClient, f.url("/ok"), Request{})
	if err != nil || len(r.Peers) != 1 || r.Interval != 15*time.Minute {
		t.Fatalf("ok: %+v %v", r, err)
	}
	var fe *FailureError
	if _, err := AnnounceHTTP(ctx, http.DefaultClient, f.url("/500fail"), Request{}); !errors.As(err, &fe) || fe.Reason != "down" {
		t.Errorf("500 with failure: %v", err)
	}
	if _, err := AnnounceHTTP(ctx, http.DefaultClient, f.url("/missing"), Request{}); err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("404: %v", err)
	}
	if _, err := AnnounceHTTP(ctx, http.DefaultClient, f.url("/big"), Request{}); !errors.Is(err, ErrMalformed) {
		t.Errorf("big: %v", err)
	}
	if _, err := AnnounceHTTP(ctx, http.DefaultClient, "http://127.0.0.1:1/x", Request{}); err == nil {
		t.Error("connection refused: no error")
	}
	if _, err := AnnounceHTTP(ctx, http.DefaultClient, "udp://x", Request{}); err == nil {
		t.Error("udp: no error")
	}
	if _, err := AnnounceHTTP(ctx, http.DefaultClient, "http://bad host/", Request{}); err == nil {
		t.Error("bad host: no error")
	}
}

func TestTiers(t *testing.T) {
	f := newFake(t)
	tiers := NewTiers([][]string{
		{"udp://skipped:80"},
		{f.url("/fail"), f.url("/missing")},
		{f.url("/fail"), f.url("/ok")},
	}, nil, nil)
	if tiers.Len() != 4 {
		t.Fatalf("Len = %d", tiers.Len())
	}
	r, u, err := tiers.Announce(context.Background(), Request{})
	if err != nil || u != f.url("/ok") || len(r.Peers) != 1 {
		t.Fatalf("Announce = %v %s %v", r, u, err)
	}
	// The responding tracker moved to the front of its tier and its
	// tracker id is sent back.
	f.mu.Lock()
	f.hit, f.ids = nil, nil
	f.mu.Unlock()
	if _, _, err := tiers.Announce(context.Background(), Request{}); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	hit, ids := f.hit, f.ids
	f.mu.Unlock()
	if len(hit) != 3 || hit[2] != "/ok" || ids[2] != "id" {
		t.Fatalf("second announce hit %v ids %q", hit, ids)
	}

	none := NewTiers([][]string{{"udp://x:1"}}, rand.New(rand.NewPCG(1, 2)), nil)
	if _, _, err := none.Announce(context.Background(), Request{}); err != ErrNoTrackers {
		t.Errorf("no trackers: %v", err)
	}
	bad := NewTiers([][]string{{f.url("/fail")}}, nil, nil)
	if _, _, err := bad.Announce(context.Background(), Request{}); err == nil {
		t.Error("all failing: no error")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := bad.Announce(ctx, Request{}); err != context.Canceled {
		t.Errorf("cancelled: %v", err)
	}
	shuffled := NewTiers([][]string{{f.url("/a"), f.url("/b"), f.url("/c")}}, rand.New(rand.NewPCG(1, 2)), nil)
	shuffled.promote(5, "x", "") // out-of-range tier is ignored
	if shuffled.Len() != 3 {
		t.Error("shuffle lost URLs")
	}
}
