// Package clock provides the injectable time source required by §16.10 T1.
//
// Engine packages (internal/bt/**, internal/stats, internal/store) must not
// call time.Now, time.After, time.NewTimer, time.NewTicker or time.Sleep
// directly. They take a Clock instead, so tests can drive time with Fake.
package clock

import "time"

// Clock is a source of the current time and of timers.
type Clock interface {
	// Now returns the current time.
	Now() time.Time
	// After returns a channel that receives the time once d has elapsed.
	After(d time.Duration) <-chan time.Time
	// NewTimer returns a Timer that fires once after d.
	NewTimer(d time.Duration) Timer
	// NewTicker returns a Ticker that fires every d. d must be positive.
	NewTicker(d time.Duration) Ticker
}

// Timer is a single-shot timer created by a Clock.
type Timer interface {
	// C returns the channel on which the time is delivered.
	C() <-chan time.Time
	// Stop prevents the timer from firing. It reports whether it was active.
	Stop() bool
	// Reset changes the timer to fire after d. It reports whether it was active.
	Reset(d time.Duration) bool
}

// Ticker delivers ticks at a fixed interval.
type Ticker interface {
	// C returns the channel on which ticks are delivered.
	C() <-chan time.Time
	// Stop turns the ticker off.
	Stop()
	// Reset stops the ticker and resets its period to d.
	Reset(d time.Duration)
}

// Real returns a Clock backed by the system clock.
func Real() Clock { return realClock{} }

type realClock struct{}

func (realClock) Now() time.Time                         { return time.Now() }
func (realClock) After(d time.Duration) <-chan time.Time { return time.After(d) }
func (realClock) NewTimer(d time.Duration) Timer         { return realTimer{time.NewTimer(d)} }
func (realClock) NewTicker(d time.Duration) Ticker       { return realTicker{time.NewTicker(d)} }

type realTimer struct{ t *time.Timer }

func (r realTimer) C() <-chan time.Time        { return r.t.C }
func (r realTimer) Stop() bool                 { return r.t.Stop() }
func (r realTimer) Reset(d time.Duration) bool { return r.t.Reset(d) }

type realTicker struct{ t *time.Ticker }

func (r realTicker) C() <-chan time.Time   { return r.t.C }
func (r realTicker) Stop()                 { r.t.Stop() }
func (r realTicker) Reset(d time.Duration) { r.t.Reset(d) }
