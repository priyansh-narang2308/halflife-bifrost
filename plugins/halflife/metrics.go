package halflife

import (
	"fmt"
	"io"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Operator attribution and metrics (Task 38). Every completed report is
// folded into per-tenant usage: audits by outcome, verdicts, claims checked,
// searches, Tavily credits and their USD, verifier LLM calls, tokens and the
// USD the gateway reported for them, and failures. The same numbers are
// served as JSON (GET /api/halflife/usage) and in Prometheus text format
// (GET /api/halflife/metrics).
//
// Counters live in memory and start at zero with the process, as Prometheus
// counters do; with report_store_path set, the claims table keeps the same
// facts per claim for queries over any period.
//
// USD: Tavily spend is credits × tavily_credit_usd (the price the relay
// charges the verifier key should be the same). LLM spend is what the
// gateway reports in each reply's usage.cost; providers that report no cost
// (free models, most mocks) count as 0, and the token counts stay exact.

// maxModelSeries caps the distinct provider/model label pairs; answers from
// further models are counted under model "other".
const maxModelSeries = 500

// TenantUsage is one tenant's verification since the process started.
type TenantUsage struct {
	Tenant           string            `json:"tenant_virtual_key_id"`
	TenantName       string            `json:"tenant_virtual_key_name,omitempty"`
	VerifierKeyID    string            `json:"verifier_virtual_key_id,omitempty"` // the key that paid
	Audits           uint64            `json:"audits"`
	Outcomes         map[string]uint64 `json:"outcomes"`
	Verdicts         map[string]uint64 `json:"verdicts"`       // report verdicts: the worst claim per answer
	ClaimVerdicts    map[string]uint64 `json:"claim_verdicts"` // every claim
	ClaimsChecked    uint64            `json:"claims_checked"`
	Searches         uint64            `json:"searches"`        // relayed searches (reused ones excluded)
	SearchesReused   uint64            `json:"searches_reused"` // served from the search memo: no credits
	Extracts         uint64            `json:"extracts"`
	TavilyCredits    float64           `json:"tavily_credits"`
	TavilyUSD        float64           `json:"tavily_usd"`
	LLMCalls         uint64            `json:"llm_calls"`
	PromptTokens     uint64            `json:"llm_prompt_tokens"`
	CompletionTokens uint64            `json:"llm_completion_tokens"`
	LLMUSD           float64           `json:"llm_usd"`
	Failures         map[string]uint64 `json:"failures"`      // by kind (resilience.go)
	AuditSeconds     float64           `json:"audit_seconds"` // total wall time from capture to completion
}

// TotalUSD is the tenant's verification spend.
func (u TenantUsage) TotalUSD() float64 { return u.TavilyUSD + u.LLMUSD }

type modelKey struct{ provider, model, verdict string }
type claimKey struct{ tenant, category, verdict string }

type metrics struct {
	creditUSD float64
	started   time.Time

	mu      sync.Mutex
	tenants map[string]*TenantUsage
	answers map[modelKey]uint64 // report verdicts by the model that answered
	models  map[[2]string]bool
	claims  map[claimKey]uint64
}

func newMetrics(creditUSD float64) *metrics {
	return &metrics{creditUSD: creditUSD, started: time.Now().UTC(), tenants: make(map[string]*TenantUsage),
		answers: make(map[modelKey]uint64), models: make(map[[2]string]bool), claims: make(map[claimKey]uint64)}
}

// record folds a completed report in.
func (m *metrics) record(r Report) {
	if m == nil || r.Status != ReportComplete {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	u := m.tenantLocked(r.Tenant.VirtualKeyID)
	if r.Tenant.VirtualKeyName != "" {
		u.TenantName = r.Tenant.VirtualKeyName
	}
	u.Audits++
	u.Outcomes[r.Outcome]++
	u.Verdicts[r.Verdict]++
	if r.CompletedAt != nil && !r.CreatedAt.IsZero() {
		u.AuditSeconds += max(r.CompletedAt.Sub(r.CreatedAt).Seconds(), 0)
	}
	m.llmLocked(u, r.Extraction)
	for _, c := range r.Claims {
		if c.Verdict != "" {
			u.ClaimVerdicts[c.Verdict]++
			m.claims[claimKey{r.Tenant.VirtualKeyID, c.Category, c.Verdict}]++
		}
		if c.Status == ClaimChecked {
			u.ClaimsChecked++
		}
		if c.Failure != "" {
			u.Failures[c.Failure]++
		}
		if ev := c.Evidence; ev != nil {
			if ev.VerifierKeyID != "" {
				u.VerifierKeyID = ev.VerifierKeyID
			}
			switch {
			case ev.Reused:
				u.SearchesReused++
			case c.Status == ClaimChecked:
				u.Searches++
			}
			if ev.ExtractedURL != "" {
				u.Extracts++
			}
			u.TavilyCredits += ev.Credits
			u.TavilyUSD += ev.Credits * m.creditUSD
		}
		m.llmLocked(u, c.Judge)
	}
	model := [2]string{r.Answer.Provider, r.Answer.Model}
	if !m.models[model] {
		if len(m.models) >= maxModelSeries {
			model[1] = "other"
		}
		m.models[model] = true
	}
	m.answers[modelKey{model[0], model[1], r.Verdict}]++
}

func (m *metrics) llmLocked(u *TenantUsage, res *LLMResult) {
	if res == nil {
		return
	}
	u.LLMCalls += uint64(res.Calls)
	u.PromptTokens += uint64(max(res.PromptTokens, 0))
	u.CompletionTokens += uint64(max(res.CompletionTokens, 0))
	u.LLMUSD += res.CostUSD
}

func (m *metrics) tenantLocked(id string) *TenantUsage {
	u, ok := m.tenants[id]
	if !ok {
		u = &TenantUsage{Tenant: id, Outcomes: map[string]uint64{}, Verdicts: map[string]uint64{}, ClaimVerdicts: map[string]uint64{}, Failures: map[string]uint64{}}
		m.tenants[id] = u
	}
	return u
}

// usage copies every tenant's usage, sorted by tenant.
func (m *metrics) usage() []TenantUsage {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]TenantUsage, 0, len(m.tenants))
	for _, id := range slices.Sorted(maps.Keys(m.tenants)) {
		u := *m.tenants[id]
		u.Outcomes, u.Verdicts, u.ClaimVerdicts, u.Failures = maps.Clone(u.Outcomes), maps.Clone(u.Verdicts), maps.Clone(u.ClaimVerdicts), maps.Clone(u.Failures)
		out = append(out, u)
	}
	return out
}

