package tracker

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"torrentd/internal/clock"
)

type loop struct {
	cfg    LoopConfig
	cancel context.CancelFunc
	done   chan struct{}

	// completedReq is set by Completed; completedCh wakes an idle loop.
	completedReq atomic.Bool
	completedCh  chan struct{}

	stopOnce sync.Once
	stopped  atomic.Bool
}

func startLoop(cfg LoopConfig) Loop {
	if cfg.Clock == nil {
		cfg.Clock = clock.Real()
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Stats == nil {
		cfg.Stats = func() Stats { return Stats{} }
	}
	ctx, cancel := context.WithCancel(context.Background())
	l := &loop{
		cfg:         cfg,
		cancel:      cancel,
		done:        make(chan struct{}),
		completedCh: make(chan struct{}, 1),
	}
	go l.run(ctx)
	return l
}

// Completed implements Loop.
func (l *loop) Completed() {
	if l.stopped.Load() || !l.completedReq.CompareAndSwap(false, true) {
		return
	}
	select {
	case l.completedCh <- struct{}{}:
	default:
	}
}

// Stop implements Loop.
func (l *loop) Stop(ctx context.Context) error {
	var err error
	l.stopOnce.Do(func() {
		l.stopped.Store(true)
		l.cancel()
		<-l.done
		_, err = l.cfg.Announcer.Announce(ctx, l.request(EventStopped))
		if err != nil {
			l.cfg.Logger.Warn("tracker: stopped announce failed", "err", err)
		}
	})
	return err
}

func (l *loop) request(ev Event) AnnounceRequest {
	s := l.cfg.Stats()
	return AnnounceRequest{
		InfoHash: l.cfg.InfoHash, PeerID: l.cfg.PeerID, Port: l.cfg.Port,
		Uploaded: s.Uploaded, Downloaded: s.Downloaded, Left: s.Left,
		Event: ev, Key: l.cfg.Key,
	}
}

// retryDelay returns the delay after the n-th (1-based) consecutive failure.
func retryDelay(n int) time.Duration {
	d := MinRetry
	for i := 1; i < n && d < MaxRetry; i++ {
		d *= 2
	}
	return min(d, MaxRetry)
}

// nextInterval returns the wait after a successful announce.
func nextInterval(r *AnnounceResponse) time.Duration {
	if r.Interval <= 0 {
		return max(DefaultInterval, r.MinInterval)
	}
	return max(r.Interval, r.MinInterval)
}

func (l *loop) run(ctx context.Context) {
	defer close(l.done)
	ev := EventStarted
	started, completedSent := false, false
	failures := 0
	for {
		resp, err := l.cfg.Announcer.Announce(ctx, l.request(ev))
		if ctx.Err() != nil {
			return
		}
		var wait time.Duration
		if err != nil {
			failures++
			wait = retryDelay(failures)
			l.cfg.Logger.Warn("tracker: announce failed", "event", ev.String(), "retry_in", wait, "err", err)
		} else {
			failures = 0
			switch ev {
			case EventStarted:
				started = true
			case EventCompleted:
				completedSent = true
			}
			if resp.Warning != "" {
				l.cfg.Logger.Warn("tracker: warning message", "warning", resp.Warning)
			}
			if len(resp.Peers) > 0 && l.cfg.OnPeers != nil {
				l.cfg.OnPeers(resp.Peers)
			}
			wait = nextInterval(resp)
			ev = EventNone
		}
		// A pending completed goes out at once once started has succeeded
		// (unless that very announce is the one being retried).
		wantCompleted := func() bool {
			return started && !completedSent && ev != EventCompleted && l.completedReq.Load()
		}
		if err == nil && wantCompleted() {
			ev = EventCompleted
			continue
		}
		if !l.wait(ctx, wait, wantCompleted) {
			return
		}
		if wantCompleted() {
			ev = EventCompleted
		}
	}
}

// wait holds one clock timer for d. It returns false if ctx is cancelled
// and true when the timer fires or wake reports true after a Completed.
func (l *loop) wait(ctx context.Context, d time.Duration, wake func() bool) bool {
	t := l.cfg.Clock.NewTimer(d)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return false
		case <-t.C():
			return true
		case <-l.completedCh:
			if wake() {
				return true
			}
		}
	}
}
