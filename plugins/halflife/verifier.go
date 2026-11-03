package halflife

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/envutils"
)

// The verifier audits one answer and completes its report. With verifier
// models configured it extracts atomic claims through the gateway, rejects
// any that drifted from the answer, classifies their temporal scope, ranks
// them by verification priority and searches the highest-ranked ones with a
// per-category retrieval policy. Without models it falls back to M1's
// heuristic: one keyword-picked sentence, searched as is. It judges nothing
// yet, so it never finds an answer stale.

const (
	searchMemoTTL    = 10 * time.Minute // D34: an identical search inside this window is reused
	searchMemoSize   = 1024
	maxSnippetBytes  = 500
	maxRetryAfter    = 5 * time.Second
	defaultRetryWait = time.Second
)

// searcher is the part of the Tavily client the verifier uses.
type searcher interface {
	Search(ctx context.Context, req SearchRequest, attr Attribution) (*SearchResponse, error)
	Extract(ctx context.Context, req ExtractRequest, attr Attribution) (*ExtractResponse, error)
}

type verifierKey struct {
	id    string
	value string // the key's secret; sent only to this gateway
}

type verifier struct {
	keys       map[string]verifierKey // tenant virtual key ID (or "*") -> verifier key
	newClient  func(key verifierKey) searcher
	newLLM     func(key verifierKey) *LLM // nil when no verifier models are configured
	maxResults int
	maxClaims  int // searches per answer
	cutoffs    cutoffTable
	boosted    map[string]bool     // categories raised one priority level
	domains    map[string][]string // category -> preferred domains
	extract    bool                // Extract the best page when snippets are thin
	reports    reports
	memo       *searchMemo
	logger     schemas.Logger
	sleep      func(context.Context, time.Duration) error
	now        func() time.Time
}

// newVerifier resolves each verifier key's secret (env.VAR allowed). It fails
// if one is missing, naming the variable, never a value.
func newVerifier(cfg Config, store reports, logger schemas.Logger) (*verifier, error) {
	cutoffs, err := newCutoffTable(cfg.ModelCutoffs)
	if err != nil {
		return nil, err
	}
	httpClient := &http.Client{Timeout: 30 * time.Second}
	v := &verifier{
		keys:       make(map[string]verifierKey, len(cfg.Verifiers)),
		maxResults: cfg.SearchMaxResults,
		maxClaims:  cfg.MaxClaimsPerResponse,
		cutoffs:    cutoffs,
		boosted:    make(map[string]bool, len(cfg.PriorityBoostCategories)),
		domains:    cfg.PreferredDomains,
		extract:    !cfg.DisableExtract,
		reports:    store,
		memo:       newSearchMemo(searchMemoSize, searchMemoTTL),
		logger:     logger,
		sleep:      sleepCtx,
		now:        time.Now,
	}
	for _, c := range cfg.PriorityBoostCategories {
		v.boosted[c] = true
	}
	v.newClient = func(key verifierKey) searcher {
		return NewTavily(cfg.GatewayURL, cfg.TavilyRelay, key.value, httpClient)
	}
	if len(cfg.VerifierModels) > 0 {
		llmHTTP := &http.Client{Timeout: 90 * time.Second} // free models can take a while; the audit timeout still bounds it
		v.newLLM = func(key verifierKey) *LLM {
			return NewLLM(cfg.GatewayURL, key.value, LLMOptions{Models: cfg.VerifierModels, MaxTokens: cfg.VerifierMaxTokens, ReasoningEffort: cfg.VerifierReasoningEffort}, llmHTTP)
		}
	}
	for _, k := range cfg.Verifiers {
		value, err := envutils.ProcessEnvValue(k.VirtualKey)
		if err != nil || value == "" {
			if err == nil {
				err = errors.New("empty value")
			}
			return nil, fmt.Errorf("halflife: verifier key for tenant %q: %w", k.Tenant, err)
		}
		v.keys[k.Tenant] = verifierKey{id: k.VirtualKeyID, value: value}
	}
	return v, nil
}

// keyFor returns the key that pays for verifying tenant's answers.
func (v *verifier) keyFor(tenant string) (verifierKey, bool) {
	if k, ok := v.keys[tenant]; ok && tenant != "" {
		return k, true
	}
	k, ok := v.keys[AnyTenant]
	return k, ok
}

