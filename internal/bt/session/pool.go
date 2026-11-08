package session

import "sync"

// pool is a fixed set of worker goroutines draining a FIFO of jobs (§4.5:
// the process-wide disk and hash pools). submit never blocks, so a torrent
// actor can hand work off without risking a deadlock with jobs that report
// back to it; the callers bound the amount of queued work.
type pool struct {
	mu     sync.Mutex
	cond   *sync.Cond
	queue  []func()
	head   int
	closed bool
	wg     sync.WaitGroup
}

// newPool starts n workers.
func newPool(n int) *pool {
	p := &pool{}
	p.cond = sync.NewCond(&p.mu)
	for range max(n, 1) {
		p.wg.Add(1)
		go p.worker()
	}
	return p
}

// submit queues f. Jobs submitted after close run on the caller's
// goroutine, so that nothing is lost.
func (p *pool) submit(f func()) {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		f()
		return
	}
	p.queue = append(p.queue, f)
	p.cond.Signal()
	p.mu.Unlock()
}

func (p *pool) worker() {
	defer p.wg.Done()
	for {
		p.mu.Lock()
		for p.head == len(p.queue) && !p.closed {
			p.cond.Wait()
		}
		if p.head == len(p.queue) {
			p.mu.Unlock()
			return
		}
		f := p.queue[p.head]
		p.queue[p.head] = nil
		p.head++
		if p.head == len(p.queue) {
			p.queue, p.head = p.queue[:0], 0
		} else if p.head > 1024 && p.head*2 > len(p.queue) {
			n := copy(p.queue, p.queue[p.head:])
			clear(p.queue[n:])
			p.queue, p.head = p.queue[:n], 0
		}
		p.mu.Unlock()
		f()
	}
}

// close lets the workers finish the queued jobs and waits for them.
func (p *pool) close() {
	p.mu.Lock()
	p.closed = true
	p.cond.Broadcast()
	p.mu.Unlock()
	p.wg.Wait()
}
