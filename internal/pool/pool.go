// Package pool keeps warm, idle containers per language (FR6).
//
// Invariant: containers in idle + refills owed <= size, so pushes onto idle and
// refill never block. Every container taken from idle owes exactly one refill.
package pool

import (
	"context"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sakethchoppara/cic/internal/runner"
)

type Pool struct {
	r            *runner.Runner
	name         string
	lang         runner.Lang
	queueTimeout time.Duration

	idle   chan string   // started, unused container IDs
	refill chan struct{} // one token per container owed to idle

	cancel context.CancelFunc
	done   sync.WaitGroup

	busy, waiting atomic.Int64
}

// New starts a pool of size idle containers and its refill goroutine.
// Close must be called to stop it and remove the containers.
func New(r *runner.Runner, name string, lang runner.Lang, size int, queueTimeout time.Duration) *Pool {
	ctx, cancel := context.WithCancel(context.Background())
	p := &Pool{
		r: r, name: name, lang: lang, queueTimeout: queueTimeout,
		idle:   make(chan string, size),
		refill: make(chan struct{}, size),
		cancel: cancel,
	}
	for range size {
		p.refill <- struct{}{}
	}
	p.done.Add(1)
	go p.refillLoop(ctx)
	return p
}

// ponytail: one refill goroutine per language starts containers one at a time
// (~300 ms each), so sustained load above ~3 req/s per language runs cold. Add
// workers if the Week 6 benchmark shows the pool draining.
func (p *Pool) refillLoop(ctx context.Context) {
	defer p.done.Done()
	for {
		select {
		case <-ctx.Done():
			return
		case <-p.refill:
		}
		id, err := p.r.Start(ctx, p.lang)
		switch {
		case ctx.Err() != nil:
			if err == nil {
				p.r.Remove(id)
			}
			return
		case err != nil:
			log.Printf("pool %s: refill: %v", p.name, err)
			p.refill <- struct{}{} // still owed; retry after a pause
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Second):
			}
		default:
			p.idle <- id
		}
	}
}

// Execute runs code in a warm container, waiting up to the queue timeout for one
// before falling back to a cold start.
func (p *Pool) Execute(ctx context.Context, code, stdin string, timeout time.Duration, memoryMB int) (runner.Result, error) {
	start := time.Now()
	warm := true
	var id string

	p.waiting.Add(1)
	t := time.NewTimer(p.queueTimeout)
	select {
	case id = <-p.idle:
		p.refill <- struct{}{}
	case <-t.C:
		warm = false
	case <-ctx.Done():
		t.Stop()
		p.waiting.Add(-1)
		return runner.Result{}, ctx.Err()
	}
	t.Stop()
	p.waiting.Add(-1)
	queueMS := time.Since(start).Milliseconds()

	p.busy.Add(1)
	defer p.busy.Add(-1)
	if !warm {
		var err error
		if id, err = p.r.Start(ctx, p.lang); err != nil {
			return runner.Result{}, err
		}
	}
	defer p.r.Remove(id) // destroy after use (FR2), warm or cold
	res, err := p.r.Exec(ctx, id, p.lang, code, stdin, timeout, memoryMB)
	res.QueueMS, res.Warm = queueMS, warm
	return res, err
}

type Stats struct {
	Idle    int   `json:"idle"`
	Busy    int64 `json:"busy"`
	Waiting int64 `json:"waiting"`
}

func (p *Pool) Stats() Stats {
	return Stats{Idle: len(p.idle), Busy: p.busy.Load(), Waiting: p.waiting.Load()}
}

// Close stops refilling and removes idle containers. Call it after in-flight
// Executes have returned (http.Server.Shutdown waits for them).
func (p *Pool) Close() {
	p.cancel()
	p.done.Wait() // refillLoop has exited, so nothing pushes onto idle any more
	for {
		select {
		case id := <-p.idle:
			p.r.Remove(id)
		default:
			return
		}
	}
}
