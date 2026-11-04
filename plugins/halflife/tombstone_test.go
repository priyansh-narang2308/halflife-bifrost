package halflife

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/plugins/semanticcache"
)

func TestQuestionKeyIgnoresCaseAndSpacing(t *testing.T) {
	if questionKey("p", "How  much is\tPro? ") != questionKey("p", "how much is pro?") {
		t.Error("the same question hashed differently")
	}
	if questionKey("p", "q") == questionKey("other", "q") || questionKey("p", "q") == questionKey("p", "r") {
		t.Error("keys collide across partitions or questions")
	}
}

func TestTombstoneExpiry(t *testing.T) {
	c := newClock()
	ts := newTombstones(time.Hour)
	ts.now = c.now
	key := questionKey("", "q")
	ts.add(key, &Capture{ReportID: "r"})
	if _, ok := ts.get(key); !ok || ts.active.Load() != 1 {
		t.Fatal("not tombstoned")
	}
	c.advance(time.Hour)
	if _, ok := ts.get(key); ok || ts.active.Load() != 0 {
		t.Error("outlived its TTL")
	}
}

// The table is bounded; expired tombstones make room, live ones aren't dropped.
func TestTombstonesBounded(t *testing.T) {
	c := newClock()
	ts := newTombstones(time.Hour)
	ts.now = c.now
	for i := range maxTombstones {
		ts.add(questionKey("", fmt.Sprint(i)), &Capture{})
	}
	over := questionKey("", "over")
	ts.add(over, &Capture{})
	if _, ok := ts.get(over); ok || ts.active.Load() != maxTombstones {
		t.Errorf("past the bound: %d", ts.active.Load())
	}
	ts.add(questionKey("", "0"), &Capture{ReportID: "again"}) // replacing needs no room
	if e, _ := ts.get(questionKey("", "0")); e.reportID != "again" {
		t.Error("existing tombstone not replaced at the bound")
	}
	c.advance(time.Hour)
	ts.add(over, &Capture{})
	if _, ok := ts.get(over); !ok || ts.active.Load() != 1 {
		t.Errorf("expired tombstones not swept: %d", ts.active.Load())
	}
}

// tombStack is a stack whose provider repeats itself, as a model does. Its
// audits find every answer outdated until outdated is cleared.
type tombStack struct {
	*staleStack
	outdated atomic.Bool
	audits   atomic.Int32
}

func newTombStack(t *testing.T) *tombStack {
	t.Helper()
	st := &tombStack{staleStack: newStaleStack(t, 40*time.Millisecond)}
	st.hl.tombs.ttl = time.Minute
	st.provider.setAnswer("Acme Cloud Pro costs $49 per month.")
	st.outdated.Store(true)
	st.hl.verify = func(_ context.Context, c *Capture) (bool, error) {
		st.audits.Add(1)
		st.hl.reports.update(c.ReportID, func(r *Report) {
			r.Status, r.Outcome = ReportComplete, OutcomeEvidenceFound
		})
		return st.outdated.Load(), nil
	}
	return st
}

func (st *tombStack) evicted() {
	st.t.Helper()
	st.settle()
	eventually(st.t, "the eviction", func() bool { return st.hl.Stats().Invalidated == 1 })
	eventually(st.t, "the final delete", func() bool { return st.hl.inv.pendingPartitions() == 0 })
}

// Eviction alone isn't enough: the model repeats itself, and without a
// tombstone the cache would store the outdated answer again.
func TestTombstoneKeepsAnOutdatedAnswerOutOfTheCache(t *testing.T) {
	st := newTombStack(t)
	first := st.ask("vk-a", priceQ)
	st.evicted()

	before := st.provider.chats.Load()
	again := st.ask("vk-a", priceQ)
	st.settle()
	if again.hit() != "none" {
		t.Errorf("a tombstoned question reached the cache: %s", again.hit())
	}
	if st.provider.chats.Load() != before+1 {
		t.Error("the provider did not answer")
	}
	if again.status() != statusKnownOutdated || again.report() != first.report() {
		t.Errorf("repeat: status %s report %s, want %s", again.status(), again.report(), first.report())
	}
	s := st.hl.Stats()
	if s.TombstoneBypasses != 1 || s.TombstoneReuses != 1 || st.audits.Load() != 1 {
		t.Errorf("audits %d, stats %+v", st.audits.Load(), s)
	}
	// Still off the cache: nothing was written by the repeat.
	if after := st.ask("vk-a", priceQ); after.status() != statusKnownOutdated {
		t.Errorf("third ask: %s", after.status())
	}
}

// Another tenant's identical answer is theirs to have audited: it gets no
// other tenant's report, and the cost of sharing a question is one cache miss.
func TestTombstoneStaysWithItsTenant(t *testing.T) {
	st := newTombStack(t)
	first := st.ask("vk-a", priceQ)
	st.evicted()
	other := st.ask("vk-b", priceQ)
	if other.report() == first.report() || other.status() == statusKnownOutdated {
		t.Errorf("tenant b got %s %s", other.status(), other.report())
	}
}

