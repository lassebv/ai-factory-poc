package storage

import (
	"runtime"
	"sync"
)

// DefaultDiskWorkers is the size of the disk I/O pool (D9).
const DefaultDiskWorkers = 4

// DefaultHashWorkers returns the size of the hashing pool:
// max(1, GOMAXPROCS/2) (D9).
func DefaultHashWorkers() int { return max(1, runtime.GOMAXPROCS(0)/2) }

// Pool runs jobs on a fixed number of goroutines. Submit never blocks: jobs
// queue without bound, so a torrent actor can submit work and later receive
// the results on its own channel without risking a deadlock.
type Pool struct {
	mu     sync.Mutex
	cond   *sync.Cond
	queue  []func()
	closed bool
	wg     sync.WaitGroup
}

// NewPool starts a pool of n workers (at least 1).
func NewPool(n int) *Pool {
	p := &Pool{}
	p.cond = sync.NewCond(&p.mu)
	n = max(1, n)
	p.wg.Add(n)
	for range n {
		go p.work()
	}
	return p
}

func (p *Pool) work() {
	defer p.wg.Done()
	for {
		p.mu.Lock()
		for len(p.queue) == 0 && !p.closed {
			p.cond.Wait()
		}
		if len(p.queue) == 0 {
			p.mu.Unlock()
			return
		}
		job := p.queue[0]
		p.queue[0] = nil
		p.queue = p.queue[1:]
		p.mu.Unlock()
		job()
	}
}

// Submit queues job. It reports false (and drops the job) after Close.
func (p *Pool) Submit(job func()) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return false
	}
	p.queue = append(p.queue, job)
	p.cond.Signal()
	return true
}

// Close stops accepting jobs, runs the queued ones and waits for the
// workers to exit.
func (p *Pool) Close() {
	p.mu.Lock()
	p.closed = true
	p.cond.Broadcast()
	p.mu.Unlock()
	p.wg.Wait()
}
