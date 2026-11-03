package halflife

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	bifrost "github.com/maximhq/bifrost/core"
)

// fakeSearcher records searches and answers from a script.
type fakeSearcher struct {
	mu    sync.Mutex
	calls []fakeCall
	reply func(n int) (*SearchResponse, error) // n counts calls from 1

	extracts     []ExtractRequest
	extractReply func(req ExtractRequest) (*ExtractResponse, error)
}

type fakeCall struct {
	key  string
	req  SearchRequest
	attr Attribution
}

func (f *fakeSearcher) client(key verifierKey) searcher { return &keyedSearcher{f, key.id} }

type keyedSearcher struct {
	f   *fakeSearcher
	key string
}

func (k *keyedSearcher) Search(_ context.Context, req SearchRequest, attr Attribution) (*SearchResponse, error) {
	k.f.mu.Lock()
	k.f.calls = append(k.f.calls, fakeCall{k.key, req, attr})
	n := len(k.f.calls)
	k.f.mu.Unlock()
	if k.f.reply != nil {
		return k.f.reply(n)
	}
	return &SearchResponse{RequestID: "tv-1", GatewayRequestID: "gw-1", Usage: TavilyUsage{Credits: 1},
		Results: []SearchResult{{Title: "Acme pricing", URL: "https://acme.example/pricing", Content: "Pro is $59 per month.", Score: 0.9, PublishedDate: "2026-10-30"}}}, nil
}

func (k *keyedSearcher) Extract(_ context.Context, req ExtractRequest, attr Attribution) (*ExtractResponse, error) {
	k.f.mu.Lock()
	k.f.extracts = append(k.f.extracts, req)
	k.f.mu.Unlock()
	if k.f.extractReply != nil {
		return k.f.extractReply(req)
	}
	out := &ExtractResponse{Usage: TavilyUsage{Credits: 1}}
	for _, u := range req.URLs {
		out.Results = append(out.Results, ExtractResult{URL: u, RawContent: "Extracted from " + u})
	}
	return out, nil
}

// claim0 and evidence0 are a report's first claim and its evidence, or nil.
func claim0(r Report) *ReportClaim {
	if len(r.Claims) == 0 {
		return nil
	}
	return &r.Claims[0]
}

func evidence0(r Report) *ReportSources {
	if c := claim0(r); c != nil {
		return c.Evidence
	}
	return nil
}

func testVerifier(t *testing.T, keys ...VerifierKey) (*verifier, *fakeSearcher) {
	t.Helper()
	cfg := Config{Verifiers: keys}.withDefaults()
	if err := cfg.validate(); err != nil {
		t.Fatal(err)
	}
	v, err := newVerifier(cfg, newReportStore(10), bifrost.NewNoOpLogger())
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeSearcher{}
	v.newClient = f.client
	v.sleep = func(context.Context, time.Duration) error { return nil }
	return v, f
}

var (
	acmeKey   = VerifierKey{Tenant: "vk-acme", VirtualKeyID: "vk-verifier-acme", VirtualKey: "sk-bf-verifier-acme"}
	sharedKey = VerifierKey{Tenant: AnyTenant, VirtualKeyID: "vk-verifier", VirtualKey: "sk-bf-verifier"}
)

func acmeCapture(id, answer string) *Capture {
	return &Capture{ReportID: id, RequestID: "req-" + id, VirtualKeyID: "vk-acme", VirtualKeyName: "Acme app",
		Provider: "openai", Model: "ok", Question: "How much is Acme Cloud Pro?", Answer: answer, CapturedAt: time.Now().UTC()}
}

func runVerify(t *testing.T, v *verifier, c *Capture) (Report, error) {
	t.Helper()
	v.reports.put(newReport(c))
	stale, err := v.verify(context.Background(), c)
	if stale {
		t.Error("the minimal verifier judged an answer stale")
	}
	r, ok := v.reports.get(c.ReportID)
	if !ok {
		t.Fatal("report missing")
	}
	return r, err
}

