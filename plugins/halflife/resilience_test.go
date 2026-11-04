package halflife

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// clock is a settable time source for breakers and backoffs.
type clock struct{ t time.Time }

func (c *clock) now() time.Time          { return c.t }
func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }
func newClock() *clock                   { return &clock{t: fixedNow} }
func useClock(v *verifier, c *clock) {
	v.tavilyCircuit.now, v.llmCircuit.now, v.budget.now = c.now, c.now, c.now
}
func tavilyErr(status int) *TavilyError {
	return &TavilyError{Status: status, Message: http.StatusText(status)}
}
func searchFails(err error) func(int) (*SearchResponse, error) {
	return func(int) (*SearchResponse, error) { return nil, err }
}

func TestFailureKind(t *testing.T) {
	timeoutErr := func() error {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { time.Sleep(200 * time.Millisecond) }))
		defer srv.Close()
		_, err := (&http.Client{Timeout: 20 * time.Millisecond}).Get(srv.URL)
		return err
	}()
	refused := func() error {
		srv := httptest.NewServer(http.NotFoundHandler())
		url := srv.URL
		srv.Close()
		_, err := http.Get(url)
		return err
	}()
	for name, tc := range map[string]struct {
		err  error
		want string
	}{
		"nil":              {nil, ""},
		"429":              {tavilyErr(429), FailureRateLimited},
		"402":              {&LLMError{Status: 402}, FailureBudget},
		"432":              {tavilyErr(432), FailureOutOfCredits},
		"433":              {tavilyErr(433), FailureOutOfCredits},
		"434":              {tavilyErr(434), FailureLiveFetchRefused},
		"503":              {&LLMError{Status: 503}, FailureServerError},
		"403":              {tavilyErr(403), FailureClientError},
		"client timeout":   {timeoutErr, FailureTimeout},
		"deadline":         {fmt.Errorf("wrapped: %w", context.DeadlineExceeded), FailureTimeout},
		"canceled":         {context.Canceled, FailureCanceled},
		"refused":          {refused, FailureNetwork},
		"circuit open":     {&UnavailableError{Dependency: DependencyTavily}, FailureCircuitOpen},
		"undecodable body": {errors.New("tavily relay: search: decode: unexpected EOF"), FailureBadReply},
	} {
		if got := failureKind(tc.err); got != tc.want {
			t.Errorf("%s: failureKind = %q, want %q (%v)", name, got, tc.want, tc.err)
		}
	}
}

func TestBreaker(t *testing.T) {
	c := newClock()
	b := newBreaker("tavily", 3, 30*time.Second)
	b.now = c.now
	outage, up := tavilyErr(503), tavilyErr(400)

	for range 2 {
		b.record(outage)
	}
	b.record(up) // any non-outage reply resets the count
	for range 2 {
		b.record(outage)
	}
	if err := b.allow(); err != nil || b.state().State != "closed" {
		t.Fatalf("opened early: %v %+v", err, b.state())
	}
	b.record(outage)
	var ue *UnavailableError
	if err := b.allow(); !errors.As(err, &ue) || !ue.Until.Equal(fixedNow.Add(30*time.Second)) || !strings.Contains(ue.Reason, "3 consecutive failures") {
		t.Fatalf("not open after 3 failures: %v", err)
	}

	c.advance(30 * time.Second) // cooldown over: one probe, the rest wait
	if s := b.state().State; s != "half_open" {
		t.Errorf("state %s", s)
	}
	if err := b.allow(); err != nil {
		t.Fatalf("probe refused: %v", err)
	}
	if err := b.allow(); err == nil {
		t.Error("second call let through while the probe is in flight")
	}
	b.record(outage) // the probe failed: open again at once
	if err := b.allow(); err == nil || b.state().Opens != 2 {
		t.Errorf("failed probe did not reopen: %v %+v", err, b.state())
	}

	c.advance(30 * time.Second)
	_ = b.allow()
	b.record(nil) // the probe succeeded
	if err := b.allow(); err != nil || b.state().State != "closed" || b.state().Failures != 0 {
		t.Errorf("successful probe did not close: %v %+v", err, b.state())
	}

	b.trip(fixedNow.Add(2*time.Hour), "credits exhausted")
	if open, ue := b.isOpen(); !open || ue.Reason != "credits exhausted" {
		t.Errorf("trip: %v %+v", open, ue)
	}
	b.trip(fixedNow, "earlier") // never shortens an open circuit
	if b.state().Reason != "credits exhausted" {
		t.Errorf("trip shortened the circuit: %+v", b.state())
	}
}

