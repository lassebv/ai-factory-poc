// Package fakes holds harness-owned test doubles: an HTTP tracker and a
// scriptable BitTorrent peer. They are written from BEP 3 directly and never
// share code with torrentd.
package fakes

import (
	"encoding/binary"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"torrentd.local/harness/acceptance/internal/benc"
)

// Announce is one announce request as the tracker saw it.
type Announce struct {
	Time       time.Time
	InfoHash   string // raw 20 bytes
	PeerID     string // raw 20 bytes
	Port       int
	Uploaded   int64
	Downloaded int64
	Left       int64
	Event      string
	Compact    string
	NumWant    string
	Key        string
	Query      url.Values
	Path       string
}

// Scrape is one BEP 48 scrape request as the tracker saw it.
type Scrape struct {
	Time       time.Time
	Path       string
	InfoHashes []string // raw 20 bytes each
}

// Tracker is a fake HTTP tracker. It hands out a fixed peer list chosen by
// the test (not the announcing clients), so tests are deterministic.
type Tracker struct {
	Server *httptest.Server

	mu        sync.Mutex
	announces []Announce
	peers     []netip.AddrPort
	compact   bool
	interval  int
	failure   string
	status    int
	minIntvl  int
	complete  int
	incompl   int
	scrape    [3]int // complete, downloaded, incomplete
	scrapes   []Scrape
	notify    chan struct{}
}

// NewTracker starts a tracker on 127.0.0.1.
func NewTracker() *Tracker {
	t := &Tracker{compact: true, interval: 1800, minIntvl: 60, complete: 1, notify: make(chan struct{}, 1000)}
	t.Server = httptest.NewServer(http.HandlerFunc(t.handle))
	return t
}

// URL is the announce URL.
func (t *Tracker) URL() string { return t.Server.URL + "/announce" }

// URLPath is an announce URL with a custom path (e.g. "/x/tracker.php",
// whose scrape URL can't be derived per BEP 48). Requests to it are recorded
// as announces.
func (t *Tracker) URLPath(path string) string { return t.Server.URL + path }

// SetHTTPStatus makes every announce fail with this HTTP status and an empty
// body (0 restores normal answers).
func (t *Tracker) SetHTTPStatus(code int) { t.mu.Lock(); t.status = code; t.mu.Unlock() }

// SetInterval sets "interval" and "min interval" (seconds).
func (t *Tracker) SetInterval(interval, minInterval int) {
	t.mu.Lock()
	t.interval, t.minIntvl = interval, minInterval
	t.mu.Unlock()
}

// SetSwarm sets "complete" and "incomplete" in announce responses.
func (t *Tracker) SetSwarm(complete, incomplete int) {
	t.mu.Lock()
	t.complete, t.incompl = complete, incomplete
	t.mu.Unlock()
}

// SetScrape sets the counts returned for every scraped info-hash.
func (t *Tracker) SetScrape(complete, downloaded, incomplete int) {
	t.mu.Lock()
	t.scrape = [3]int{complete, downloaded, incomplete}
	t.mu.Unlock()
}

// Scrapes returns a copy of every scrape so far.
func (t *Tracker) Scrapes() []Scrape {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]Scrape(nil), t.scrapes...)
}


// Close stops the tracker.
func (t *Tracker) Close() { t.Server.Close() }

// SetPeers sets the peers returned to every announce.
func (t *Tracker) SetPeers(peers ...netip.AddrPort) {
	t.mu.Lock()
	t.peers = peers
	t.mu.Unlock()
}

// SetCompact chooses compact (default) or dict peer lists.
func (t *Tracker) SetCompact(c bool) {
	t.mu.Lock()
	t.compact = c
	t.mu.Unlock()
}

// SetFailure makes every announce return "failure reason" (empty clears it).
func (t *Tracker) SetFailure(reason string) {
	t.mu.Lock()
	t.failure = reason
	t.mu.Unlock()
}

// Announces returns a copy of every announce so far.
func (t *Tracker) Announces() []Announce {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]Announce(nil), t.announces...)
}

// WaitFor waits until pred holds for some announce, or the timeout expires.
func (t *Tracker) WaitFor(timeout time.Duration, pred func(Announce) bool) (Announce, bool) {
	deadline := time.After(timeout)
	for {
		for _, a := range t.Announces() {
			if pred(a) {
				return a, true
			}
		}
		select {
		case <-t.notify:
		case <-deadline:
			return Announce{}, false
		}
	}
}

func atoi64(s string) int64 { n, _ := strconv.ParseInt(s, 10, 64); return n }

func (t *Tracker) handleScrape(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	sc := Scrape{Time: time.Now(), Path: r.URL.Path, InfoHashes: q["info_hash"]}
	t.mu.Lock()
	t.scrapes = append(t.scrapes, sc)
	counts := t.scrape
	t.mu.Unlock()
	select {
	case t.notify <- struct{}{}:
	default:
	}
	files := map[string]any{}
	for _, h := range sc.InfoHashes {
		files[h] = map[string]any{"complete": counts[0], "downloaded": counts[1], "incomplete": counts[2]}
	}
	w.Header().Set("Content-Type", "text/plain")
	w.Write(benc.Encode(map[string]any{"files": files}))
}

func (t *Tracker) handle(w http.ResponseWriter, r *http.Request) {
	// BEP 48: the scrape URL replaces the last "announce" path component
	// with "scrape". Without this, a client's scrape would be recorded as an
	// announce and confuse announce-counting checks from earlier milestones.
	if strings.HasSuffix(r.URL.Path, "/scrape") || strings.HasPrefix(path.Base(r.URL.Path), "scrape") {
		t.handleScrape(w, r)
		return
	}
	q := r.URL.Query()
	port, _ := strconv.Atoi(q.Get("port"))
	a := Announce{
		Time: time.Now(), InfoHash: q.Get("info_hash"), PeerID: q.Get("peer_id"), Port: port,
		Uploaded: atoi64(q.Get("uploaded")), Downloaded: atoi64(q.Get("downloaded")), Left: atoi64(q.Get("left")),
		Event: q.Get("event"), Compact: q.Get("compact"), NumWant: q.Get("numwant"), Key: q.Get("key"), Query: q,
		Path: r.URL.Path,
	}
	t.mu.Lock()
	t.announces = append(t.announces, a)
	peers, compact, interval, failure := t.peers, t.compact, t.interval, t.failure
	status, minIntvl, complete, incompl := t.status, t.minIntvl, t.complete, t.incompl
	t.mu.Unlock()
	select {
	case t.notify <- struct{}{}:
	default:
	}

	if status != 0 {
		w.WriteHeader(status)
		return
	}
	resp := map[string]any{"interval": interval, "min interval": minIntvl, "complete": complete, "incomplete": incompl}
	if failure != "" {
		resp = map[string]any{"failure reason": failure}
	} else if compact {
		var b []byte
		for _, p := range peers {
			if p.Addr().Is4() {
				ip := p.Addr().As4()
				b = append(b, ip[:]...)
				b = binary.BigEndian.AppendUint16(b, p.Port())
			}
		}
		resp["peers"] = b
	} else {
		var list []any
		for _, p := range peers {
			list = append(list, map[string]any{"ip": p.Addr().String(), "port": int(p.Port()), "peer id": "-FP0001-000000000000"})
		}
		if list == nil {
			list = []any{}
		}
		resp["peers"] = list
	}
	w.Header().Set("Content-Type", "text/plain")
	w.Write(benc.Encode(resp))
}