func TestVerifierCollectsEvidence(t *testing.T) {
	v, f := testVerifier(t, acmeKey, sharedKey)
	r, err := runVerify(t, v, acmeCapture("hlr_1", "Sure! It costs $49 per month."))
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != ReportComplete || r.Outcome != OutcomeEvidenceFound || r.Verdict != VerdictNone || r.CompletedAt == nil {
		t.Errorf("report = %+v", r)
	}
	if claim0(r).Text != "It costs $49 per month." || claim0(r).Query != "How much is Acme Cloud Pro? It costs $49 per month." {
		t.Errorf("claim = %+v", claim0(r))
	}
	if len(f.calls) != 1 {
		t.Fatalf("searches = %d", len(f.calls))
	}
	call := f.calls[0]
	if call.key != "vk-verifier-acme" || call.attr != (Attribution{Tenant: "vk-acme", Report: "hlr_1"}) ||
		!reflect.DeepEqual(call.req, SearchRequest{Query: claim0(r).Query, SearchDepth: "basic", MaxResults: DefaultSearchMaxResults}) {
		t.Errorf("search = %+v", call)
	}
	e := evidence0(r)
	if e.VerifierKeyID != "vk-verifier-acme" || e.Credits != 1 || e.Reused || e.GatewayRequestID != "gw-1" || e.TavilyRequestID != "tv-1" ||
		len(e.Sources) != 1 || e.Sources[0] != (ReportSource{Title: "Acme pricing", URL: "https://acme.example/pricing", Score: 0.9, PublishedDate: "2026-10-30", Snippet: "Pro is $59 per month."}) {
		t.Errorf("evidence = %+v", e)
	}
	// The report never carries a key's secret value.
	raw, _ := json.Marshal(r)
	if strings.Contains(string(raw), "sk-bf-") {
		t.Errorf("report holds a key value: %s", raw)
	}
}

// D37: tenants without their own key use "*"; with neither, nothing is spent.
func TestVerifierKeyChoice(t *testing.T) {
	v, f := testVerifier(t, acmeKey, sharedKey)
	c := acmeCapture("hlr_g", "Globex Basic costs $10 per month.")
	c.VirtualKeyID = "vk-globex"
	if r, _ := runVerify(t, v, c); evidence0(r) == nil || evidence0(r).VerifierKeyID != "vk-verifier" || f.calls[0].attr.Tenant != "vk-globex" {
		t.Errorf("fallback key: %+v / %+v", evidence0(r), f.calls)
	}
	c.VirtualKeyID = "" // no governance on the request
	if r, _ := runVerify(t, v, c); evidence0(r) == nil || evidence0(r).VerifierKeyID != "vk-verifier" {
		t.Errorf("keyless request: %+v", evidence0(r))
	}

	v, f = testVerifier(t, acmeKey)
	c.VirtualKeyID = "vk-globex"
	r, err := runVerify(t, v, c)
	if err != nil || r.Outcome != OutcomeNoVerifierKey || claim0(r) == nil || evidence0(r) != nil || len(f.calls) != 0 {
		t.Errorf("no key: outcome %q err %v calls %d", r.Outcome, err, len(f.calls))
	}
}

func TestVerifierNoClaimSpendsNothing(t *testing.T) {
	v, f := testVerifier(t, sharedKey)
	r, err := runVerify(t, v, acmeCapture("hlr_n", "Recursion is when a function calls itself."))
	if err != nil || r.Outcome != OutcomeNoClaim || claim0(r) != nil || len(f.calls) != 0 {
		t.Errorf("outcome %q err %v calls %d", r.Outcome, err, len(f.calls))
	}
}

func TestVerifierNoEvidence(t *testing.T) {
	v, f := testVerifier(t, sharedKey)
	f.reply = func(int) (*SearchResponse, error) { return &SearchResponse{Usage: TavilyUsage{Credits: 1}}, nil }
	r, err := runVerify(t, v, acmeCapture("hlr_e", "Acme Cloud Pro costs $49 per month."))
	if err != nil || r.Outcome != OutcomeNoEvidence || evidence0(r).Credits != 1 || evidence0(r).Sources == nil {
		t.Errorf("outcome %q err %v evidence %+v", r.Outcome, err, evidence0(r))
	}
}