func TestRetryAfter(t *testing.T) {
	for v, want := range map[string]time.Duration{"": 0, "7": 7 * time.Second, " 2 ": 2 * time.Second, "0": 0, "-3": 0, "soon": 0,
		time.Now().Add(-time.Hour).UTC().Format(http.TimeFormat): 0} {
		if got := retryAfter(v); got != want {
			t.Errorf("retryAfter(%q) = %v, want %v", v, got, want)
		}
	}
	date := time.Now().Add(90 * time.Second).UTC().Format(http.TimeFormat)
	if got := retryAfter(date); got < 85*time.Second || got > 91*time.Second {
		t.Errorf("retryAfter(HTTP date) = %v", got)
	}
}

// A long Retry-After is not waited out: the claim fails, the circuit opens
// until then, the rest of the answer and the next audit spend nothing.
func TestLongRetryAfterOpensTheCircuit(t *testing.T) {
	v, f, g := extractingVerifier(t, nil, extractionReply(t, priceClaim, storageClaim), extractionReply(t, priceClaim))
	c := newClock()
	useClock(v, c)
	var slept []time.Duration
	v.sleep = func(_ context.Context, d time.Duration) error { slept = append(slept, d); return nil }
	f.reply = searchFails(&TavilyError{Status: 429, Message: "slow down", RetryAfter: 2 * time.Minute})

	r, err := runVerify(t, v, acmeCapture("hlr_1", pricingAnswer))
	if err == nil || r.Outcome != OutcomeSearchFailed || len(f.calls) != 1 || len(slept) != 0 {
		t.Fatalf("outcome %s err %v searches %d slept %v", r.Outcome, err, len(f.calls), slept)
	}
	if c0, c1 := r.Claims[0], r.Claims[1]; c0.Failure != FailureRateLimited || c1.Failure != FailureCircuitOpen || c1.Status != ClaimNotChecked ||
		!strings.Contains(c1.StatusReason, "rate limited, Retry-After 2m0s") {
		t.Errorf("claims: %s/%s %q; %s/%s %q", c0.Status, c0.Failure, c0.StatusReason, c1.Status, c1.Failure, c1.StatusReason)
	}

	llmCalls := g.calls()
	r, _ = runVerify(t, v, acmeCapture("hlr_2", pricingAnswer))
	if r.Outcome != OutcomeUnavailable || !strings.Contains(r.OutcomeReason, "tavily unavailable") || g.calls() != llmCalls || len(f.calls) != 1 {
		t.Errorf("while open: outcome %s (%s), llm calls %d→%d, searches %d", r.Outcome, r.OutcomeReason, llmCalls, g.calls(), len(f.calls))
	}

	c.advance(2 * time.Minute) // Retry-After passed: the next audit probes
	f.reply = nil
	r, _ = runVerify(t, v, acmeCapture("hlr_3", pricingAnswer))
	if r.Outcome != OutcomeEvidenceFound || v.tavilyCircuit.state().State != "closed" {
		t.Errorf("after the pause: outcome %s, circuit %+v", r.Outcome, v.tavilyCircuit.state())
	}
}

