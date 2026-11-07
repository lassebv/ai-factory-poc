package clock

import (
	"sort"
	"sync"
	"time"
)

// Fake is a manually driven Clock for tests. Time only moves when Advance or
// Set is called; timers and tickers whose deadline is reached fire then.
type Fake struct {
	mu      sync.Mutex
	now     time.Time
	waiters []*fakeWaiter
}

type fakeWaiter struct {
	fake     *Fake
	deadline time.Time
	period   time.Duration // > 0 for tickers
	ch       chan time.Time
	active   bool
}

// NewFake returns a Fake clock set to start.
func NewFake(start time.Time) *Fake { return &Fake{now: start} }

// Now returns the fake current time.
func (f *Fake) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

// After returns a channel that receives the fake time once d has elapsed.
func (f *Fake) After(d time.Duration) <-chan time.Time { return f.NewTimer(d).C() }

// NewTimer returns a Timer that fires when the fake time reaches now+d.
func (f *Fake) NewTimer(d time.Duration) Timer {
	return fakeTimer{f.add(d, 0)}
}

// NewTicker returns a Ticker that fires every d of fake time.
func (f *Fake) NewTicker(d time.Duration) Ticker {
	if d <= 0 {
		panic("clock: non-positive interval for NewTicker")
	}
	return fakeTicker{f.add(d, d)}
}

// Advance moves the fake time forward by d and fires due timers and tickers.
func (f *Fake) Advance(d time.Duration) { f.Set(f.Now().Add(d)) }

// Set moves the fake time to t (never backwards) and fires due timers.
func (f *Fake) Set(t time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if t.Before(f.now) {
		return
	}
	for {
		w := f.nextDue(t)
		if w == nil {
			break
		}
		f.now = w.deadline
		select {
		case w.ch <- w.deadline:
		default: // like time.Ticker, drop ticks for slow receivers
		}
		if w.period > 0 {
			w.deadline = w.deadline.Add(w.period)
		} else {
			w.active = false
		}
	}
	f.now = t
	f.compact()
}

// Waiters returns the number of active timers and tickers.
func (f *Fake) Waiters() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.compact()
	return len(f.waiters)
}

func (f *Fake) add(d time.Duration, period time.Duration) *fakeWaiter {
	f.mu.Lock()
	defer f.mu.Unlock()
	w := &fakeWaiter{fake: f, deadline: f.now.Add(d), period: period, ch: make(chan time.Time, 1), active: true}
	f.waiters = append(f.waiters, w)
	if period == 0 && d <= 0 {
		w.ch <- f.now
		w.active = false
	}
	return w
}

// nextDue returns the active waiter with the earliest deadline <= t.
func (f *Fake) nextDue(t time.Time) *fakeWaiter {
	var due []*fakeWaiter
	for _, w := range f.waiters {
		if w.active && !w.deadline.After(t) {
			due = append(due, w)
		}
	}
	if len(due) == 0 {
		return nil
	}
	sort.SliceStable(due, func(i, j int) bool { return due[i].deadline.Before(due[j].deadline) })
	return due[0]
}

func (f *Fake) compact() {
	kept := f.waiters[:0]
	for _, w := range f.waiters {
		if w.active {
			kept = append(kept, w)
		}
	}
	f.waiters = kept
}

func (w *fakeWaiter) stop() bool {
	w.fake.mu.Lock()
	defer w.fake.mu.Unlock()
	was := w.active
	w.active = false
	return was
}

func (w *fakeWaiter) reset(d, period time.Duration) bool {
	w.fake.mu.Lock()
	defer w.fake.mu.Unlock()
	was := w.active
	w.deadline = w.fake.now.Add(d)
	w.period = period
	w.active = true
	for _, x := range w.fake.waiters {
		if x == w {
			return was
		}
	}
	w.fake.waiters = append(w.fake.waiters, w)
	return was
}

type fakeTimer struct{ w *fakeWaiter }

func (t fakeTimer) C() <-chan time.Time        { return t.w.ch }
func (t fakeTimer) Stop() bool                 { return t.w.stop() }
func (t fakeTimer) Reset(d time.Duration) bool { return t.w.reset(d, 0) }

type fakeTicker struct{ w *fakeWaiter }

func (t fakeTicker) C() <-chan time.Time { return t.w.ch }
func (t fakeTicker) Stop()               { t.w.stop() }
func (t fakeTicker) Reset(d time.Duration) {
	if d <= 0 {
		panic("clock: non-positive interval for Ticker.Reset")
	}
	t.w.reset(d, d)
}
