package halflife

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	bifrost "github.com/maximhq/bifrost/core"
	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/plugins/semanticcache"
)

// staleStack is a stack whose audits wait for release, then find the answer
// stale only if it is the test's stale one; its evictor is the cache's own ClearCacheForCacheID (as the gateway
// wires it), counting calls and failing while fail > 0.
type staleStack struct {
	*stack
	release chan struct{} // audits wait here
	stale   string        // the stale answer, set before release
	evicts  atomic.Int32
	fail    atomic.Int32
}

func newStaleStack(t *testing.T, settle time.Duration) *staleStack {
	st := &staleStack{stack: newStack(t, stackOptions{}), release: make(chan struct{})}
	st.hl.tombs.ttl = 0 // eviction on its own; tombstone_test.go covers both
	st.hl.inv.settle = settle
	st.hl.inv.retry = 10 * time.Millisecond
	st.hl.verify = func(ctx context.Context, c *Capture) (bool, error) {
		select {
		case <-st.release:
			return c.Answer == st.stale, nil
		case <-ctx.Done():
			return false, ctx.Err()
		}
	}
	st.hl.SetCacheEvictor(func(id string) error {
		st.evicts.Add(1)
		if st.fail.Load() > 0 {
			st.fail.Add(-1)
			return errors.New("store unavailable")
		}
		return st.cache.ClearCacheForCacheID(id)
	})
	return st
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// askMany sends n concurrent requests and returns their replies.
func (st *stack) askMany(n int, vk, question string) []reply {
	out := make([]reply, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() { defer wg.Done(); out[i] = st.ask(vk, question) }()
	}
	wg.Wait()
	return out
}

// The race: verification finishes before the cache has written the answer.
// The first delete finds nothing; the write then lands. Repeated and
// concurrent requests must still never get the stale answer: its partition
// bypasses the cache until the delete after the write window succeeds.
func TestStaleAnswerFoundBeforeCacheWrite(t *testing.T) {
	st := newStaleStack(t, 500*time.Millisecond)
	st.store.hold()
	stale := st.ask("vk-a", priceQ)
	st.store.waitQueued(t, 1) // the cache's write is waiting at the gate

	st.stale = stale.answer
	close(st.release) // verification finishes first: stale
	eventually(t, "the first delete", func() bool { return st.evicts.Load() == 1 })
	if st.hl.inv.pendingPartitions() != 1 || st.hl.links.len() != 0 {
		t.Fatalf("after the first delete: pending=%d links=%d", st.hl.inv.pendingPartitions(), st.hl.links.len())
	}

	st.store.open() // the stale write lands after the delete
	st.settle()
	if _, ok := st.store.stored.Load(stale.cacheID()); !ok {
		t.Fatal("the stale write never reached the store")
	}

	// Inside the window nothing is served from this partition's cache; after
	// it, hits on fresh entries are fine. The stale answer never comes back.
	check := func(phase string, rs ...reply) {
		t.Helper()
		inWindow := st.hl.inv.pendingPartitions() > 0
		for _, r := range rs {
			if r.answer == stale.answer || inWindow && (r.hit() == "direct" || r.hit() == "semantic") {
				t.Errorf("%s: served %q (cache %s)", phase, r.answer, r.hit())
			}
		}
	}
	// Inside the window: repeated, paraphrased and concurrent requests.
	before := st.provider.chats.Load()
	check("repeat", st.ask("vk-a", priceQ), st.ask("vk-a", priceQ))
	check("paraphrase", st.ask("vk-a", "What is the price of the Acme Pro plan right now?"))
	check("concurrent", st.askMany(16, "vk-a", priceQ)...)
	if got := st.provider.chats.Load() - before; got != 19 {
		t.Errorf("provider answered %d of 19 requests in the window", got)
	}
	if st.hl.Stats().BypassedRequests != 19 {
		t.Errorf("bypassed = %d", st.hl.Stats().BypassedRequests)
	}
	// Another partition is not held off the cache.
	other := st.askWith("vk-a", priceQ, func(ctx *schemas.BifrostContext) { ctx.SetValue(semanticcache.CacheKey, "other") })
	if other.hit() != "miss" {
		t.Errorf("other partition: %s", other.hit())
	}

	// After the window: the final delete removed the late write.
	eventually(t, "the final delete", func() bool { return st.hl.inv.pendingPartitions() == 0 })
	if st.evicts.Load() != 2 {
		t.Errorf("evictions = %d, want 2", st.evicts.Load())
	}
	after := st.ask("vk-a", priceQ)
	check("after", after)
	check("after, concurrent", st.askMany(16, "vk-a", priceQ)...)
	if after.hit() != "miss" {
		t.Errorf("after the window the stale entry is gone, so a miss; got %s", after.hit())
	}
	if s := st.hl.Stats(); s.Invalidated != 1 {
		t.Errorf("stats = %+v", s)
	}
}

// An answer found stale after its write settled is deleted once, with no
// bypass; the next request misses and gets a fresh answer.
func TestStaleAnswerAfterCacheWrite(t *testing.T) {
	st := newStaleStack(t, 20*time.Millisecond)
	stale := st.ask("vk-a", priceQ)
	st.settle()
	time.Sleep(st.hl.inv.settle) // the write window has closed
	if hit := st.ask("vk-a", priceQ); hit.hit() != "direct" {
		t.Fatalf("before the audit: %s", hit.hit())
	}
	st.stale = stale.answer
	close(st.release)
	eventually(t, "the eviction", func() bool { return st.hl.Stats().Invalidated == 1 })
	eventually(t, "the delete", func() bool { return st.evicts.Load() == 1 })
	if st.hl.inv.pendingPartitions() != 0 {
		t.Error("bypass set for a settled write")
	}
	for _, r := range append(st.askMany(8, "vk-a", priceQ), st.ask("vk-a", priceQ)) {
		if r.answer == stale.answer {
			t.Fatalf("stale answer served after eviction (cache %s)", r.hit())
		}
	}
	if st.hl.Stats().BypassedRequests != 0 {
		t.Errorf("bypassed = %d", st.hl.Stats().BypassedRequests)
	}
}

// A failed delete keeps the partition off the cache until a retry succeeds.
func TestFailedEvictionKeepsBypass(t *testing.T) {
	st := newStaleStack(t, time.Millisecond)
	st.fail.Store(3)
	stale := st.ask("vk-a", priceQ)
	st.settle()
	st.stale = stale.answer
	close(st.release)
	eventually(t, "the first attempt", func() bool { return st.evicts.Load() >= 1 })
	for st.evicts.Load() < 4 { // three failures, then success
		if r := st.ask("vk-a", priceQ); r.answer == stale.answer {
			t.Fatalf("stale answer served while the delete was failing (cache %s)", r.hit())
		}
		time.Sleep(2 * time.Millisecond)
	}
	eventually(t, "the bypass to lift", func() bool { return st.hl.inv.pendingPartitions() == 0 })
	if r := st.ask("vk-a", priceQ); r.answer == stale.answer {
		t.Errorf("stale answer served after the retry (cache %s)", r.hit())
	}
}

// Fresh answers stay cached, and with no evictor wired a stale one is only
// logged (the gateway always wires one; the plugin must not depend on it).
func TestNoEvictionWithoutStaleOrEvictor(t *testing.T) {
	st := newStack(t, stackOptions{})
	st.ask("vk-a", priceQ)
	st.settle()
	st.hl.Cleanup() // the placeholder verifier has found nothing stale
	if hit := st.ask("vk-a", priceQ); hit.hit() != "direct" || st.hl.Stats().Invalidated != 0 {
		t.Errorf("fresh answer: %s, stats %+v", hit.hit(), st.hl.Stats())
	}

	st = newStack(t, stackOptions{})
	st.hl.tombs.ttl = 0
	st.hl.verify = func(context.Context, *Capture) (bool, error) { return true, nil }
	st.ask("vk-a", priceQ)
	st.settle()
	st.hl.Cleanup()
	if hit := st.ask("vk-a", priceQ); hit.hit() != "direct" || st.hl.Stats().Invalidated != 0 {
		t.Errorf("no evictor: %s, stats %+v", hit.hit(), st.hl.Stats())
	}
}

// Shutdown stops pending final deletes instead of waiting out the window.
func TestCleanupStopsPendingEvictions(t *testing.T) {
	st := newStaleStack(t, time.Hour)
	st.stale = st.ask("vk-a", priceQ).answer
	close(st.release)
	eventually(t, "the hold", func() bool { return st.hl.inv.pendingPartitions() == 1 })
	done := make(chan struct{})
	go func() { st.hl.Cleanup(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Cleanup waited for the settle window")
	}
}

// A volatile answer leaves the cache at its claims' TTL, long before the
// cache's own, through the same race-safe eviction.
func TestVolatileAnswerExpires(t *testing.T) {
	st := newStack(t, stackOptions{halflife: &Config{VolatilityTTLSeconds: map[string]int{BandHoursDays: 1}}})
	st.hl.inv.settle = 0
	st.hl.SetCacheEvictor(st.cache.ClearCacheForCacheID)
	st.hl.verify = func(_ context.Context, c *Capture) (bool, error) {
		st.hl.reports.update(c.ReportID, func(r *Report) {
			r.Status, r.Outcome = ReportComplete, OutcomeEvidenceFound
			r.Claims = []ReportClaim{{Status: ClaimChecked, Priority: PriorityHigh, VolatilityBand: BandHoursDays}}
		})
		return false, nil
	}
	st.ask("vk-a", priceQ)
	st.settle()
	if hit := st.ask("vk-a", priceQ); hit.hit() != "direct" {
		t.Fatalf("before the TTL: %s", hit.hit())
	}
	eventually(t, "the expiry", func() bool { return st.hl.Stats().Expired == 1 })
	if hit := st.ask("vk-a", priceQ); hit.hit() != "miss" {
		t.Errorf("after the TTL: %s", hit.hit())
	}
	if s := st.hl.Stats(); s.Invalidated != 0 {
		t.Errorf("an expiry is not an invalidation: %+v", s)
	}
}

// Stale evidence beats a scheduled expiry; shutdown stops the rest.
func TestExpiryCancelled(t *testing.T) {
	p := newPlugin(t)
	var evicted atomic.Int32
	p.SetCacheEvictor(func(string) error { evicted.Add(1); return nil })
	p.inv.settle = 0
	c := &Capture{CacheID: "e", CapturedAt: time.Now()}
	p.expire(c, time.Hour)
	p.expire(c, time.Hour) // scheduled once
	if n := len(p.inv.expiries); n != 1 {
		t.Fatalf("expiries %d", n)
	}
	p.invalidate(c)
	if len(p.inv.expiries) != 0 || evicted.Load() != 1 {
		t.Errorf("expiries %d, evicted %d", len(p.inv.expiries), evicted.Load())
	}
	p.expire(&Capture{CacheID: "other", CapturedAt: time.Now()}, 10*time.Millisecond)
	p.inv.close()
	time.Sleep(30 * time.Millisecond)
	if evicted.Load() != 1 || p.Stats().Expired != 0 {
		t.Errorf("fired after close: %d", evicted.Load())
	}
}

func TestExpiriesBounded(t *testing.T) {
	v := newInvalidator(0, bifrost.NewNoOpLogger())
	defer v.close()
	for i := range maxCacheLinks + 5 {
		v.schedule(fmt.Sprint(i), time.Hour, func() {})
	}
	if n := len(v.expiries); n != maxCacheLinks {
		t.Errorf("expiries %d", n)
	}
}
