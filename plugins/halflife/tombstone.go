package halflife

import (
	"crypto/sha256"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/plugins/semanticcache"
)

// Tombstones. Evicting an outdated answer is not enough: the model that wrote
// it still believes it, so the next request regenerates it and the cache
// stores it again. A tombstone sends that question past the cache (no lookup,
// no write) for tombstone_ttl_seconds, and an identical answer in that time
// reuses its report instead of paying for the same audit. A different answer
// is audited as usual; if it is checked against evidence and nothing in it is
// outdated, the tombstone is lifted.
//
// Keys are the cache partition and the normalised question, so a tombstone
// can only cost another tenant asking the same question a cache miss; its
// report is never shown to them.

const maxTombstones = 10000

type tombstoneKey [sha256.Size]byte

// questionKey identifies a question within a cache partition: case and
// spacing don't matter, as for a person asking it again.
func questionKey(partition, question string) tombstoneKey {
	return sha256.Sum256([]byte(partition + "\x00" + strings.Join(strings.Fields(strings.ToLower(question)), " ")))
}

type tombstone struct {
	expires  time.Time
	reportID string
	tenant   string
	answer   [sha256.Size]byte
}

type tombstones struct {
	ttl time.Duration
	now func() time.Time

	mu     sync.Mutex
	byKey  map[tombstoneKey]tombstone
	active atomic.Int32 // len(byKey), read on the request path

	bypassed, reused atomic.Uint64
}

func newTombstones(ttl time.Duration) *tombstones {
	return &tombstones{ttl: ttl, now: time.Now, byKey: make(map[tombstoneKey]tombstone)}
}

// add tombstones the question, replacing any earlier one. When the table is
// full of live tombstones the new one is dropped: the answer was still evicted.
func (t *tombstones) add(key tombstoneKey, c *Capture) {
	if t.ttl <= 0 {
		return
	}
	now := t.now()
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, ok := t.byKey[key]; !ok && len(t.byKey) >= maxTombstones {
		t.sweepLocked(now)
		if len(t.byKey) >= maxTombstones {
			return
		}
	}
	t.byKey[key] = tombstone{expires: now.Add(t.ttl), reportID: c.ReportID, tenant: c.VirtualKeyID, answer: sha256.Sum256([]byte(c.Answer))}
	t.active.Store(int32(len(t.byKey)))
}

func (t *tombstones) sweepLocked(now time.Time) {
	for k, e := range t.byKey {
		if !now.Before(e.expires) {
			delete(t.byKey, k)
		}
	}
	t.active.Store(int32(len(t.byKey)))
}

func (t *tombstones) get(key tombstoneKey) (tombstone, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	e, ok := t.byKey[key]
	if ok && !t.now().Before(e.expires) {
		delete(t.byKey, key)
		t.active.Store(int32(len(t.byKey)))
		return tombstone{}, false
	}
	return e, ok
}

func (t *tombstones) lift(key tombstoneKey) {
	t.mu.Lock()
	delete(t.byKey, key)
	t.active.Store(int32(len(t.byKey)))
	t.mu.Unlock()
}

// bypass sends a tombstoned question past the cache. One atomic load when
// there are no tombstones.
func (t *tombstones) bypass(ctx *schemas.BifrostContext, question string) {
	if t.active.Load() == 0 {
		return
	}
	partition, _ := ctx.Value(semanticcache.CacheKey).(string)
	if _, ok := t.get(questionKey(partition, question)); ok {
		ctx.SetValue(semanticcache.CacheTypeKey, bypassCacheType)
		t.bypassed.Add(1)
	}
}

// resolveTombstoned gives a repeat of a tombstoned answer its existing report
// instead of a new audit. Tenants never share a report.
func (p *Plugin) resolveTombstoned(s *slot, c *Capture) bool {
	if p.tombs.active.Load() == 0 {
		return false
	}
	e, ok := p.tombs.get(questionKey(c.CacheKey, c.Question))
	if !ok || e.tenant != c.VirtualKeyID || e.answer != sha256.Sum256([]byte(c.Answer)) {
		return false
	}
	verdict := ""
	if s.protect {
		verdict = VerdictOutdated
	}
	s.setReport(e.reportID, false)
	s.setProtected(statusKnownOutdated, verdict)
	p.tombs.reused.Add(1)
	return true
}

// applyFreshness acts on a finished audit: an outdated answer is evicted and
// its question tombstoned; any other gets an expiry from its claims' volatility.
func (p *Plugin) applyFreshness(c *Capture, stale, finished bool) {
	key := questionKey(c.CacheKey, c.Question)
	if stale {
		// Tombstoned even with no entry to evict: a request sent past the
		// cache by another eviction would otherwise write the answer later.
		p.invalidate(c)
		p.tombs.add(key, c)
		return
	}
	if !finished {
		return
	}
	r, ok := p.reports.get(c.ReportID)
	if !ok {
		return
	}
	if r.Outcome == OutcomeEvidenceFound {
		p.tombs.lift(key)
	}
	if ttl, ok := p.config.answerTTL(r.Claims); ok && c.CacheID != "" {
		p.expire(c, ttl)
	}
}
