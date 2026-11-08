package choke

import (
	"testing"
	"time"
)

func TestChokerDefaultsAndOptimisticLeaves(t *testing.T) {
	c := newChoker(nil)
	if _, ok := c.Optimistic(); ok {
		t.Fatal("no optimistic peer yet")
	}
	peers := leechers(5)
	got := c.Rechoke(t0, peers, true)
	if len(got) != 5 {
		t.Fatalf("got %v", got)
	}
	opt, _ := c.Optimistic()
	for i := range peers {
		if peers[i].Key == opt {
			peers[i].Interested = false
		}
	}
	got = c.Rechoke(t0.Add(time.Second), peers, true)
	if len(got) != 4 {
		t.Fatalf("after the optimistic peer left: %v", got)
	}
	if NewMeter(nil).Rate() != 0 {
		t.Fatal("meter")
	}
}