// Usage is the JSON body of GET /api/halflife/usage.
type Usage struct {
	Since           time.Time               `json:"since"` // counters start with the process
	Mode            string                  `json:"mode"`
	TavilyCreditUSD float64                 `json:"tavily_credit_usd"`
	Tenants         []TenantUsage           `json:"tenants"`
	Dependencies    map[string]BreakerState `json:"dependencies"`
	Spend           map[string]TenantSpend  `json:"spend"` // tenants with their own spend cap
	Requests        Stats                   `json:"requests"`
	Queue           PoolStats               `json:"queue"`
}

// Usage reports per-tenant verification and spend since the process started.
func (p *Plugin) Usage() Usage {
	return Usage{Since: p.metrics.started, Mode: p.config.Mode, TavilyCreditUSD: p.config.TavilyCreditUSD, Tenants: p.metrics.usage(),
		Dependencies: p.verifier.dependencies(), Spend: p.verifier.spendState(), Requests: p.Stats(), Queue: p.pool.stats()}
}

func (v *verifier) dependencies() map[string]BreakerState {
	return map[string]BreakerState{DependencyTavily: v.tavilyCircuit.state(), DependencyLLM: v.llmCircuit.state()}
}

// WriteMetrics writes every metric in the Prometheus text exposition format
// (version 0.0.4). Series are sorted, so output is stable.
func (p *Plugin) WriteMetrics(w io.Writer) error {
	e := &exposition{w: w}
	s, q := p.Stats(), p.pool.stats()

	e.family("halflife_requests_total", "counter", "Finalized requests by what Halflife did with them.")
	for _, kv := range []struct {
		k string
		v uint64
	}{{"captured", s.Captured}, {"cache_hit", s.CachedHits}, {"skipped_failed", s.SkippedFailed}, {"skipped_streaming", s.SkippedStreaming},
		{"skipped_unsupported", s.SkippedUnsupported}, {"skipped_verifier", s.SkippedVerifier}, {"dropped_queue_full", q.Dropped}} {
		e.sample("halflife_requests_total", labels{"outcome", kv.k}, float64(kv.v))
	}
	e.family("halflife_queue_depth", "gauge", "Captures waiting for audit.")
	e.sample("halflife_queue_depth", nil, float64(q.QueueDepth))
	e.family("halflife_cache_evictions_total", "counter", "Stale answers evicted from the semantic cache.")
	e.sample("halflife_cache_evictions_total", nil, float64(s.Invalidated))
	e.family("halflife_spend_usd", "gauge", "Verification spend in the tenant's current spend window.")
	e.family("halflife_spend_cap_usd", "gauge", "The tenant's spend cap per window.")
	spend := p.verifier.spendState()
	for _, tenant := range slices.Sorted(maps.Keys(spend)) {
		e.sample("halflife_spend_usd", labels{"tenant", tenant}, spend[tenant].SpentUSD)
		e.sample("halflife_spend_cap_usd", labels{"tenant", tenant}, spend[tenant].CapUSD)
	}
	e.family("halflife_cache_expiries_total", "counter", "Answers evicted at their volatility TTL.")
	e.sample("halflife_cache_expiries_total", nil, float64(s.Expired))
	e.family("halflife_tombstone_bypasses_total", "counter", "Requests sent past the cache because their question's answer was found outdated.")
	e.sample("halflife_tombstone_bypasses_total", nil, float64(s.TombstoneBypasses))
	e.family("halflife_tombstone_reuses_total", "counter", "Repeats of an outdated answer given its report instead of a new audit.")
	e.sample("halflife_tombstone_reuses_total", nil, float64(s.TombstoneReuses))

	deps := p.verifier.dependencies()
	e.family("halflife_circuit_open", "gauge", "1 while a dependency's circuit refuses calls (open or half-open).")
	for _, d := range slices.Sorted(maps.Keys(deps)) {
		e.sample("halflife_circuit_open", labels{"dependency", d}, boolFloat(deps[d].State != "closed"))
	}
	e.family("halflife_circuit_opens_total", "counter", "Times a dependency's circuit opened.")
	for _, d := range slices.Sorted(maps.Keys(deps)) {
		e.sample("halflife_circuit_opens_total", labels{"dependency", d}, float64(deps[d].Opens))
	}

	tenants := p.metrics.usage()
	type series struct {
		name, kind, help string
		value            func(TenantUsage) float64
	}
	for _, sr := range []series{
		{"halflife_claims_checked_total", "counter", "Claims searched with evidence recorded.", func(u TenantUsage) float64 { return float64(u.ClaimsChecked) }},
		{"halflife_searches_total", "counter", "Tavily searches relayed (reused ones excluded).", func(u TenantUsage) float64 { return float64(u.Searches) }},
		{"halflife_searches_reused_total", "counter", "Searches served from the search memo, at no cost.", func(u TenantUsage) float64 { return float64(u.SearchesReused) }},
		{"halflife_extracts_total", "counter", "Tavily extracts.", func(u TenantUsage) float64 { return float64(u.Extracts) }},
		{"halflife_tavily_credits_total", "counter", "Tavily credits spent verifying the tenant's answers.", func(u TenantUsage) float64 { return u.TavilyCredits }},
		{"halflife_tavily_usd_total", "counter", "Tavily credits in USD (tavily_credit_usd).", func(u TenantUsage) float64 { return u.TavilyUSD }},
		{"halflife_llm_calls_total", "counter", "Verifier LLM calls through the gateway, retries included.", func(u TenantUsage) float64 { return float64(u.LLMCalls) }},
		{"halflife_llm_usd_total", "counter", "Verifier LLM cost the gateway reported (usage.cost).", func(u TenantUsage) float64 { return u.LLMUSD }},
		{"halflife_audit_seconds_total", "counter", "Wall time from capture to completed report.", func(u TenantUsage) float64 { return u.AuditSeconds }},
	} {
		e.family(sr.name, sr.kind, sr.help)
		for _, u := range tenants {
			e.sample(sr.name, labels{"tenant", u.Tenant}, sr.value(u))
		}
	}
	e.family("halflife_llm_tokens_total", "counter", "Verifier LLM tokens.")
	for _, u := range tenants {
		e.sample("halflife_llm_tokens_total", labels{"tenant", u.Tenant, "kind", "prompt"}, float64(u.PromptTokens))
		e.sample("halflife_llm_tokens_total", labels{"tenant", u.Tenant, "kind", "completion"}, float64(u.CompletionTokens))
	}
	for _, f := range []struct {
		name, help, label string
		values            func(TenantUsage) map[string]uint64
	}{
		{"halflife_audits_total", "Completed audits by outcome.", "outcome", func(u TenantUsage) map[string]uint64 { return u.Outcomes }},
		{"halflife_answer_verdicts_total", "Audited answers by verdict (the worst claim's).", "verdict", func(u TenantUsage) map[string]uint64 { return u.Verdicts }},
		{"halflife_failures_total", "Claims left unchecked or unjudged, by failure kind.", "kind", func(u TenantUsage) map[string]uint64 { return u.Failures }},
	} {
		e.family(f.name, "counter", f.help)
		for _, u := range tenants {
			vals := f.values(u)
			for _, k := range slices.Sorted(maps.Keys(vals)) {
				e.sample(f.name, labels{"tenant", u.Tenant, f.label, k}, float64(vals[k]))
			}
		}
	}

	p.metrics.mu.Lock()
	claims, answers := maps.Clone(p.metrics.claims), maps.Clone(p.metrics.answers)
	p.metrics.mu.Unlock()
	e.family("halflife_claim_verdicts_total", "counter", "Claims by tenant, category and verdict.")
	for _, k := range slices.SortedFunc(maps.Keys(claims), func(a, b claimKey) int {
		return strings.Compare(a.tenant+"\x00"+a.category+"\x00"+a.verdict, b.tenant+"\x00"+b.category+"\x00"+b.verdict)
	}) {
		e.sample("halflife_claim_verdicts_total", labels{"tenant", k.tenant, "category", k.category, "verdict", k.verdict}, float64(claims[k]))
	}
	e.family("halflife_model_verdicts_total", "counter", "Audited answers by the provider and model that answered, and verdict.")
	for _, k := range slices.SortedFunc(maps.Keys(answers), func(a, b modelKey) int {
		return strings.Compare(a.provider+"\x00"+a.model+"\x00"+a.verdict, b.provider+"\x00"+b.model+"\x00"+b.verdict)
	}) {
		e.sample("halflife_model_verdicts_total", labels{"provider", k.provider, "model", k.model, "verdict", k.verdict}, float64(answers[k]))
	}
	return e.err
}

// labels are name, value pairs, in order.
type labels []string

type exposition struct {
	w   io.Writer
	err error
}

func (e *exposition) family(name, kind, help string) {
	e.printf("# HELP %s %s\n# TYPE %s %s\n", name, help, name, kind)
}

func (e *exposition) sample(name string, l labels, v float64) {
	var b strings.Builder
	b.WriteString(name)
	if len(l) > 0 {
		b.WriteByte('{')
		for i := 0; i+1 < len(l); i += 2 {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(l[i])
			b.WriteString(`="`)
			b.WriteString(escapeLabel(l[i+1]))
			b.WriteByte('"')
		}
		b.WriteByte('}')
	}
	e.printf("%s %s\n", b.String(), strconv.FormatFloat(v, 'g', -1, 64))
}

func (e *exposition) printf(format string, args ...any) {
	if e.err == nil {
		_, e.err = fmt.Fprintf(e.w, format, args...)
	}
}

// escapeLabel escapes a label value as the text format requires.
func escapeLabel(v string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(v)
}

func boolFloat(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
