package halflife

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/plugins/semanticcache"
)

// CacheEvictor deletes one semantic cache entry by its cache ID. The gateway
// wires it to the semantic_cache plugin's ClearCacheForCacheID.
type CacheEvictor func(cacheID string) error

// bypassCacheType is a cache type the semantic cache does not know. Set on a
// request, it makes the cache skip both lookup and write for that request.
const bypassCacheType = semanticcache.CacheType("halflife-bypass")

const evictRetryInterval = time.Second

// invalidator evicts stale answers from the semantic cache.
//
// The race: the cache writes an answer asynchronously, for up to its 30s write
// timeout after the response, and an audit can finish first. A delete before
// the write lands deletes nothing, and the stale answer is then written and
// served. So an eviction within the settle window deletes at once, sends the
// answer's cache partition past the cache (no lookup, no write) until the
// window closes, then deletes again; the bypass is lifted only once that
// final delete succeeds. A delete that fails is retried the same way.
type invalidator struct {
	settle time.Duration
	retry  time.Duration
	logger schemas.Logger

	mu      sync.Mutex
	evict   CacheEvictor
	pending map[string]int // cache partition -> evictions not yet final
	active  atomic.Int32   // len(pending) > 0, read on the request path
	done    chan struct{}
	wg      sync.WaitGroup
	closed  bool

	invalidated, bypassed atomic.Uint64
}

func newInvalidator(settle time.Duration, logger schemas.Logger) *invalidator {
	return &invalidator{settle: settle, retry: evictRetryInterval, logger: logger,
		pending: map[string]int{}, done: make(chan struct{})}
}

func (v *invalidator) setEvictor(e CacheEvictor) {
	v.mu.Lock()
	v.evict = e
	v.mu.Unlock()
}

// SetCacheEvictor connects Halflife to the semantic cache. Without it, stale
// answers are reported but stay cached until their TTL.
func (p *Plugin) SetCacheEvictor(e CacheEvictor) { p.inv.setEvictor(e) }

// invalidate evicts the cache entry behind a stale answer. Hits on it stop
// resolving to its report at once.
func (p *Plugin) invalidate(c *Capture) {
	if c.CacheID == "" {
		return
	}
	p.links.remove(c.CacheID)
	p.inv.invalidate(c.CacheID, c.CacheKey, c.CapturedAt)
}

func (v *invalidator) invalidate(cacheID, partition string, answeredAt time.Time) {
	v.mu.Lock()
	evict := v.evict
	closed := v.closed
	v.mu.Unlock()
	if evict == nil || closed {
		v.logger.Warn("halflife: stale cache entry %s not evicted (no cache evictor)", cacheID)
		return
	}
	v.invalidated.Add(1)
	wait := v.settle - time.Since(answeredAt)
	if wait > 0 {
		// The write may still land: hold the partition off the cache before
		// the first delete, so nothing written after it is ever served.
		v.hold(evict, cacheID, partition, wait)
		if err := evict(cacheID); err != nil {
			v.logger.Warn("halflife: evicting stale cache entry %s failed, retrying: %v", cacheID, err)
		}
		return
	}
	if err := evict(cacheID); err != nil {
		v.logger.Warn("halflife: evicting stale cache entry %s failed, retrying: %v", cacheID, err)
		v.hold(evict, cacheID, partition, v.retry)
		return
	}
	v.logger.Info("halflife: evicted stale cache entry %s", cacheID)
}

// hold makes partition bypass the cache until a delete of cacheID after wait
// succeeds.
func (v *invalidator) hold(evict CacheEvictor, cacheID, partition string, wait time.Duration) {
	v.mu.Lock()
	if v.closed {
		v.mu.Unlock()
		return
	}
	v.pending[partition]++
	v.active.Store(int32(len(v.pending)))
	v.wg.Add(1)
	v.mu.Unlock()
	v.logger.Info("halflife: evicting stale cache entry %s; partition %q bypasses the cache for at least %s",
		cacheID, partition, wait.Round(time.Millisecond))
	go v.finish(evict, cacheID, partition, wait)
}

// finish deletes the entry once the write window has closed, retrying until
// a delete succeeds, then lifts the bypass. On shutdown it stops; the bypass
// dies with the process.
func (v *invalidator) finish(evict CacheEvictor, cacheID, partition string, wait time.Duration) {
	defer v.wg.Done()
	t := time.NewTimer(wait)
	defer t.Stop()
	for {
		select {
		case <-v.done:
			return
		case <-t.C:
		}
		if err := evict(cacheID); err != nil {
			v.logger.Warn("halflife: evicting stale cache entry %s failed, retrying: %v", cacheID, err)
			t.Reset(v.retry)
			continue
		}
		break
	}
	v.mu.Lock()
	if v.pending[partition]--; v.pending[partition] <= 0 {
		delete(v.pending, partition)
	}
	v.active.Store(int32(len(v.pending)))
	v.mu.Unlock()
	v.logger.Info("halflife: evicted stale cache entry %s; partition %q uses the cache again", cacheID, partition)
}

// bypassIfPending sends the request past the semantic cache if its partition
// has an eviction in progress. One atomic load when there is none.
func (v *invalidator) bypassIfPending(ctx *schemas.BifrostContext) {
	if v.active.Load() == 0 {
		return
	}
	partition, _ := ctx.Value(semanticcache.CacheKey).(string)
	v.mu.Lock()
	_, hold := v.pending[partition]
	v.mu.Unlock()
	if hold {
		ctx.SetValue(semanticcache.CacheTypeKey, bypassCacheType)
		v.bypassed.Add(1)
	}
}

// pendingPartitions reports how many partitions bypass the cache.
func (v *invalidator) pendingPartitions() int { return int(v.active.Load()) }

func (v *invalidator) close() {
	v.mu.Lock()
	if !v.closed {
		v.closed = true
		close(v.done)
	}
	v.mu.Unlock()
	v.wg.Wait()
}
