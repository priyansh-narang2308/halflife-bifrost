package halflife

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// AuditFunc audits one capture on a worker, off the request path. ctx ends at
// the audit timeout or when shutdown stops waiting; audits must honour it.
type AuditFunc func(ctx context.Context, c *Capture) error

// pool is a fixed-size queue served by a fixed number of workers.
//
// submit never blocks: when the queue is full the capture is dropped and
// counted, so a slow verifier can never slow down requests. Audits run on a
// context derived from the pool, not from the request, because the request's
// context ends as soon as the response is sent.
type pool struct {
	audit   AuditFunc
	timeout time.Duration

	queue  chan *Capture
	mu     sync.RWMutex // guards closed against concurrent submit and close
	closed bool
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	enqueued, dropped, audited, failed, abandoned atomic.Uint64
}

// PoolStats counts captures by what happened to them in the pool.
type PoolStats struct {
	Enqueued   uint64 // accepted into the queue
	Dropped    uint64 // rejected because the queue was full or closed
	Audited    uint64 // audit returned nil
	Failed     uint64 // audit returned an error, timed out or panicked
	Abandoned  uint64 // still queued when shutdown stopped waiting
	QueueDepth int    // waiting right now
}

func newPool(workers, size int, timeout time.Duration, audit AuditFunc) *pool {
	ctx, cancel := context.WithCancel(context.Background())
	q := &pool{audit: audit, timeout: timeout, queue: make(chan *Capture, size), ctx: ctx, cancel: cancel}
	q.wg.Add(workers)
	for range workers {
		go q.work()
	}
	return q
}

// submit queues c and reports whether it was accepted.
func (q *pool) submit(c *Capture) bool {
	q.mu.RLock()
	defer q.mu.RUnlock()
	if q.closed {
		q.dropped.Add(1)
		return false
	}
	select {
	case q.queue <- c:
		q.enqueued.Add(1)
		return true
	default:
		q.dropped.Add(1)
		return false
	}
}

func (q *pool) work() {
	defer q.wg.Done()
	for c := range q.queue {
		if q.ctx.Err() != nil {
			q.abandoned.Add(1)
			continue
		}
		q.run(c)
	}
}

func (q *pool) run(c *Capture) {
	ctx, cancel := context.WithTimeout(q.ctx, q.timeout)
	defer cancel()
	defer func() {
		if r := recover(); r != nil {
			q.failed.Add(1)
		}
	}()
	if err := q.audit(ctx, c); err != nil {
		q.failed.Add(1)
		return
	}
	q.audited.Add(1)
}

// close stops accepting captures and lets the workers finish the queue. After
// drain it cancels running audits; anything still queued is abandoned. It
// returns false if workers were still running when it gave up waiting, which
// only happens if an audit ignores its context. Safe to call more than once.
func (q *pool) close(drain time.Duration) bool {
	q.mu.Lock()
	if q.closed {
		q.mu.Unlock()
		return true
	}
	q.closed = true
	close(q.queue)
	q.mu.Unlock()

	done := make(chan struct{})
	go func() { q.wg.Wait(); close(done) }()
	select {
	case <-done:
		q.cancel()
		return true
	case <-time.After(drain):
	}
	q.cancel()
	select {
	case <-done:
		return true
	case <-time.After(time.Second):
		return false
	}
}

func (q *pool) stats() PoolStats {
	return PoolStats{
		Enqueued:   q.enqueued.Load(),
		Dropped:    q.dropped.Load(),
		Audited:    q.audited.Load(),
		Failed:     q.failed.Load(),
		Abandoned:  q.abandoned.Load(),
		QueueDepth: len(q.queue),
	}
}

func (s PoolStats) String() string {
	return fmt.Sprintf("enqueued=%d dropped=%d audited=%d failed=%d abandoned=%d queued=%d",
		s.Enqueued, s.Dropped, s.Audited, s.Failed, s.Abandoned, s.QueueDepth)
}
