package storage

import (
	"context"
	"runtime"
	"sync"
)

// job states.
const (
	jobQueued = iota
	jobStarted
	jobCancelled
)

// job is one queued fn. done is nil for Submit.
type job struct {
	fn    func()
	state int
	done  chan error
}

// pool implements Pool: a fixed set of workers draining a FIFO queue.
type pool struct {
	workers int
	mu      sync.Mutex
	cond    *sync.Cond
	queue   []*job
	closed  bool
	wg      sync.WaitGroup
}

func newPool(workers int) Pool {
	workers = max(workers, 1)
	p := &pool{workers: workers}
	p.cond = sync.NewCond(&p.mu)
	p.wg.Add(workers)
	for i := 0; i < workers; i++ {
		go p.work()
	}
	return p
}

func defaultHashWorkers() int { return max(1, runtime.GOMAXPROCS(0)/2) }

func (p *pool) work() {
	defer p.wg.Done()
	for {
		p.mu.Lock()
		for len(p.queue) == 0 && !p.closed {
			p.cond.Wait()
		}
		if p.closed {
			p.mu.Unlock()
			return
		}
		j := p.queue[0]
		p.queue[0] = nil
		p.queue = p.queue[1:]
		if j.state == jobCancelled {
			p.mu.Unlock()
			continue
		}
		j.state = jobStarted
		p.mu.Unlock()
		j.fn()
		if j.done != nil {
			j.done <- nil
		}
	}
}

func (p *pool) enqueue(j *job) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return ErrClosed
	}
	p.queue = append(p.queue, j)
	p.cond.Signal()
	return nil
}

// Run queues fn and waits until it has run.
func (p *pool) Run(ctx context.Context, fn func()) error {
	j := &job{fn: fn, done: make(chan error, 1)}
	if err := p.enqueue(j); err != nil {
		return err
	}
	select {
	case err := <-j.done:
		return err
	case <-ctx.Done():
	}
	p.mu.Lock()
	if j.state == jobQueued {
		j.state = jobCancelled
		p.mu.Unlock()
		return ctx.Err()
	}
	p.mu.Unlock()
	return <-j.done // it started: wait for it
}

// Submit queues fn and returns at once.
func (p *pool) Submit(fn func()) error { return p.enqueue(&job{fn: fn}) }

// Workers returns the number of workers.
func (p *pool) Workers() int { return p.workers }

// Close drops queued work and waits for running work and the workers.
func (p *pool) Close() {
	p.mu.Lock()
	if !p.closed {
		p.closed = true
		for _, j := range p.queue {
			if j.state == jobQueued {
				j.state = jobCancelled
				if j.done != nil {
					j.done <- ErrClosed
				}
			}
		}
		p.queue = nil
		p.cond.Broadcast()
	}
	p.mu.Unlock()
	p.wg.Wait()
}
