package halflife

import (
	"fmt"
	"testing"
)

const priceQ = "What is the price of the Acme Pro plan?"

// A miss records which report covers the cache entry it fills; direct and
// semantic hits on that entry get the same report and start no new audit.
func TestCacheHitsResolveToFillingReport(t *testing.T) {
	st := newStack(t, stackOptions{})
	miss := st.ask("vk-a", priceQ)
	st.settle()
	if miss.hit() != "miss" || miss.status() != "queued" || miss.report() == "" || miss.cacheID() == "" {
		t.Fatalf("miss: %s %v", miss.hit(), miss.headers)
	}

	direct := st.ask("vk-a", priceQ)
	semantic := st.ask("vk-a", "What is the price of the Acme Pro plan right now?") // cosine 0.89
	for name, r := range map[string]reply{"direct": direct, "semantic": semantic} {
		if r.hit() != name {
			t.Fatalf("%s: got %s", name, r.hit())
		}
		if r.report() != miss.report() || r.status() != "cached" || r.cacheID() != miss.cacheID() || r.answer != miss.answer {
			t.Errorf("%s: report=%q status=%q cache=%q, want report %q", name, r.report(), r.status(), r.cacheID(), miss.report())
		}
	}
	if st.provider.chats.Load() != 1 {
		t.Errorf("provider calls = %d", st.provider.chats.Load())
	}
	if s := st.hl.Stats(); s.Captured != 1 || s.CachedHits != 2 {
		t.Errorf("stats = %+v", s)
	}
	st.hl.Cleanup() // drain the queue
	if s := st.hl.PoolStats(); s.Enqueued != 1 || s.Audited != 1 {
		t.Errorf("one audit for one answer, got %s", s)
	}
}

// With the cache's identity scoping off (as in this harness, see stack), a
// tenant can hit another tenant's entry. It is audited as a new answer and
// never pointed at the other tenant's report, which keeps the entry's link.
func TestCachedHitAcrossTenantsGetsOwnReport(t *testing.T) {
	st := newStack(t, stackOptions{})
	a := st.ask("vk-a", priceQ)
	st.settle()
	b := st.ask("vk-b", priceQ)
	if b.hit() != "direct" || b.cacheID() != a.cacheID() {
		t.Fatalf("tenant b should hit a's entry: %s %q", b.hit(), b.cacheID())
	}
	if b.report() == "" || b.report() == a.report() || b.status() != "queued" {
		t.Errorf("tenant b: report=%q status=%q (a's %q)", b.report(), b.status(), a.report())
	}
	if again := st.ask("vk-a", priceQ); again.report() != a.report() || again.status() != "cached" {
		t.Errorf("tenant a after b: report=%q status=%q", again.report(), again.status())
	}
}

// An entry Halflife has no report for (filled before a restart, or its report
// was dropped) is audited on its first hit, and later hits reuse that report.
func TestCachedHitWithoutLinkStartsReport(t *testing.T) {
	st := newStack(t, stackOptions{})
	st.ask("vk-a", priceQ)
	st.settle()
	st.hl.links = newCacheLinks(maxCacheLinks) // as after a restart

	first := st.ask("vk-a", priceQ)
	second := st.ask("vk-a", priceQ)
	if first.hit() != "direct" || first.status() != "queued" || first.report() == "" {
		t.Fatalf("first hit: %s %v", first.hit(), first.headers)
	}
	if second.report() != first.report() || second.status() != "cached" {
		t.Errorf("second hit: report=%q status=%q, want %q cached", second.report(), second.status(), first.report())
	}
}

// A dropped report must not stay linked: the next hit is audited instead of
// being pointed at a report that will never exist.
func TestDroppedReportIsUnlinked(t *testing.T) {
	st := newStack(t, stackOptions{})
	st.hl.sink = func(*Capture) bool { return false } // queue full
	miss := st.ask("vk-a", priceQ)
	st.settle()
	if miss.report() != "" || st.hl.links.len() != 0 {
		t.Fatalf("dropped miss: report=%q links=%d", miss.report(), st.hl.links.len())
	}
	st.hl.sink = st.hl.pool.submit
	if hit := st.ask("vk-a", priceQ); hit.hit() != "direct" || hit.status() != "queued" || hit.report() == "" {
		t.Errorf("hit after drop: %s %v", hit.hit(), hit.headers)
	}
}

// Without the semantic cache plugin, nothing is linked.
func TestNoCacheNoLinks(t *testing.T) {
	p, got := recordingPlugin(t)
	r := newRequest(t, p)
	r.attempt(chatReq("q"), chatResp("a", "ok"), nil)
	if h := r.finish(200); h[HeaderStatus] != "queued" || (*got)[0].CacheID != "" || p.links.len() != 0 {
		t.Errorf("headers=%v cache=%q links=%d", h, (*got)[0].CacheID, p.links.len())
	}
}

func TestCacheLinksLRU(t *testing.T) {
	l := newCacheLinks(3)
	for i := range 3 {
		if !l.add(fmt.Sprint("c", i), fmt.Sprint("r", i), "vk") {
			t.Fatalf("add c%d refused", i)
		}
	}
	if l.add("c0", "other", "vk") {
		t.Error("relinked an entry: the first answer written is the one it holds")
	}
	l.get("c0")             // c0 is now the most recently used; c1 the least
	l.add("c3", "r3", "vk") // evicts c1
	if _, ok := l.get("c1"); ok {
		t.Error("least recently used link kept")
	}
	for _, id := range []string{"c0", "c2", "c3"} {
		if _, ok := l.get(id); !ok {
			t.Errorf("%s evicted", id)
		}
	}
	if link, _ := l.get("c0"); link.reportID != "r0" || link.tenant != "vk" {
		t.Errorf("c0 = %+v", link)
	}
	l.remove("c0")
	l.remove("missing")
	if _, ok := l.get("c0"); ok || l.len() != 2 {
		t.Errorf("after remove: len=%d", l.len())
	}
}
