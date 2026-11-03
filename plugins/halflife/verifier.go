package halflife

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/envutils"
)

// The minimal verifier (M1): pick one claim, search Tavily for it through the
// relay with the tenant's verifier key, and store the sources on the report.
// It judges nothing yet, so it never finds an answer stale.

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
	reports    *reportStore
	memo       *searchMemo
	logger     schemas.Logger
	sleep      func(context.Context, time.Duration) error
}

// newVerifier resolves each verifier key's secret (env.VAR allowed). It fails
// if one is missing, naming the variable, never a value.
func newVerifier(cfg Config, reports *reportStore, logger schemas.Logger) (*verifier, error) {
	httpClient := &http.Client{Timeout: 30 * time.Second}
	v := &verifier{
		keys:       make(map[string]verifierKey, len(cfg.Verifiers)),
		maxResults: cfg.SearchMaxResults,
		reports:    reports,
		memo:       newSearchMemo(searchMemoSize, searchMemoTTL),
		logger:     logger,
		sleep:      sleepCtx,
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

// verify audits one capture and completes its report. Expected outcomes (no
// claim, no key, budget spent) are not errors; a failed search is.
func (v *verifier) verify(ctx context.Context, c *Capture) (bool, error) {
	v.reports.update(c.ReportID, func(r *Report) { r.Status = ReportRunning })
	outcome, claim, evidence, err := v.run(ctx, c)
	now := time.Now().UTC()
	v.reports.update(c.ReportID, func(r *Report) {
		r.Status, r.Outcome, r.Claim, r.Evidence, r.CompletedAt = ReportComplete, outcome, claim, evidence, &now
		if err != nil {
			r.Error = err.Error()
		}
	})
	v.logger.Debug("halflife: report %s complete: outcome=%s", c.ReportID, outcome)
	if outcome == OutcomeSearchFailed {
		return false, err
	}
	return false, nil
}

func (v *verifier) run(ctx context.Context, c *Capture) (string, *ReportClaim, *ReportSources, error) {
	text, ok := pickClaim(c.Answer)
	if !ok {
		return OutcomeNoClaim, nil, nil, nil
	}
	claim := &ReportClaim{Text: text, Query: buildQuery(text, c.Question)}
	key, ok := v.keyFor(c.VirtualKeyID)
	if !ok {
		return OutcomeNoVerifierKey, claim, nil, nil
	}
	evidence := &ReportSources{VerifierKeyID: key.id, SearchDepth: "basic", Sources: []ReportSource{}}

	memoKey := c.VirtualKeyID + "\x00" + strings.ToLower(claim.Query)
	resp, reused := v.memo.get(memoKey)
	if !reused {
		var err error
		resp, err = v.search(ctx, key, claim.Query, Attribution{Tenant: c.VirtualKeyID, Report: c.ReportID})
		var te *TavilyError
		switch {
		case errors.As(err, &te) && te.Status == http.StatusPaymentRequired:
			// D36: the key's budget is spent for this window; retrying only adds refusals.
			return OutcomeBudgetExhausted, claim, evidence, err
		case err != nil:
			return OutcomeSearchFailed, claim, evidence, err
		}
		v.memo.put(memoKey, resp)
		evidence.Credits = resp.Usage.Credits
		evidence.GatewayRequestID = resp.GatewayRequestID
	}
	evidence.Reused = reused
	evidence.TavilyRequestID = resp.RequestID
	for _, r := range resp.Results {
		snippet, _ := clip(strings.TrimSpace(r.Content), maxSnippetBytes)
		evidence.Sources = append(evidence.Sources, ReportSource{Title: r.Title, URL: r.URL, Score: r.Score, PublishedDate: r.PublishedDate, Snippet: snippet})
	}
	if len(evidence.Sources) == 0 {
		return OutcomeNoEvidence, claim, evidence, nil
	}
	return OutcomeEvidenceFound, claim, evidence, nil
}

// search runs one search, retrying once if Tavily or the gateway says a retry
// may succeed (rate limit, capacity, server error).
func (v *verifier) search(ctx context.Context, key verifierKey, query string, attr Attribution) (*SearchResponse, error) {
	client := v.newClient(key)
	req := SearchRequest{Query: query, SearchDepth: "basic", MaxResults: v.maxResults}
	resp, err := client.Search(ctx, req, attr)
	var te *TavilyError
	if err == nil || !errors.As(err, &te) || !te.Retryable() {
		return resp, err
	}
	wait := min(te.RetryAfter, maxRetryAfter)
	if wait <= 0 {
		wait = defaultRetryWait
	}
	if serr := v.sleep(ctx, wait); serr != nil {
		return nil, err
	}
	return client.Search(ctx, req, attr)
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

// searchMemo remembers recent search responses per tenant and query (D34:
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
