package session

import (
	"context"
	"time"

	"torrentd/internal/bt/tracker"
)

// Announce timing.
const (
	announceTimeout  = 30 * time.Second
	defaultInterval  = 30 * time.Minute
	minRetry         = 15 * time.Second
	maxRetry         = 30 * time.Minute
	minimumInterval  = 30 * time.Second // floor for a tracker's interval
	trackerNumWanted = tracker.DefaultNumWant
)

// request builds an announce from the latest stats.
func (t *Torrent) request(event string) tracker.Request {
	st := t.Stats()
	return tracker.Request{
		InfoHash:   t.mi.InfoHash,
		PeerID:     t.s.peerID,
		Port:       t.s.Port(),
		Uploaded:   st.UploadedTotal,
		Downloaded: st.DownloadedTotal,
		Left:       st.Left,
		Event:      event,
		Key:        t.s.key,
		NumWant:    trackerNumWanted,
	}
}

// announceLoop runs the torrent's announces: started first, completed when
// the download finishes, regular announces on the tracker's interval, and
// stopped (bounded by the stop timeout) when the torrent stops. A failed
// announce is retried with exponential backoff.
func (t *Torrent) announceLoop() {
	defer t.annWG.Done()
	if t.tiers.Len() == 0 {
		t.log.Warn("torrent has no HTTP trackers; nothing to announce to")
		return
	}
	pending := tracker.EventStarted
	finished := t.finished
	announced := false
	failures := 0
	timer := t.s.clk.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-t.ctx.Done():
			if announced {
				t.announceStopped()
			}
			return
		case <-finished:
			finished = nil
			if pending == tracker.EventNone {
				pending = tracker.EventCompleted
			}
			timer.Reset(0)
			continue
		case <-timer.C():
		}

		event := pending
		ctx, cancel := context.WithTimeout(t.ctx, announceTimeout)
		resp, url, err := t.tiers.Announce(ctx, t.request(event))
		cancel()
		if t.ctx.Err() != nil {
			continue // stopping: the next loop iteration sends stopped
		}
		if err != nil {
			failures++
			wait := min(minRetry<<min(failures-1, 10), maxRetry)
			t.log.Warn("announce failed", "event", event, "err", err, "retry_in", wait)
			timer.Reset(wait)
			continue
		}
		failures = 0
		announced = true
		if resp.Warning != "" {
			t.log.Warn("tracker warning", "url", url, "warning", resp.Warning)
		}
		t.log.Debug("announced", "url", url, "event", event, "peers", len(resp.Peers))
		switch {
		case event == tracker.EventStarted && finished == nil:
			// The download finished while started was in flight.
			pending = tracker.EventCompleted
		default:
			pending = tracker.EventNone
		}
		t.send(evSwarm{resp.Seeders, resp.Leechers})
		if len(resp.Peers) > 0 {
			t.send(evPeers{resp.Peers})
		}
		if pending != tracker.EventNone {
			timer.Reset(0)
			continue
		}
		interval := resp.Interval
		if interval <= 0 {
			interval = defaultInterval
		}
		interval = max(interval, resp.MinInterval, minimumInterval)
		timer.Reset(interval)
	}
}

// announceStopped sends the stopped announce within the stop timeout
// (§6.2 step 2).
func (t *Torrent) announceStopped() {
	ctx, cancel := context.WithTimeout(context.Background(), t.s.cfg.StopTimeout)
	defer cancel()
	if _, _, err := t.tiers.Announce(ctx, t.request(tracker.EventStopped)); err != nil {
		t.log.Warn("stopped announce failed", "err", err)
	}
}
