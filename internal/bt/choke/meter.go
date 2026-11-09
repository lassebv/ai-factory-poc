package choke

import (
	"sync"
	"time"

	"torrentd/internal/clock"
)

// slotDur is the granularity of a meter's sliding window.
const slotDur = 250 * time.Millisecond

// numSlots covers MeterWindow plus the current, partial slot.
const numSlots = int(MeterWindow/slotDur) + 1

// meter implements Meter with a ring of per-slot byte counts.
type meter struct {
	clk   clock.Clock
	mu    sync.Mutex
	start time.Time
	total int64
	ids   [numSlots]int64 // slot number stored in each ring position
	bytes [numSlots]int64
}

func newMeter(clk clock.Clock) Meter {
	if clk == nil {
		clk = clock.Real()
	}
	return &meter{clk: clk, start: clk.Now()}
}

func (m *meter) slot(t time.Time) int64 { return int64(t.Sub(m.start) / slotDur) }

// Add records n bytes now.
func (m *meter) Add(n int64) {
	now := m.clk.Now()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.total += n
	id := m.slot(now)
	k := int(id % int64(numSlots))
	if m.ids[k] != id {
		m.ids[k], m.bytes[k] = id, 0
	}
	m.bytes[k] += n
}

// Rate returns bytes/s over the last MeterWindow.
func (m *meter) Rate() int64 {
	now := m.clk.Now()
	m.mu.Lock()
	defer m.mu.Unlock()
	cur := m.slot(now)
	var sum int64
	for k := range m.ids {
		if id := m.ids[k]; id > cur-int64(numSlots) && id <= cur && m.bytes[k] > 0 {
			sum += m.bytes[k]
		}
	}
	span := min(now.Sub(m.start), MeterWindow)
	span = max(span, time.Second)
	return sum * int64(time.Second) / int64(span)
}

// Total returns every byte ever added.
func (m *meter) Total() int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.total
}
