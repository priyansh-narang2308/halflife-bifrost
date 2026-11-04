package halflife

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// meteredReport is a completed report for tenant vk-a: two checked claims
// (one reused search, one extracted page), one unchecked, and LLM calls.
func meteredReport(id, provider, model string) Report {
	created := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	done := created.Add(1500 * time.Millisecond)
	r := Report{ID: id, Status: ReportComplete, Outcome: OutcomeEvidenceFound, Verdict: VerdictContradicted,
		CreatedAt: created, CompletedAt: &done,
		Tenant:     ReportTenant{VirtualKeyID: "vk-a", VirtualKeyName: "Acme"},
		Answer:     ReportAnswer{Provider: provider, Model: model},
		Extraction: &LLMResult{Calls: 1, PromptTokens: 100, CompletionTokens: 20, CostUSD: 0.001},
		Claims: []ReportClaim{
			{Category: CategoryPrice, Status: ClaimChecked, Verdict: VerdictContradicted,
				Evidence: &ReportSources{VerifierKeyID: "vk-verifier", Credits: 2, ExtractedURL: "https://acme.example/pricing"},
				Judge:    &LLMResult{Calls: 2, PromptTokens: 300, CompletionTokens: 40, CostUSD: 0.002}},
			{Category: CategoryPrice, Status: ClaimChecked, Verdict: VerdictCurrent, Evidence: &ReportSources{Reused: true}},
			{Category: CategoryVersion, Status: ClaimNotChecked, Failure: FailureCircuitOpen},
		}}
	return r
}

func TestMetricsRecordUsage(t *testing.T) {
	m := newMetrics(0.008)
	m.record(meteredReport("hlr_1", "openai", "gpt-4o"))
	m.record(Report{Status: ReportRunning, Tenant: ReportTenant{VirtualKeyID: "vk-a"}}) // incomplete: ignored
	var nilMetrics *metrics
	nilMetrics.record(meteredReport("hlr_2", "openai", "gpt-4o")) // nil-safe

	got := m.usage()
	if len(got) != 1 {
		t.Fatalf("tenants %+v", got)
	}
	u := got[0]
	want := TenantUsage{Tenant: "vk-a", TenantName: "Acme", VerifierKeyID: "vk-verifier", Audits: 1,
		ClaimsChecked: 2, Searches: 1, SearchesReused: 1, Extracts: 1, TavilyCredits: 2,
		LLMCalls: 3, PromptTokens: 400, CompletionTokens: 60}
	if u.Tenant != want.Tenant || u.TenantName != want.TenantName || u.VerifierKeyID != want.VerifierKeyID || u.Audits != want.Audits ||
		u.ClaimsChecked != want.ClaimsChecked || u.Searches != want.Searches || u.SearchesReused != want.SearchesReused ||
		u.Extracts != want.Extracts || u.TavilyCredits != want.TavilyCredits || u.LLMCalls != want.LLMCalls ||
		u.PromptTokens != want.PromptTokens || u.CompletionTokens != want.CompletionTokens {
		t.Errorf("usage %+v", u)
	}
	if !near(u.TavilyUSD, 0.016) || !near(u.LLMUSD, 0.003) || !near(u.TotalUSD(), 0.019) || !near(u.AuditSeconds, 1.5) {
		t.Errorf("usd tavily %v llm %v total %v seconds %v", u.TavilyUSD, u.LLMUSD, u.TotalUSD(), u.AuditSeconds)
	}
	if u.Outcomes[OutcomeEvidenceFound] != 1 || u.Verdicts[VerdictContradicted] != 1 ||
		u.ClaimVerdicts[VerdictContradicted] != 1 || u.ClaimVerdicts[VerdictCurrent] != 1 || u.Failures[FailureCircuitOpen] != 1 {
		t.Errorf("maps %+v %+v %+v %+v", u.Outcomes, u.Verdicts, u.ClaimVerdicts, u.Failures)
	}

	// usage returns copies: changing them does not touch the counters.
	u.Outcomes[OutcomeEvidenceFound] = 99
	if m.usage()[0].Outcomes[OutcomeEvidenceFound] != 1 {
		t.Error("usage shares its maps with the counters")
	}
}

