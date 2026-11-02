package halflife

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func capture(i int) *Capture { return &Capture{ReportID: fmt.Sprint("r", i)} }

func TestPoolAuditsEverySubmission(t *testing.T) {
	var mu sync.Mutex
	seen := map[string]int{}
	q := newPool(4, 128, time.Second, func(_ context.Context, c *Capture) error {
		mu.Lock()
		seen[c.ReportID]++
		mu.Unlock()
		return nil
	})
	for i := range 100 {
		if !q.submit(capture(i)) {
			t.Fatalf("submit %d rejected", i)
		}
	}
	if !q.close(time.Second) {
		t.Fatal("close timed out")
	}
	if len(seen) != 100 {
		t.Errorf("audited %d distinct captures, want 100", len(seen))
	}
	for id, n := range seen {
		if n != 1 {
			t.Errorf("%s audited %d times", id, n)
		}
	}
	if s := q.stats(); s != (PoolStats{Enqueued: 100, Audited: 100}) {
		t.Errorf("stats = %+v", s)
	}
}

// With every worker busy and the queue full, submit returns false at once
// instead of waiting: the request path never blocks on auditing.
func TestPoolDropsWhenFullWithoutBlocking(t *testing.T) {
	gate := make(chan struct{})
	started := make(chan struct{}, 1)
	q := newPool(1, 2, time.Minute, func(ctx context.Context, _ *Capture) error {
		select { // signal without blocking: only the first audit is waited on
		case started <- struct{}{}:
		default:
		}
		select {
		case <-gate:
		case <-ctx.Done():
		}
		return nil
	})
	q.submit(capture(0))
	<-started // worker holds capture 0
	if !q.submit(capture(1)) || !q.submit(capture(2)) {
		t.Fatal("queue rejected before it was full")
	}
	begin := time.Now()
	for i := 3; i < 1003; i++ {
		if q.submit(capture(i)) {
			t.Fatalf("submit %d accepted into a full queue", i)
		}
	}
	if d := time.Since(begin); d > 100*time.Millisecond {
		t.Errorf("1000 rejected submits took %v", d)
	}
	if s := q.stats(); s.Dropped != 1000 || s.QueueDepth != 2 {
		t.Errorf("stats = %+v", s)
	}
	close(gate)
	q.close(time.Second)
	if s := q.stats(); s.Audited != 3 || s.Dropped != 1000 {
		t.Errorf("after close: %+v", s)
	}
}

// Captures still queued at shutdown are audited if the drain timeout allows.
func TestPoolDrainsQueueOnClose(t *testing.T) {
	gate := make(chan struct{})
	var audited atomic.Int32
	q := newPool(1, 10, time.Minute, func(context.Context, *Capture) error {
		<-gate
		audited.Add(1)
		return nil
	})
	for i := range 10 {
		q.submit(capture(i))
	}
	go func() { time.Sleep(20 * time.Millisecond); close(gate) }()
	if !q.close(5 * time.Second) {
		t.Fatal("close timed out")
	}
	if audited.Load() != 10 || q.stats().Abandoned != 0 {
		t.Errorf("audited=%d stats=%+v", audited.Load(), q.stats())
	}
}

// Past the drain timeout, the running audit is cancelled and the rest of the
// queue is abandoned, so shutdown is bounded.
func TestPoolDrainTimeoutCancelsAndAbandons(t *testing.T) {
	var cancelled atomic.Bool
	q := newPool(1, 10, time.Minute, func(ctx context.Context, _ *Capture) error {
		<-ctx.Done()
		cancelled.Store(true)
		return ctx.Err()
	})
	for i := range 5 {
		q.submit(capture(i))
	}
	begin := time.Now()
	if !q.close(50 * time.Millisecond) {
		t.Fatal("workers did not stop after cancel")
	}
	if d := time.Since(begin); d > 500*time.Millisecond {
		t.Errorf("close took %v", d)
	}
	s := q.stats()
	if !cancelled.Load() || s.Failed != 1 || s.Abandoned != 4 || s.Audited != 0 {
		t.Errorf("cancelled=%t stats=%+v", cancelled.Load(), s)
	}
}

// An audit that ignores cancellation can't hang shutdown forever.
func TestPoolCloseGivesUpOnStuckAudit(t *testing.T) {
	stuck := make(chan struct{})
	defer close(stuck)
	q := newPool(1, 1, time.Minute, func(context.Context, *Capture) error { <-stuck; return nil })
	q.submit(capture(0))
	time.Sleep(10 * time.Millisecond)
	begin := time.Now()
	if q.close(10 * time.Millisecond) {
		t.Error("close reported a clean stop with an audit still running")
	}
	if d := time.Since(begin); d > 2*time.Second {
		t.Errorf("close took %v", d)
	}
}