// llmFor returns the LLM client that charges tenant's verifier key, or
// ok=false when the tenant has no key or no verifier model is configured.
func (v *verifier) llmFor(tenant string) (*LLM, verifierKey, bool) {
	key, ok := v.keyFor(tenant)
	if !ok || v.newLLM == nil {
		return nil, verifierKey{}, false
	}
	return v.newLLM(key), key, true
}

// audit is the outcome of one answer's verification.
type audit struct {
	outcome    string
	verdict    string
	claims     []ReportClaim
	extraction *LLMResult
	err        error
}

// verify audits one capture and completes its report. Expected outcomes (no
// claim, no key, budget spent, nothing worth checking) are not errors;
// failed searches and a failed extraction are.
func (v *verifier) verify(ctx context.Context, c *Capture) (bool, error) {
	v.reports.update(c.ReportID, func(r *Report) { r.Status = ReportRunning })
	var a audit
	if v.newLLM == nil {
		a = v.runHeuristic(ctx, c)
	} else {
		a = v.runExtracted(ctx, c)
	}
	if a.claims == nil {
		a.claims = []ReportClaim{}
	}
	now := time.Now().UTC()
	v.reports.update(c.ReportID, func(r *Report) {
		r.Status, r.Outcome, r.Claims, r.Extraction, r.CompletedAt = ReportComplete, a.outcome, a.claims, a.extraction, &now
		if a.verdict != "" {
			r.Verdict = a.verdict
		}
		if a.err != nil {
			r.Error = a.err.Error()
		}
	})
	v.logger.Debug("halflife: report %s complete: outcome=%s verdict=%s claims=%d", c.ReportID, a.outcome, a.verdict, len(a.claims))
	if a.outcome == OutcomeSearchFailed || a.outcome == OutcomeExtractionFailed {
		return false, a.err
	}
	return false, nil
}

// runHeuristic is M1's path: one keyword-picked sentence, searched as is.
func (v *verifier) runHeuristic(ctx context.Context, c *Capture) audit {
	text, ok := pickClaim(c.Answer)
	if !ok {
		return audit{outcome: OutcomeNoClaim}
	}
	claim := ReportClaim{Text: text, Span: text, Status: ClaimPending, Query: buildQuery(text, c.Question)}
	key, ok := v.keyFor(c.VirtualKeyID)
	if !ok {
		claim.Status, claim.StatusReason = ClaimNotChecked, "no verifier key"
		return audit{outcome: OutcomeNoVerifierKey, claims: []ReportClaim{claim}}
	}
	req := SearchRequest{Query: claim.Query, SearchDepth: "basic", MaxResults: v.maxResults}
	err := v.check(ctx, key, c, &claim, req, false)
	return audit{outcome: outcomeOf([]ReportClaim{claim}, err), claims: []ReportClaim{claim}, err: err}
}

// runExtracted is the model path: extract, filter, rank, search, then judge.
func (v *verifier) runExtracted(ctx context.Context, c *Capture) audit {
	llm, key, ok := v.llmFor(c.VirtualKeyID)
	if !ok {
		return audit{outcome: OutcomeNoVerifierKey}
	}
	extracted, res, err := extractClaims(ctx, llm, c.Question, c.Answer)
	var le *LLMError
	switch {
	case errors.As(err, &le) && le.BudgetSpent():
		return audit{outcome: OutcomeBudgetExhausted, extraction: res, err: err}
	case err != nil:
		return audit{outcome: OutcomeExtractionFailed, extraction: res, err: err}
	case len(extracted) == 0:
		return audit{outcome: OutcomeNoClaim, verdict: VerdictNotChecked, extraction: res}
	}

	now := v.now()
	claims := make([]ReportClaim, len(extracted))
	for i, e := range extracted {
		claims[i] = v.classify(e, c, now)
	}
	ptrs := make([]*ReportClaim, len(claims))
	for i := range claims {
		ptrs[i] = &claims[i]
	}

	var firstErr error
	searched, spent := 0, false
	for _, rc := range rankForChecking(ptrs, v.boosted) {
		switch {
		case spent:
			rc.Status, rc.StatusReason = ClaimNotChecked, "verifier budget spent (402)"
			continue
		case searched >= v.maxClaims:
			rc.Status, rc.StatusReason = ClaimNotChecked, fmt.Sprintf("over max_claims_per_response (%d)", v.maxClaims)
			continue
		}
		searched++
		req := retrievalPolicy(rc, v.maxResults, v.domains[rc.Category])
		if err := v.check(ctx, key, c, rc, req, v.extract); err != nil {
			spent = isBudgetRefusal(err) // the key's budget covers every later call too
			if firstErr == nil {
				firstErr = err
			}
		}
		if rc.Status == ClaimChecked && !spent {
			spent = v.judge(ctx, llm, rc) != nil // only a 402 is returned
		}
	}
	for i := range claims {
		rc := &claims[i]
		if rc.Status == ClaimPending { // SKIP
			rc.Status, rc.StatusReason = ClaimNotChecked, "priority SKIP: "+rc.PriorityReason
		}
		if rc.Verdict == "" {
			rc.Verdict, rc.VerdictReason = VerdictNotChecked, notCheckedReason(rc)
		}
		assess(rc, v.domains[rc.Category])
	}
	return audit{outcome: outcomeOf(claims, firstErr), verdict: reportVerdict(claims), claims: claims, extraction: res, err: firstErr}
}

