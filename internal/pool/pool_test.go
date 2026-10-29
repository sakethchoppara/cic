package pool

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/sakethchoppara/cic/internal/config"
	"github.com/sakethchoppara/cic/internal/runner"
)

// Integration tests: need a Docker daemon and python:3.12-slim.
func setup(t *testing.T) (*runner.Runner, runner.Lang) {
	t.Helper()
	langs, err := config.Load("../../config/languages.yaml")
	if err != nil {
		t.Fatal(err)
	}
	r, err := runner.New()
	if err != nil {
		t.Skip("docker unavailable:", err)
	}
	t.Cleanup(func() { r.Close() })
	py := langs["python"]
	if err := r.Check(context.Background(), py); err != nil {
		t.Skip("python image unusable:", err)
	}
	return r, py
}

func waitIdle(t *testing.T, p *Pool, n int) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for p.Stats().Idle < n {
		if time.Now().After(deadline) {
			t.Fatalf("pool never reached %d idle: %+v", n, p.Stats())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// closeAndCheck closes p and fails if any of r's containers survive.
func closeAndCheck(t *testing.T, r *runner.Runner, p *Pool) {
	t.Helper()
	p.Close()
	if n, err := r.Containers(context.Background()); err != nil || n != 0 {
		t.Fatalf("%d containers left after Close (err %v)", n, err)
	}
}

func TestWarmRunAndRefill(t *testing.T) {
	r, py := setup(t)
	p := New(r, "python", py, 2, 5*time.Second)
	waitIdle(t, p, 2)

	res, err := p.Execute(context.Background(), "print(1)", "", 5*time.Second, 0)
	if err != nil || res.Status != "ok" || res.Stdout != "1\n" || !res.Warm {
		t.Fatalf("got %+v, %v", res, err)
	}
	waitIdle(t, p, 2) // the used container was replaced
	if s := p.Stats(); s.Busy != 0 || s.Waiting != 0 {
		t.Fatalf("counters not back to zero: %+v", s)
	}
	closeAndCheck(t, r, p)
}

func TestQueueTimeoutFallsBackCold(t *testing.T) {
	r, py := setup(t)
	p := New(r, "python", py, 1, 10*time.Millisecond)
	waitIdle(t, p, 1)

	var wg sync.WaitGroup
	results := make([]runner.Result, 4)
	for i := range results {
		wg.Go(func() {
			res, err := p.Execute(context.Background(), "print(1)", "", 5*time.Second, 0)
			if err != nil {
				t.Error(err)
			}
			results[i] = res
		})
	}
	wg.Wait()

	warm := 0
	for _, res := range results {
		if res.Status != "ok" {
			t.Fatalf("got %+v", res)
		}
		if res.Warm {
			warm++
		}
	}
	// One idle container, a 10ms queue timeout, and ~300ms to start a replacement.
	if warm != 1 {
		t.Fatalf("%d warm runs, want 1", warm)
	}
	closeAndCheck(t, r, p)
}

func TestCancelWhileQueued(t *testing.T) {
	r, py := setup(t)
	p := New(r, "python", py, 0, time.Minute) // nothing idle, so Execute queues
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err := p.Execute(ctx, "print(1)", "", 5*time.Second, 0)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v, want deadline exceeded", err)
	}
	if s := p.Stats(); s.Waiting != 0 || s.Busy != 0 {
		t.Fatalf("counters not back to zero: %+v", s)
	}
	closeAndCheck(t, r, p)
}

func TestCustomMemoryOnWarmContainer(t *testing.T) {
	r, py := setup(t)
	p := New(r, "python", py, 1, 5*time.Second)
	waitIdle(t, p, 1)
	// 100 MB fits the 256 MB default but not the requested 64 MB.
	res, err := p.Execute(context.Background(), "x = bytearray(100<<20)", "", 5*time.Second, 64)
	if err != nil || res.Status != "memory_exceeded" || !res.Warm {
		t.Fatalf("got %+v, %v", res, err)
	}
	closeAndCheck(t, r, p)
}