// D36: a 402 is the budget, not a fault: no retry, no audit failure.
func TestVerifierBudgetExhausted(t *testing.T) {
	v, f := testVerifier(t, sharedKey)
	f.reply = func(int) (*SearchResponse, error) { return nil, &TavilyError{Status: 402, Message: "Budget exceeded"} }
	r, err := runVerify(t, v, acmeCapture("hlr_b", "Acme Cloud Pro costs $49 per month."))
	if err != nil || r.Outcome != OutcomeBudgetExhausted || len(f.calls) != 1 || !strings.Contains(r.Error, "402") {
		t.Errorf("outcome %q err %v calls %d report error %q", r.Outcome, err, len(f.calls), r.Error)
	}
}

func TestVerifierRetriesOnceWhenRetryable(t *testing.T) {
	v, f := testVerifier(t, sharedKey)
	var waited time.Duration
	v.sleep = func(_ context.Context, d time.Duration) error { waited = d; return nil }
	f.reply = func(n int) (*SearchResponse, error) {
		if n == 1 {
			return nil, &TavilyError{Status: 429, Message: "slow down", RetryAfter: 30 * time.Second}
		}
		return &SearchResponse{Results: []SearchResult{{URL: "https://x.example"}}}, nil
	}
	r, err := runVerify(t, v, acmeCapture("hlr_r", "Acme Cloud Pro costs $49 per month."))
	if err != nil || r.Outcome != OutcomeEvidenceFound || len(f.calls) != 2 || waited != maxRetryAfter {
		t.Errorf("outcome %q err %v calls %d waited %v", r.Outcome, err, len(f.calls), waited)
	}

	// A second failure, or one that is not retryable, fails the audit.
	for name, reply := range map[string]func(int) (*SearchResponse, error){
		"500 twice": func(int) (*SearchResponse, error) { return nil, &TavilyError{Status: 500, Message: "boom"} },
		"432":       func(int) (*SearchResponse, error) { return nil, &TavilyError{Status: 432, Message: "plan limit"} },
		"network":   func(int) (*SearchResponse, error) { return nil, errors.New("connection refused") },
	} {
		v, f := testVerifier(t, sharedKey)
		f.reply = reply
		r, err := runVerify(t, v, acmeCapture("hlr_f", "Acme Cloud Pro costs $49 per month."))
		if err == nil || r.Outcome != OutcomeSearchFailed || r.Error == "" {
			t.Errorf("%s: outcome %q err %v", name, r.Outcome, err)
		}
		if want := map[string]int{"500 twice": 2, "432": 1, "network": 1}[name]; len(f.calls) != want {
			t.Errorf("%s: searches = %d, want %d", name, len(f.calls), want)
		}
	}
}

// D34: the same tenant's same query within the window is searched once.
func TestVerifierReusesIdenticalSearch(t *testing.T) {
	v, f := testVerifier(t, acmeKey, sharedKey)
	first, _ := runVerify(t, v, acmeCapture("hlr_1", "Acme Cloud Pro costs $49 per month."))
	second, _ := runVerify(t, v, acmeCapture("hlr_2", "ACME cloud pro costs $49 per month."))
	if len(f.calls) != 1 || evidence0(first).Reused || !evidence0(second).Reused || evidence0(second).Credits != 0 ||
		evidence0(second).GatewayRequestID != "" || len(evidence0(second).Sources) != 1 {
		t.Errorf("calls %d first %+v second %+v", len(f.calls), evidence0(first), evidence0(second))
	}
	other := acmeCapture("hlr_3", "Acme Cloud Pro costs $49 per month.")
	other.VirtualKeyID = "vk-globex" // another tenant pays for its own search
	runVerify(t, v, other)
	if len(f.calls) != 2 {
		t.Errorf("cross-tenant reuse: calls = %d", len(f.calls))
	}

	now := time.Now()
	v.memo.now = func() time.Time { return now.Add(searchMemoTTL + time.Second) }
	runVerify(t, v, acmeCapture("hlr_4", "Acme Cloud Pro costs $49 per month."))
	if len(f.calls) != 3 {
		t.Errorf("expired memo reused: calls = %d", len(f.calls))
	}
}