// 432/433: Tavily has no credits left; nothing is retried, later claims and
// audits are skipped for the credits cooldown.
func TestTavilyCreditsExhausted(t *testing.T) {
	for _, status := range []int{432, 433} {
		v, f, g := extractingVerifier(t, nil, extractionReply(t, priceClaim, storageClaim))
		useClock(v, newClock())
		f.reply = searchFails(tavilyErr(status))
		r, _ := runVerify(t, v, acmeCapture("hlr_c", pricingAnswer))
		if r.Outcome != OutcomeUnavailable || len(f.calls) != 1 || r.Claims[0].Failure != FailureOutOfCredits || r.Claims[1].Failure != FailureCircuitOpen {
			t.Errorf("%d: outcome %s searches %d failures %s, %s", status, r.Outcome, len(f.calls), r.Claims[0].Failure, r.Claims[1].Failure)
		}
		if s := v.tavilyCircuit.state(); !s.OpenUntil.Equal(fixedNow.Add(DefaultTavilyCreditsCooldownSeconds * time.Second)) {
			t.Errorf("%d: circuit %+v", status, s)
		}
		calls := g.calls()
		if r, _ := runVerify(t, v, acmeCapture("hlr_d", pricingAnswer)); r.Outcome != OutcomeUnavailable || g.calls() != calls {
			t.Errorf("%d: next audit %s, llm calls %d→%d", status, r.Outcome, calls, g.calls())
		}
	}
}

// Consecutive outages across audits open the circuit; one probe after the
// cooldown closes it again.
func TestConsecutiveFailuresOpenTheCircuit(t *testing.T) {
	v, f := testVerifier(t, sharedKey)
	c := newClock()
	useClock(v, c)
	f.reply = searchFails(tavilyErr(502))
	for i := range 3 { // 2 calls each (one retry): the 5th failure opens it
		runVerify(t, v, acmeCapture(fmt.Sprint("hlr_", i), fmt.Sprintf("Acme Cloud Pro costs $%d per month.", 40+i)))
	}
	if s := v.tavilyCircuit.state(); s.State != "open" || len(f.calls) != DefaultCircuitBreakerFailures {
		t.Fatalf("circuit %+v after %d calls", s, len(f.calls))
	}
	r, _ := runVerify(t, v, acmeCapture("hlr_x", "Acme Cloud Pro costs $50 per month."))
	if r.Outcome != OutcomeUnavailable || r.Claims[0].Failure != FailureCircuitOpen || len(f.calls) != DefaultCircuitBreakerFailures {
		t.Errorf("while open: %s %s calls %d", r.Outcome, r.Claims[0].Failure, len(f.calls))
	}
	c.advance(DefaultCircuitBreakerCooldownSeconds * time.Second)
	f.reply = nil
	if r, _ := runVerify(t, v, acmeCapture("hlr_y", "Acme Cloud Pro costs $51 per month.")); r.Outcome != OutcomeEvidenceFound {
		t.Errorf("probe: %s", r.Outcome)
	}
}

// A slow relay times out at search_timeout_seconds, as a real HTTP timeout.
func TestSearchTimeout(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
	}))
	defer slow.Close()
	v, _ := testVerifier(t, sharedKey)
	client := NewTavily(slow.URL, "tavily", "sk-bf-test", &http.Client{Timeout: 50 * time.Millisecond})
	v.newClient = func(verifierKey) searcher { return client }
	start := time.Now()
	r, err := runVerify(t, v, acmeCapture("hlr_t", "Acme Cloud Pro costs $49 per month."))
	if err == nil || r.Claims[0].Failure != FailureTimeout || time.Since(start) > time.Second {
		t.Errorf("failure %q err %v after %v", r.Claims[0].Failure, err, time.Since(start))
	}
}

