package clock

import (
	"testing"
	"time"
)

var t0 = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

func recv(t *testing.T, ch <-chan time.Time) (time.Time, bool) {
	t.Helper()
	select {
	case v := <-ch:
		return v, true
	default:
		return time.Time{}, false
	}
}

func TestRealClock(t *testing.T) {
	c := Real()
	before := time.Now()
	if n := c.Now(); n.Before(before) {
		t.Fatalf("Now went backwards")
	}
	<-c.After(time.Millisecond)
	tm := c.NewTimer(time.Millisecond)
	<-tm.C()
	if tm.Stop() {
		t.Error("Stop on fired timer reported active")
	}
	tm.Reset(time.Hour)
	if !tm.Stop() {
		t.Error("Stop on reset timer reported inactive")
	}
	tk := c.NewTicker(time.Millisecond)
	<-tk.C()
	tk.Reset(time.Millisecond)
	<-tk.C()
	tk.Stop()
}

func TestFakeNowAndAdvance(t *testing.T) {
	f := NewFake(t0)
	if !f.Now().Equal(t0) {
		t.Fatal("wrong start")
	}
	f.Advance(time.Second)
	if !f.Now().Equal(t0.Add(time.Second)) {
		t.Fatal("Advance did not move time")
	}
	f.Set(t0) // backwards is ignored
	if !f.Now().Equal(t0.Add(time.Second)) {
		t.Fatal("Set moved time backwards")
	}
}

func TestFakeTimer(t *testing.T) {
	f := NewFake(t0)
	tm := f.NewTimer(10 * time.Second)
	if f.Waiters() != 1 {
		t.Fatalf("Waiters = %d", f.Waiters())
	}
	f.Advance(9 * time.Second)
	if _, ok := recv(t, tm.C()); ok {
		t.Fatal("fired early")
	}
	f.Advance(time.Second)
	v, ok := recv(t, tm.C())
	if !ok || !v.Equal(t0.Add(10*time.Second)) {
		t.Fatalf("got %v %v", v, ok)
	}
	if tm.Stop() {
		t.Error("Stop after firing reported active")
	}
	if tm.Reset(time.Second) {
		t.Error("Reset after firing reported active")
	}
	if !tm.Stop() {
		t.Error("Stop after Reset reported inactive")
	}
	tm.Reset(time.Second)
	tm.Reset(2 * time.Second)
	if f.Waiters() != 1 {
		t.Fatalf("Waiters after double Reset = %d", f.Waiters())
	}
	f.Advance(2 * time.Second)
	if _, ok := recv(t, tm.C()); !ok {
		t.Fatal("reset timer did not fire")
	}
	if f.Waiters() != 0 {
		t.Fatalf("Waiters = %d", f.Waiters())
	}
}

func TestFakeAfterZero(t *testing.T) {
	f := NewFake(t0)
	if _, ok := recv(t, f.After(0)); !ok {
		t.Fatal("After(0) did not fire immediately")
	}
	ch := f.After(time.Minute)
	f.Advance(time.Minute)
	if _, ok := recv(t, ch); !ok {
		t.Fatal("After did not fire")
	}
}

func TestFakeTicker(t *testing.T) {
	f := NewFake(t0)
	tk := f.NewTicker(time.Second)
	for i := 1; i <= 3; i++ {
		f.Advance(time.Second)
		v, ok := recv(t, tk.C())
		if !ok || !v.Equal(t0.Add(time.Duration(i)*time.Second)) {
			t.Fatalf("tick %d: %v %v", i, v, ok)
		}
	}
	f.Advance(5 * time.Second) // slow receiver: extra ticks dropped
	if _, ok := recv(t, tk.C()); !ok {
		t.Fatal("no tick")
	}
	if _, ok := recv(t, tk.C()); ok {
		t.Fatal("ticks were not dropped")
	}
	tk.Reset(10 * time.Second)
	f.Advance(9 * time.Second)
	if _, ok := recv(t, tk.C()); ok {
		t.Fatal("ticked before reset period")
	}
	f.Advance(time.Second)
	if _, ok := recv(t, tk.C()); !ok {
		t.Fatal("no tick after reset")
	}
	tk.Stop()
	f.Advance(time.Hour)
	if _, ok := recv(t, tk.C()); ok {
		t.Fatal("stopped ticker ticked")
	}
}

func TestFakeOrdering(t *testing.T) {
	f := NewFake(t0)
	late := f.NewTimer(2 * time.Second)
	early := f.NewTimer(time.Second)
	f.Advance(3 * time.Second)
	a, _ := recv(t, early.C())
	b, _ := recv(t, late.C())
	if !a.Before(b) {
		t.Fatalf("early %v, late %v", a, b)
	}
}

func TestFakeTickerPanics(t *testing.T) {
	f := NewFake(t0)
	for name, fn := range map[string]func(){
		"new":   func() { f.NewTicker(0) },
		"reset": func() { f.NewTicker(time.Second).Reset(0) },
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("%s: no panic", name)
				}
			}()
			fn()
		}()
	}
}