// notCheckedReason says why a claim has no verdict.
func notCheckedReason(rc *ReportClaim) string {
	switch rc.Status {
	case ClaimRejected:
		return "rejected by the span check: " + rc.StatusReason
	case ClaimFailed:
		return "search failed: " + rc.StatusReason
	case ClaimChecked:
		return "verifier budget spent before the judge (402)"
	}
	return rc.StatusReason
}

// classify turns an extracted claim into a report claim: span check, then
// temporal scope, then priority.
func (v *verifier) classify(e extractedClaim, c *Capture, now time.Time) ReportClaim {
	rc := ReportClaim{
		Text: e.Text, Span: e.Span, Subject: e.Subject, Attribute: e.Attribute, AssertedValue: e.AssertedValue,
		Category: e.Category, AsOf: e.AsOf, VolatilityBand: e.VolatilityBand, Status: ClaimPending,
	}
	if reason := checkSpan(e, c.Answer); reason != "" {
		rc.Status, rc.StatusReason = ClaimRejected, reason
		return rc
	}
	rc.TemporalScope, rc.ScopeReason = classifyScope(e, now)
	rc.Priority, rc.PriorityReason = prioritise(priorityInput{
		Scope: rc.TemporalScope, Band: rc.VolatilityBand, Category: rc.Category, AsOf: rc.AsOf, Span: rc.Span,
		Provider: string(c.Provider), Model: c.Model,
	}, v.cutoffs, v.boosted, now)
	return rc
}

// outcomeOf summarises the claims' statuses into the report's outcome.
func outcomeOf(claims []ReportClaim, err error) string {
	var checked, failed, budget, evidence int
	for _, c := range claims {
		switch {
		case c.Status == ClaimChecked:
			checked++
			if c.Evidence != nil && len(c.Evidence.Sources) > 0 {
				evidence++
			}
		case c.Status == ClaimFailed:
			failed++
		case c.Status == ClaimNotChecked && strings.Contains(c.StatusReason, "402"):
			budget++
		}
	}
	switch {
	case evidence > 0:
		return OutcomeEvidenceFound
	case checked > 0:
		return OutcomeNoEvidence
	case budget > 0:
		return OutcomeBudgetExhausted
	case failed > 0 || err != nil:
		return OutcomeSearchFailed
	case len(claims) == 0:
		return OutcomeNoClaim
	}
	return OutcomeNothingToCheck
}

