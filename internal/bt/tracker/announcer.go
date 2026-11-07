package tracker

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"sync"
)

// errNoTrackers is returned by an Announcer that has no tracker URLs.
var errNoTrackers = errors.New("tracker: no trackers")

type announcer struct {
	client Client

	mu    sync.Mutex
	tiers [][]string
	ids   map[string]string // tracker id per URL
}

func newAnnouncer(tiers [][]string, c Client, rnd *rand.Rand) Announcer {
	a := &announcer{client: c, ids: make(map[string]string)}
	for _, tier := range tiers {
		var t []string
		for _, u := range tier {
			if u != "" {
				t = append(t, u)
			}
		}
		if len(t) == 0 {
			continue
		}
		if rnd != nil {
			rnd.Shuffle(len(t), func(i, j int) { t[i], t[j] = t[j], t[i] })
		}
		a.tiers = append(a.tiers, t)
	}
	return a
}

// Tiers implements Announcer.
func (a *announcer) Tiers() [][]string {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([][]string, len(a.tiers))
	for i, t := range a.tiers {
		out[i] = slices.Clone(t)
	}
	return out
}

// Announce implements Announcer. The lock is not held during HTTP requests.
func (a *announcer) Announce(ctx context.Context, req AnnounceRequest) (*AnnounceResponse, error) {
	tiers := a.Tiers()
	if len(tiers) == 0 {
		return nil, errNoTrackers
	}
	var errs []error
	for ti, tier := range tiers {
		for _, u := range tier {
			if err := ctx.Err(); err != nil {
				errs = append(errs, err)
				return nil, errors.Join(errs...)
			}
			r := req
			a.mu.Lock()
			r.TrackerID = a.ids[u]
			a.mu.Unlock()
			resp, err := a.client.Announce(ctx, u, r)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", u, err))
				continue
			}
			a.succeeded(ti, u, resp.TrackerID)
			return resp, nil
		}
	}
	return nil, errors.Join(errs...)
}

// succeeded promotes u to the front of tier ti and records its tracker id.
func (a *announcer) succeeded(ti int, u, trackerID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if trackerID != "" {
		a.ids[u] = trackerID
	}
	tier := a.tiers[ti]
	i := slices.Index(tier, u)
	if i <= 0 {
		return
	}
	copy(tier[1:i+1], tier[:i])
	tier[0] = u
}