// A new answer checked against evidence with nothing outdated lifts it.
func TestTombstoneLiftedByAPassingAnswer(t *testing.T) {
	st := newTombStack(t)
	st.ask("vk-a", priceQ)
	st.evicted()
	if st.hl.tombs.active.Load() != 1 {
		t.Fatal("no tombstone")
	}

	st.outdated.Store(false)
	st.provider.setAnswer("Acme Cloud Pro costs $59 per month.")
	fresh := st.ask("vk-a", priceQ)
	if fresh.status() == statusKnownOutdated {
		t.Fatalf("a different answer was not audited: %s", fresh.status())
	}
	eventually(t, "the lift", func() bool { return st.hl.tombs.active.Load() == 0 })
	st.settle()
	st.ask("vk-a", priceQ)
	st.settle()
	if hit := st.ask("vk-a", priceQ); hit.hit() != "direct" {
		t.Errorf("the cache did not recover: %s", hit.hit())
	}
}

// A failed audit neither lifts a tombstone nor sets an expiry.
func TestFreshnessIgnoresAFailedAudit(t *testing.T) {
	p := newPlugin(t)
	c := &Capture{ReportID: "r", CacheID: "e", Question: "q", Answer: "a", VirtualKeyID: "vk"}
	p.tombs.add(questionKey("", "q"), c)
	p.reports.put(&Report{ID: "r", Outcome: OutcomeEvidenceFound})
	p.applyFreshness(c, false, false)
	if p.tombs.active.Load() != 1 || len(p.inv.expiries) != 0 {
		t.Errorf("tombstones %d, expiries %d", p.tombs.active.Load(), len(p.inv.expiries))
	}
}

func TestMonitorHasNoFreshnessEffects(t *testing.T) {
	st := newStack(t, stackOptions{halflife: &Config{Mode: ModeMonitor}})
	var evicted int
	st.hl.SetCacheEvictor(func(string) error { evicted++; return nil })
	st.hl.verify = func(context.Context, *Capture) (bool, error) { return true, nil }
	st.ask("vk-a", priceQ)
	st.settle()
	st.hl.Cleanup()
	if s := st.hl.Stats(); evicted != 0 || s.Invalidated != 0 || st.hl.tombs.active.Load() != 0 {
		t.Errorf("monitor touched the cache: evicted %d, stats %+v", evicted, s)
	}
}

func TestBypassOnlyInItsPartition(t *testing.T) {
	ts := newTombstones(time.Minute)
	ts.add(questionKey("team-a", "q"), &Capture{})
	for partition, want := range map[string]bool{"team-a": true, "team-b": false} {
		ctx := newCtx()
		ctx.SetValue(semanticcache.CacheKey, partition)
		ts.bypass(ctx, "q")
		if got := ctx.Value(semanticcache.CacheTypeKey) == bypassCacheType; got != want {
			t.Errorf("%s: bypassed %v", partition, got)
		}
	}
	var none *schemas.BifrostContext // no tombstones: the context isn't touched
	newTombstones(time.Minute).bypass(none, "q")
}

func TestAnswerTTL(t *testing.T) {
	cfg := Config{}
	week, year := ReportClaim{Priority: PriorityHigh, VolatilityBand: BandWeeksMonths}, ReportClaim{Priority: PriorityLow, VolatilityBand: BandYear}
	if ttl, ok := cfg.answerTTL([]ReportClaim{year, week}); !ok || ttl != 7*24*time.Hour {
		t.Errorf("most volatile claim decides: %v %v", ttl, ok)
	}
	if ttl, ok := (Config{VolatilityTTLSeconds: map[string]int{BandWeeksMonths: 60}}).answerTTL([]ReportClaim{year, week}); !ok || ttl != time.Minute {
		t.Errorf("override: %v %v", ttl, ok)
	}
	for name, claims := range map[string][]ReportClaim{
		"no claims": nil,
		"skipped":   {{Priority: PrioritySkip, VolatilityBand: BandHoursDays}},
		"rejected":  {{Priority: PriorityHigh, Status: ClaimRejected, VolatilityBand: BandHoursDays}},
		"no band":   {{Priority: PriorityHigh, VolatilityBand: BandNever}},
	} {
		if ttl, ok := cfg.answerTTL(claims); ok {
			t.Errorf("%s: %v", name, ttl)
		}
	}
	if _, ok := (Config{CacheTTLSeconds: 3600}).answerTTL([]ReportClaim{year, week}); ok {
		t.Error("the cache's own cap comes first")
	}
}

func TestFreshnessConfigValidation(t *testing.T) {
	for name, c := range map[string]Config{
		"unknown band":  {VolatilityTTLSeconds: map[string]int{"SOON": 60}},
		"zero seconds":  {VolatilityTTLSeconds: map[string]int{BandYear: 0}},
		"tombstone ttl": {TombstoneTTLSeconds: -2},
	} {
		if err := c.withDefaults().validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	off := Config{TombstoneTTLSeconds: -1}.withDefaults()
	if err := off.validate(); err != nil {
		t.Error(err)
	}
	ts := newTombstones(secs(off.TombstoneTTLSeconds))
	ts.add(questionKey("", "q"), &Capture{})
	if ts.active.Load() != 0 {
		t.Error("tombstones off, but one was added")
	}
}
