package queue

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

const wait = 2 * time.Second

func recv[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(wait):
		t.Fatal("timed out waiting")
		panic("unreachable")
	}
}

// blocker is a task that reports its start and runs until released or cancelled.
type blocker struct {
	started chan struct{}
	release chan struct{}
	cause   chan error
}

func newBlocker() *blocker {
	return &blocker{started: make(chan struct{}), release: make(chan struct{}), cause: make(chan error, 1)}
}

func (b *blocker) run(ctx context.Context) {
	close(b.started)
	select {
	case <-b.release:
		b.cause <- nil
	case <-ctx.Done():
		b.cause <- context.Cause(ctx)
	}
}

func TestOrderAndPosition(t *testing.T) {
	q := New(Config{Workers: 1, Size: 10, PerUser: 1})
	defer q.Shutdown(context.Background())

	first := newBlocker()
	t1, err := q.Submit(1, first.run)
	if err != nil {
		t.Fatal(err)
	}
	recv(t, first.started)

	var mu sync.Mutex
	var order []int64
	record := func(id int64) Func {
		return func(context.Context) {
			mu.Lock()
			order = append(order, id)
			mu.Unlock()
		}
	}
	t2, _ := q.Submit(2, record(2))
	t3, _ := q.Submit(3, record(3))

	if got := []int{t1.Position(), t2.Position(), t3.Position()}; got[0] != 0 || got[1] != 1 || got[2] != 2 {
		t.Fatalf("positions = %v, want [0 1 2]", got)
	}
	if s := q.Stats(); s.Running != 1 || s.Waiting != 2 {
		t.Fatalf("stats = %+v", s)
	}

	close(first.release)
	q.Shutdown(context.Background())
	// Shutdown drops waiting tasks, but both still get their single call.
	if len(order) != 2 {
		t.Fatalf("order = %v, want two calls", order)
	}
}

func TestLimits(t *testing.T) {
	q := New(Config{Workers: 1, Size: 1, PerUser: 1})
	defer q.Shutdown(context.Background())

	running := newBlocker()
	if _, err := q.Submit(1, running.run); err != nil {
		t.Fatal(err)
	}
	recv(t, running.started)

	if _, err := q.Submit(1, func(context.Context) {}); !errors.Is(err, ErrUserBusy) {
		t.Fatalf("second task of the same user: %v, want ErrUserBusy", err)
	}
	if _, err := q.Submit(2, func(context.Context) {}); err != nil {
		t.Fatal(err)
	}
	if _, err := q.Submit(3, func(context.Context) {}); !errors.Is(err, ErrFull) {
		t.Fatalf("task over the queue size: %v, want ErrFull", err)
	}

	close(running.release)
	recv(t, running.cause)
	// The slot of user 1 is free again once the task is done.
	deadline := time.Now().Add(wait)
	for {
		_, err := q.Submit(1, func(context.Context) {})
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("user slot was not released: %v", err)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestCancelRunning(t *testing.T) {
	q := New(Config{Workers: 1, Size: 10, PerUser: 1})
	defer q.Shutdown(context.Background())

	b := newBlocker()
	task, _ := q.Submit(1, b.run)
	recv(t, b.started)
	task.Cancel()
	if cause := recv(t, b.cause); !errors.Is(cause, ErrCancelled) {
		t.Fatalf("cause = %v, want ErrCancelled", cause)
	}
}

func TestCancelWaiting(t *testing.T) {
	q := New(Config{Workers: 1, Size: 10, PerUser: 1})
	defer q.Shutdown(context.Background())

	running := newBlocker()
	q.Submit(1, running.run)
	recv(t, running.started)

	cause := make(chan error, 1)
	waiting, _ := q.Submit(2, func(ctx context.Context) { cause <- context.Cause(ctx) })
	third, _ := q.Submit(3, func(context.Context) {})
	waiting.Cancel()

	if got := recv(t, cause); !errors.Is(got, ErrCancelled) {
		t.Fatalf("cause = %v, want ErrCancelled", got)
	}
	if pos := third.Position(); pos != 1 {
		t.Fatalf("position after cancel = %d, want 1", pos)
	}
	close(running.release)
}

func TestTimeout(t *testing.T) {
	q := New(Config{Workers: 1, Size: 10, PerUser: 1, TaskLimit: 20 * time.Millisecond})
	defer q.Shutdown(context.Background())

	b := newBlocker()
	q.Submit(1, b.run)
	if cause := recv(t, b.cause); !errors.Is(cause, ErrTimeout) {
		t.Fatalf("cause = %v, want ErrTimeout", cause)
	}
}

func TestShutdown(t *testing.T) {
	q := New(Config{Workers: 1, Size: 10, PerUser: 1})

	running := newBlocker()
	q.Submit(1, running.run)
	recv(t, running.started)
	waitingCause := make(chan error, 1)
	q.Submit(2, func(ctx context.Context) { waitingCause <- context.Cause(ctx) })

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	q.Shutdown(ctx)

	if cause := recv(t, waitingCause); !errors.Is(cause, ErrShutdown) {
		t.Fatalf("waiting task cause = %v, want ErrShutdown", cause)
	}
	// The running task did not finish in time and was cancelled.
	if cause := recv(t, running.cause); !errors.Is(cause, ErrShutdown) {
		t.Fatalf("running task cause = %v, want ErrShutdown", cause)
	}
	if _, err := q.Submit(3, func(context.Context) {}); !errors.Is(err, ErrClosed) {
		t.Fatalf("submit after shutdown: %v, want ErrClosed", err)
	}
}
