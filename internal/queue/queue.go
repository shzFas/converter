// Package queue is an in-memory job queue with a fixed pool of workers,
// per-user limits and cancellation.
package queue

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

var (
	// ErrFull means the queue has no free slots.
	ErrFull = errors.New("queue: full")
	// ErrUserBusy means the user already has the allowed number of tasks.
	ErrUserBusy = errors.New("queue: user has an active task")
	// ErrClosed means the queue is shutting down.
	ErrClosed = errors.New("queue: closed")

	// ErrCancelled is the context cause of a task cancelled by its user.
	ErrCancelled = errors.New("queue: cancelled by user")
	// ErrTimeout is the context cause of a task that ran for too long.
	ErrTimeout = errors.New("queue: task timed out")
	// ErrShutdown is the context cause of a task dropped by Shutdown.
	ErrShutdown = errors.New("queue: shutting down")
)

// Func is the work of one task. It is called exactly once: by a worker, or —
// if the task was cancelled while waiting or the queue is shutting down —
// right away with an already cancelled context. context.Cause tells why.
type Func func(ctx context.Context)

// Config sets the queue limits.
type Config struct {
	Workers   int           // tasks running at once
	Size      int           // tasks waiting at most
	PerUser   int           // waiting + running tasks per user
	TaskLimit time.Duration // time a task may run, 0 for no limit
}

// Task is a submitted unit of work.
type Task struct {
	ID     string
	UserID int64

	q      *Queue
	fn     Func
	ctx    context.Context
	cancel context.CancelCauseFunc
}

// Queue runs tasks in submission order.
type Queue struct {
	cfg Config

	mu      sync.Mutex
	cond    *sync.Cond
	pending []*Task
	running map[*Task]struct{}
	perUser map[int64]int
	closed  bool

	nextID  atomic.Uint64
	workers sync.WaitGroup
	// dropped counts the fn calls made for tasks that never reached a worker.
	dropped sync.WaitGroup
}

// New starts the workers.
func New(cfg Config) *Queue {
	q := &Queue{
		cfg:     cfg,
		running: make(map[*Task]struct{}),
		perUser: make(map[int64]int),
	}
	q.cond = sync.NewCond(&q.mu)
	q.workers.Add(cfg.Workers)
	for range cfg.Workers {
		go q.worker()
	}
	return q
}

// Submit puts a task at the end of the queue.
func (q *Queue) Submit(userID int64, fn Func) (*Task, error) {
	q.mu.Lock()
	defer q.mu.Unlock()

	switch {
	case q.closed:
		return nil, ErrClosed
	case q.cfg.PerUser > 0 && q.perUser[userID] >= q.cfg.PerUser:
		return nil, ErrUserBusy
	case len(q.pending) >= q.cfg.Size:
		return nil, ErrFull
	}

	ctx, cancel := context.WithCancelCause(context.Background())
	t := &Task{
		ID:     strconv.FormatUint(q.nextID.Add(1), 36),
		UserID: userID,
		q:      q,
		fn:     fn,
		ctx:    ctx,
		cancel: cancel,
	}
	q.pending = append(q.pending, t)
	q.perUser[userID]++
	q.cond.Signal()
	return t, nil
}

// Position is the task's 1-based place in the line, 0 once it is running
// or finished.
func (t *Task) Position() int {
	t.q.mu.Lock()
	defer t.q.mu.Unlock()
	for i, p := range t.q.pending {
		if p == t {
			return i + 1
		}
	}
	return 0
}

// Cancel stops the task with ErrCancelled as the cause. A waiting task
// leaves the queue immediately.
func (t *Task) Cancel() {
	t.q.mu.Lock()
	waiting := t.q.removePending(t)
	if waiting {
		t.q.dropped.Add(1)
	}
	t.q.mu.Unlock()

	t.cancel(ErrCancelled)
	if waiting {
		t.q.drop(t)
	}
}

// removePending takes the task out of the line. The caller holds q.mu.
func (q *Queue) removePending(t *Task) bool {
	for i, p := range q.pending {
		if p == t {
			q.pending = append(q.pending[:i], q.pending[i+1:]...)
			return true
		}
	}
	return false
}

// drop finishes a task that never reached a worker. The caller has already
// done q.dropped.Add(1) under q.mu, so that Shutdown cannot miss the task.
func (q *Queue) drop(t *Task) {
	go func() {
		defer q.dropped.Done()
		t.fn(t.ctx)
		q.release(t)
	}()
}

func (q *Queue) release(t *Task) {
	t.cancel(nil)
	q.mu.Lock()
	defer q.mu.Unlock()
	delete(q.running, t)
	if q.perUser[t.UserID]--; q.perUser[t.UserID] <= 0 {
		delete(q.perUser, t.UserID)
	}
}

func (q *Queue) worker() {
	defer q.workers.Done()
	for {
		q.mu.Lock()
		for len(q.pending) == 0 && !q.closed {
			q.cond.Wait()
		}
		if len(q.pending) == 0 {
			q.mu.Unlock()
			return
		}
		t := q.pending[0]
		q.pending = q.pending[1:]
		q.running[t] = struct{}{}
		q.mu.Unlock()

		ctx, cancel := t.ctx, context.CancelFunc(func() {})
		if q.cfg.TaskLimit > 0 {
			ctx, cancel = context.WithTimeoutCause(t.ctx, q.cfg.TaskLimit, ErrTimeout)
		}
		t.fn(ctx)
		cancel()
		q.release(t)
	}
}

// Stats is a snapshot of the queue load.
type Stats struct {
	Running, Waiting int
}

func (q *Queue) Stats() Stats {
	q.mu.Lock()
	defer q.mu.Unlock()
	return Stats{Running: len(q.running), Waiting: len(q.pending)}
}

// Shutdown stops accepting tasks, drops the waiting ones with ErrShutdown
// and waits for the running ones until ctx is done; after that they are
// cancelled with ErrShutdown too. It returns when every task has finished.
func (q *Queue) Shutdown(ctx context.Context) {
	q.mu.Lock()
	q.closed = true
	waiting := q.pending
	q.pending = nil
	q.dropped.Add(len(waiting))
	q.cond.Broadcast()
	q.mu.Unlock()

	for _, t := range waiting {
		t.cancel(ErrShutdown)
		q.drop(t)
	}

	done := make(chan struct{})
	go func() {
		q.workers.Wait()
		q.dropped.Wait()
		close(done)
	}()

	select {
	case <-done:
		return
	case <-ctx.Done():
	}

	q.mu.Lock()
	for t := range q.running {
		t.cancel(ErrShutdown)
	}
	q.mu.Unlock()
	<-done
}