func TestSearchMemoBounded(t *testing.T) {
	m := newSearchMemo(2, time.Minute)
	for _, k := range []string{"a", "b", "c"} {
		m.put(k, &SearchResponse{})
	}
	if len(m.entries) != 2 {
		t.Errorf("entries = %d", len(m.entries))
	}
	if _, ok := m.get("c"); !ok {
		t.Error("newest entry evicted")
	}
}

// The verifier through the real Tavily client and a fake relay: what goes on
// the wire is the verifier key, never the tenant's.
func TestVerifierOverRelay(t *testing.T) {
	relay := newFakeRelay(t)
	cfg := Config{GatewayURL: relay.srv.URL, Verifiers: []VerifierKey{acmeKey}}.withDefaults()
	v, err := newVerifier(cfg, newReportStore(4), bifrost.NewNoOpLogger())
	if err != nil {
		t.Fatal(err)
	}
	r, err := runVerify(t, v, acmeCapture("hlr_w", "Acme Cloud Pro costs $49 per month."))
	if err != nil || r.Outcome != OutcomeEvidenceFound || evidence0(r).GatewayRequestID != "gw-1" {
		t.Fatalf("outcome %q err %v evidence %+v", r.Outcome, err, evidence0(r))
	}
	if relay.path != "/relay/tavily/search" || relay.header.Get("x-bf-vk") != "sk-bf-verifier-acme" ||
		relay.header.Get("X-Project-ID") != "vk-acme" || relay.header.Get("X-Session-Id") != "hlr_w" {
		t.Errorf("relay saw %s %v", relay.path, relay.header)
	}
	relay.reply = relayReply(http.StatusPaymentRequired, `{"error":{"message":"Budget exceeded"}}`)
	if r, err := runVerify(t, v, acmeCapture("hlr_x", "Initech Max costs $99 per month.")); err != nil || r.Outcome != OutcomeBudgetExhausted {
		t.Errorf("402 over the wire: outcome %q err %v", r.Outcome, err)
	}
}

// End to end inside the plugin: the hooks queue the answer with a report,
// a pool worker completes it, and Report serves it.
func TestPluginReportLifecycle(t *testing.T) {
	relay := newFakeRelay(t)
	p, err := Init(context.Background(), &Config{GatewayURL: relay.srv.URL,
		Verifiers: []VerifierKey{{Tenant: "vk-smoke", VirtualKeyID: "vk-verifier-smoke", VirtualKey: "sk-bf-verifier-smoke"}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Cleanup() })
	r := newRequest(t, p)
	r.attempt(chatReq("How much is Acme Cloud Pro?"), chatResp("Acme Cloud Pro costs $49 per month.", "ok"), nil)
	id := r.finish(200)[HeaderReport]
	if id == "" {
		t.Fatal("no report header")
	}
	var rep Report
	deadline := time.Now().Add(5 * time.Second)
	for {
		var ok bool
		if rep, ok = p.Report(id); !ok {
			t.Fatal("report not stored when queued")
		}
		if rep.Status == ReportComplete || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if rep.Status != ReportComplete || rep.Outcome != OutcomeEvidenceFound || rep.RequestID != "req-1" ||
		rep.Tenant != (ReportTenant{VirtualKeyID: "vk-smoke", VirtualKeyName: "Smoke test app"}) ||
		rep.Answer.Question != "How much is Acme Cloud Pro?" || evidence0(rep).VerifierKeyID != "vk-verifier-smoke" {
		t.Errorf("report = %+v", rep)
	}
	if relay.header.Get("x-bf-vk") != "sk-bf-verifier-smoke" {
		t.Error("relay call not made with the tenant's verifier key")
	}
	if _, ok := p.Report("hlr_unknown"); ok {
		t.Error("unknown report found")
	}

	// A dropped capture leaves no report behind.
	p.sink = func(*Capture) bool { return false }
	r = newRequest(t, p)
	r.attempt(chatReq("q"), chatResp("Acme Cloud Pro costs $49 per month.", "ok"), nil)
	if h := r.finish(200); h[HeaderReport] != "" || p.reports.(*reportStore).len() != 1 {
		t.Errorf("dropped capture: header %q, reports %d", h[HeaderReport], p.reports.(*reportStore).len())
	}
}