func TestPoolAuditTimeoutAndErrors(t *testing.T) {
	q := newPool(1, 10, 20*time.Millisecond, func(ctx context.Context, c *Capture) error {
		switch c.ReportID {
		case "r0":
			<-ctx.Done() // runs into the audit timeout
			if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
				t.Errorf("ctx err = %v", ctx.Err())
			}
			return ctx.Err()
		case "r1":
			return errors.New("verifier failed")
		case "r2":
			panic("verifier bug")
		}
		return nil
	})
	for i := range 4 {
		q.submit(capture(i))
	}
	q.close(time.Second)
	if s := q.stats(); s.Failed != 3 || s.Audited != 1 {
		t.Errorf("stats = %+v (a panic must not kill the worker)", s)
	}
}

// A capture carries no request context, so an audit runs on the pool's own
// context: live when it starts, bounded by the audit timeout.
func TestPoolAuditContextHasOwnDeadline(t *testing.T) {
	errs := make(chan error, 1)
	q := newPool(1, 1, time.Minute, func(ctx context.Context, _ *Capture) error {
		dl, ok := ctx.Deadline()
		if ctx.Err() != nil || !ok || time.Until(dl) < 50*time.Second {
			errs <- fmt.Errorf("audit ctx err=%v deadline=%v", ctx.Err(), dl)
		}
		close(errs)
		return nil
	})
	q.submit(capture(0))
	q.close(time.Second)
	if err := <-errs; err != nil {
		t.Error(err)
	}
}

func TestPoolSubmitAfterCloseAndConcurrentClose(t *testing.T) {
	q := newPool(2, 64, time.Second, func(context.Context, *Capture) error { return nil })
	var wg sync.WaitGroup
	var accepted atomic.Uint64
	for g := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 200 {
				if q.submit(capture(g*1000 + i)) {
					accepted.Add(1)
				}
			}
		}()
	}
	for range 3 {
		wg.Add(1)
		go func() { defer wg.Done(); q.close(time.Second) }()
	}
	wg.Wait()
	if q.submit(capture(-1)) {
		t.Error("submit accepted after close")
	}
	s := q.stats()
	if s.Enqueued != accepted.Load() || s.Enqueued+s.Dropped != 1601 || s.Audited+s.Abandoned != s.Enqueued {
		t.Errorf("stats = %+v accepted=%d", s, accepted.Load())
	}
}

// Through the plugin: a captured request reaches a worker, and a full queue
// withdraws the report header because no report will exist.
func TestPluginQueuesCapturesForAudit(t *testing.T) {
	p, err := Init(context.Background(), &Config{QueueSize: 1, Workers: 1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	gate := make(chan struct{})
	audited := make(chan string, 10)
	p.pool.close(time.Second)
	p.pool = newPool(1, 1, time.Minute, func(ctx context.Context, c *Capture) error {
		<-gate
		audited <- c.ReportID
		return nil
	})
	p.sink = p.pool.submit

	headers := make([]map[string]string, 3)
	for i := range headers { // 1 runs, 1 waits in the queue, 1 is dropped
		r := newRequest(t, p)
		r.attempt(chatReq("q"), chatResp("a", "ok"), nil)
		headers[i] = r.finish(200)
		if i == 0 {
			for p.pool.stats().QueueDepth != 0 {
				time.Sleep(time.Millisecond)
			}
		}
	}
	if headers[0][HeaderReport] == "" || headers[1][HeaderReport] == "" || headers[2][HeaderReport] != "" {
		t.Errorf("report headers = %q %q %q", headers[0][HeaderReport], headers[1][HeaderReport], headers[2][HeaderReport])
	}
	if headers[1][HeaderStatus] != "queued" || headers[2][HeaderStatus] != "not-audited; reason=queue-full" {
		t.Errorf("status headers = %q %q", headers[1][HeaderStatus], headers[2][HeaderStatus])
	}
	close(gate)
	if err := p.Cleanup(); err != nil {
		t.Fatal(err)
	}
	close(audited)
	var ids []string
	for id := range audited {
		ids = append(ids, id)
	}
	if len(ids) != 2 || ids[0] != headers[0][HeaderReport] || ids[1] != headers[1][HeaderReport] {
		t.Errorf("audited %v", ids)
	}
	if s := p.PoolStats(); s.Enqueued != 2 || s.Dropped != 1 || s.Audited != 2 || p.Stats().Captured != 3 {
		t.Errorf("pool=%+v plugin=%+v", s, p.Stats())
	}
}
