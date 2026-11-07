package tracker

import (
	"context"
	"errors"
	"math/rand/v2"
	"net/http"
	"strings"
	"sync"
)

// ErrNoTrackers is returned when a torrent has no usable tracker.
var ErrNoTrackers = errors.New("tracker: no usable trackers")

// Tiers announces through a BEP 12 tier list: tiers are tried in order,
// each tier is shuffled once, and a tracker that responds is moved to the
// front of its tier. It is safe for concurrent use.
type Tiers struct {
	client *http.Client

	mu    sync.Mutex
	tiers [][]string
	ids   map[string]string // tracker id per URL
}

// NewTiers builds a tier list. URLs with schemes other than http and https
// are skipped (UDP trackers arrive in milestone 5). rng shuffles each tier
// (T2); a nil rng keeps the order.
func NewTiers(tiers [][]string, rng *rand.Rand, client *http.Client) *Tiers {
	t := &Tiers{client: client, ids: map[string]string{}}
	if t.client == nil {
		t.client = http.DefaultClient
	}
	for _, tier := range tiers {
		var urls []string
		for _, u := range tier {
			if l := strings.ToLower(u); strings.HasPrefix(l, "http://") || strings.HasPrefix(l, "https://") {
				urls = append(urls, u)
			}
		}
		if len(urls) == 0 {
			continue
		}
		if rng != nil {
			rng.Shuffle(len(urls), func(i, j int) { urls[i], urls[j] = urls[j], urls[i] })
		}
		t.tiers = append(t.tiers, urls)
	}
	return t
}

// Len returns the number of usable trackers.
func (t *Tiers) Len() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	n := 0
	for _, tier := range t.tiers {
		n += len(tier)
	}
	return n
}

// Announce sends r to the first tracker that responds, in BEP 12 order. It
// returns the response and the URL that gave it, or the last error.
func (t *Tiers) Announce(ctx context.Context, r Request) (*Response, string, error) {
	t.mu.Lock()
	order := make([][]string, len(t.tiers))
	for i, tier := range t.tiers {
		order[i] = append([]string(nil), tier...)
	}
	t.mu.Unlock()

	err := ErrNoTrackers
	for ti, tier := range order {
		for _, u := range tier {
			if ctx.Err() != nil {
				return nil, "", ctx.Err()
			}
			req := r
			t.mu.Lock()
			req.TrackerID = t.ids[u]
			t.mu.Unlock()
			resp, aerr := AnnounceHTTP(ctx, t.client, u, req)
			if aerr != nil {
				err = aerr
				continue
			}
			t.promote(ti, u, resp.TrackerID)
			return resp, u, nil
		}
	}
	return nil, "", err
}

func (t *Tiers) promote(ti int, u, trackerID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if trackerID != "" {
		t.ids[u] = trackerID
	}
	if ti >= len(t.tiers) {
		return
	}
	tier := t.tiers[ti]
	for i, x := range tier {
		if x == u {
			copy(tier[1:i+1], tier[:i])
			tier[0] = u
			return
		}
	}
}