func near(a, b float64) bool { return a-b < 1e-9 && b-a < 1e-9 }

func TestMetricsModelSeriesCapped(t *testing.T) {
	m := newMetrics(0)
	for i := range maxModelSeries + 3 {
		m.record(meteredReport("hlr", "openai", fmt.Sprintf("model-%d", i)))
	}
	if len(m.models) != maxModelSeries+1 { // the cap, plus "other"
		t.Errorf("%d model series", len(m.models))
	}
	if n := m.answers[modelKey{"openai", "other", VerdictContradicted}]; n != 3 {
		t.Errorf("other = %d, want 3", n)
	}
}

func TestWriteMetrics(t *testing.T) {
	p := newPlugin(t)
	p.metrics.record(meteredReport("hlr_1", "openai", `gpt "4o"`+"\n"+`\x`))
	var b strings.Builder
	if err := p.WriteMetrics(&b); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, line := range []string{
		"# TYPE halflife_requests_total counter",
		`halflife_requests_total{outcome="captured"} 0`,
		"halflife_queue_depth 0",
		`halflife_circuit_open{dependency="llm"} 0`,
		`halflife_circuit_open{dependency="tavily"} 0`,
		`halflife_claims_checked_total{tenant="vk-a"} 2`,
		`halflife_tavily_usd_total{tenant="vk-a"} 0.016`,
		`halflife_llm_tokens_total{tenant="vk-a",kind="prompt"} 400`,
		`halflife_audits_total{tenant="vk-a",outcome="evidence_found"} 1`,
		`halflife_failures_total{tenant="vk-a",kind="circuit_open"} 1`,
		`halflife_claim_verdicts_total{tenant="vk-a",category="price",verdict="CONTRADICTED"} 1`,
		`halflife_model_verdicts_total{provider="openai",model="gpt \"4o\"\n\\x",verdict="CONTRADICTED"} 1`,
	} {
		if !strings.Contains(out, line+"\n") {
			t.Errorf("missing %q", line)
		}
	}
	// Every sample line is name{labels} value, and each family is declared once.
	seen := map[string]bool{}
	for _, l := range strings.Split(strings.TrimSuffix(out, "\n"), "\n") {
		if name, ok := strings.CutPrefix(l, "# TYPE "); ok {
			name = strings.Fields(name)[0]
			if seen[name] {
				t.Errorf("family %s declared twice", name)
			}
			seen[name] = true
			continue
		}
		if !strings.HasPrefix(l, "# HELP ") && !strings.HasPrefix(l, "halflife_") {
			t.Errorf("bad line %q", l)
		}
	}
	// Output is stable.
	var again strings.Builder
	p.WriteMetrics(&again)
	if again.String() != out {
		t.Error("output changed between two writes")
	}
}

func TestUsageReportsModeAndDependencies(t *testing.T) {
	p, err := Init(t.Context(), &Config{Mode: ModeMonitor, TavilyCreditUSD: 0.01}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Cleanup()
	u := p.Usage()
	if u.Mode != ModeMonitor || u.TavilyCreditUSD != 0.01 || u.Dependencies[DependencyTavily].State != "closed" || u.Dependencies[DependencyLLM].State != "closed" || len(u.Tenants) != 0 {
		t.Errorf("usage %+v", u)
	}
}

func TestReplyCost(t *testing.T) {
	for raw, want := range map[string]float64{
		"":                      0,
		"0.0042":                0.0042,
		`{"total_cost": 0.003}`: 0.003,
		"-1":                    0,
		`"free"`:                0,
		"null":                  0,
	} {
		if got := replyCost(json.RawMessage(raw)); !near(got, want) {
			t.Errorf("replyCost(%s) = %v, want %v", raw, got, want)
		}
	}
}