// Budget-aware degradation: after a 402 the tenant's audits pause without a
// call; other tenants carry on; after the backoff the tenant tries again.
func TestBudgetBackoff(t *testing.T) {
	v, f := testVerifier(t, acmeKey, sharedKey)
	c := newClock()
	useClock(v, c)
	f.reply = searchFails(&TavilyError{Status: 402, Message: "budget exceeded"})
	runVerify(t, v, acmeCapture("hlr_1", "Acme Cloud Pro costs $49 per month."))

	f.reply = nil
	r, err := runVerify(t, v, acmeCapture("hlr_2", "Acme Cloud Pro costs $59 per month."))
	if err != nil || r.Outcome != OutcomeBudgetExhausted || !strings.Contains(r.OutcomeReason, "paused until 2026-11-03T12:05:00Z") || len(f.calls) != 1 || len(r.Claims) != 0 {
		t.Errorf("paused: outcome %s (%s) err %v searches %d", r.Outcome, r.OutcomeReason, err, len(f.calls))
	}
	// Finding nothing to check costs nothing, so it isn't paused.
	if r, _ := runVerify(t, v, acmeCapture("hlr_nothing", "Hello there.")); r.Outcome != OutcomeNoClaim {
		t.Errorf("nothing to check while paused: %s", r.Outcome)
	}
	other := acmeCapture("hlr_3", "Acme Cloud Pro costs $69 per month.")
	other.VirtualKeyID = "vk-globex"
	if r, _ := runVerify(t, v, other); r.Outcome != OutcomeEvidenceFound {
		t.Errorf("another tenant was paused: %s", r.Outcome)
	}
	c.advance(DefaultBudgetBackoffSeconds * time.Second)
	if r, _ := runVerify(t, v, acmeCapture("hlr_4", "Acme Cloud Pro costs $79 per month.")); r.Outcome != OutcomeEvidenceFound {
		t.Errorf("after the backoff: %s", r.Outcome)
	}
}

// The verifier LLM has its own circuit: failing extractions open it, and the
// judge is skipped while it is open.
func TestLLMCircuit(t *testing.T) {
	down := gwReply{status: 503, raw: `{"error":{"message":"all fallbacks failed"}}`}
	v, f, g := extractingVerifier(t, func(c *Config) { c.CircuitBreakerFailures = 2 }, down)
	useClock(v, newClock())
	// One structured call (with its own retry: 2 HTTP calls) is one failure.
	var r Report
	for i := range 2 {
		r, _ = runVerify(t, v, acmeCapture(fmt.Sprint("hlr_", i), pricingAnswer))
	}
	if r.Outcome != OutcomeExtractionFailed || v.llmCircuit.state().State != "open" || g.calls() != 4 {
		t.Fatalf("outcome %s circuit %+v calls %d", r.Outcome, v.llmCircuit.state(), g.calls())
	}
	r, _ = runVerify(t, v, acmeCapture("hlr_2", pricingAnswer))
	if r.Outcome != OutcomeUnavailable || !strings.Contains(r.OutcomeReason, "llm unavailable") || g.calls() != 4 || len(f.calls) != 0 {
		t.Errorf("while open: %s (%s) calls %d searches %d", r.Outcome, r.OutcomeReason, g.calls(), len(f.calls))
	}

	// The circuit opens between search and judge: the claim keeps its
	// evidence and says why it has no verdict.
	v, _, _ = extractingVerifier(t, nil, extractionReply(t, priceClaim), judgeReply(judgement{Verdict: judgeInsufficient}))
	f2 := &fakeSearcher{}
	v.newClient = func(key verifierKey) searcher {
		v.llmCircuit.trip(time.Now().Add(time.Minute), "test outage")
		return f2.client(key)
	}
	r, _ = runVerify(t, v, acmeCapture("hlr_3", pricingAnswer))
	if c := r.Claims[0]; c.Status != ClaimChecked || c.Verdict != VerdictNotChecked || c.Failure != FailureCircuitOpen || !strings.HasPrefix(c.VerdictReason, "judge skipped: llm unavailable") {
		t.Errorf("claim %s %s %s %q", c.Status, c.Verdict, c.Failure, c.VerdictReason)
	}
}