// check searches one claim and records the evidence on it. A 402 leaves the
// claim not checked; any other failure marks it failed. Both are returned.
func (v *verifier) check(ctx context.Context, key verifierKey, c *Capture, rc *ReportClaim, req SearchRequest, extract bool) error {
	rc.Query, rc.Search = req.Query, &req
	evidence := &ReportSources{VerifierKeyID: key.id, SearchDepth: "basic", Sources: []ReportSource{}}
	attr := Attribution{Tenant: c.VirtualKeyID, Report: c.ReportID}
	client := v.newClient(key)

	memoKey := c.VirtualKeyID + "\x00" + memoRequestKey(req)
	resp, reused := v.memo.get(memoKey)
	if !reused {
		var refused bool
		var err error
		resp, refused, err = v.search(ctx, client, req, attr)
		if err != nil {
			return v.recordFailure(rc, evidence, err)
		}
		v.memo.put(memoKey, resp)
		evidence.Credits, evidence.GatewayRequestID, evidence.LiveFetchRefused = resp.Usage.Credits, resp.GatewayRequestID, refused
	}
	evidence.Reused, evidence.TavilyRequestID = reused, resp.RequestID
	for _, r := range resp.Results {
		snippet, _ := clip(strings.TrimSpace(r.Content), maxSnippetBytes)
		evidence.Sources = append(evidence.Sources, ReportSource{Title: r.Title, URL: r.URL, Score: r.Score, PublishedDate: r.PublishedDate, Snippet: snippet})
	}
	rc.Status, rc.Evidence = ClaimChecked, evidence

	// One Extract per claim at most, never for a reused search (its pages
	// were already judged thin or not), and only when the snippets miss it.
	if !extract || reused || !thinEvidence(rc, evidence.Sources) {
		return nil
	}
	i := extractTarget(evidence.Sources, v.domains[rc.Category])
	if i < 0 {
		return nil
	}
	er, err := client.Extract(ctx, ExtractRequest{URLs: []string{evidence.Sources[i].URL}, Query: claimQuery(rc), ChunksPerSource: extractChunksPerSource}, attr)
	if err != nil {
		if isBudgetRefusal(err) {
			rc.StatusReason = "extract refused: verifier budget spent (402)"
			return err
		}
		rc.StatusReason = "extract failed: " + err.Error() // the search's evidence stands
		return nil
	}
	evidence.Credits += er.Usage.Credits
	evidence.ExtractedURL = evidence.Sources[i].URL
	for _, r := range er.Results {
		if r.URL == evidence.Sources[i].URL {
			evidence.Sources[i].Extracted, _ = clip(strings.TrimSpace(r.RawContent), maxExtractedBytes)
		}
	}
	return nil
}

func isBudgetRefusal(err error) bool {
	var te *TavilyError
	return errors.As(err, &te) && te.Status == http.StatusPaymentRequired
}

func (v *verifier) recordFailure(rc *ReportClaim, evidence *ReportSources, err error) error {
	if isBudgetRefusal(err) {
		// D36: the key's budget is spent for this window; retrying only adds refusals.
		rc.Status, rc.StatusReason = ClaimNotChecked, "verifier budget spent (402)"
	} else {
		rc.Status, rc.StatusReason = ClaimFailed, err.Error()
	}
	rc.Evidence = evidence
	return err
}

// memoRequestKey identifies a search by everything that changes its results.
func memoRequestKey(req SearchRequest) string {
	b, _ := json.Marshal(req)
	return strings.ToLower(string(b))
}

// search runs one search. A refused live fetch (434) is retried at once
// without max_age_hours; then a retryable failure (rate limit, capacity,
// server error) is retried once after a wait. refused reports the first.
func (v *verifier) search(ctx context.Context, client searcher, req SearchRequest, attr Attribution) (resp *SearchResponse, refused bool, err error) {
	resp, err = client.Search(ctx, req, attr)
	var te *TavilyError
	if errors.As(err, &te) && te.LiveFetchRefused() && req.MaxAgeHours != nil {
		req.MaxAgeHours, refused = nil, true
		resp, err = client.Search(ctx, req, attr)
	}
	if err == nil || !errors.As(err, &te) || !te.Retryable() {
		return resp, refused, err
	}
	wait := min(te.RetryAfter, maxRetryAfter)
	if wait <= 0 {
		wait = defaultRetryWait
	}
	if serr := v.sleep(ctx, wait); serr != nil {
		return nil, refused, err
	}
	resp, err = client.Search(ctx, req, attr)
	return resp, refused, err
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// searchMemo remembers recent search responses per tenant and request (D34:
// Tavily bills a repeated query again, even when it serves it from its cache).
type searchMemo struct {
	mu      sync.Mutex
	max     int
	ttl     time.Duration
	now     func() time.Time
	entries map[string]memoEntry
}

type memoEntry struct {
	resp    *SearchResponse
	expires time.Time
}

func newSearchMemo(max int, ttl time.Duration) *searchMemo {
	return &searchMemo{max: max, ttl: ttl, now: time.Now, entries: make(map[string]memoEntry)}
}

func (m *searchMemo) get(key string) (*SearchResponse, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.entries[key]
	if !ok || m.now().After(e.expires) {
		delete(m.entries, key)
		return nil, false
	}
	return e.resp, true
}

func (m *searchMemo) put(key string, resp *SearchResponse) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	if len(m.entries) >= m.max {
		for k, e := range m.entries { // drop expired entries, else one arbitrary entry
			if now.After(e.expires) {
				delete(m.entries, k)
			}
		}
		for k := range m.entries {
			if len(m.entries) < m.max {
				break
			}
			delete(m.entries, k)
		}
	}
	m.entries[key] = memoEntry{resp: resp, expires: now.Add(m.ttl)}
}
