package halflife

import (
	"container/list"
	"sync"
	"time"
)

// Report is the audit record for one answer, served by the gateway at
// GET /api/halflife/reports/{id}. It is created when the answer is queued for
// audit and completed by the verifier.
type Report struct {
	ID        string `json:"id"`
	RequestID string `json:"request_id"` // the gateway request (X-Bifrost-Request-Id) whose answer this audits
	Status    string `json:"status"`     // queued, running or complete
	Outcome   string `json:"outcome,omitempty"`
	// OutcomeReason explains an outcome reached without checking anything:
	// a paused budget or an unavailable dependency.
	OutcomeReason string     `json:"outcome_reason,omitempty"`
	Verdict       string     `json:"verdict"` // the claims' worst verdict; not_judged on the keyword path, which has no judge
	CreatedAt     time.Time  `json:"created_at"`
	CompletedAt   *time.Time `json:"completed_at,omitempty"`

	Tenant ReportTenant  `json:"tenant"`
	Answer ReportAnswer  `json:"answer"`
	Claims []ReportClaim `json:"claims"`
	// Extraction is the extractor's LLM call, when one was made.
	Extraction *LLMResult `json:"extraction,omitempty"`
	Error      string     `json:"error,omitempty"`
}

// Report outcomes, set when the report completes.
const (
	OutcomeEvidenceFound    = "evidence_found"       // at least one claim's search returned sources
	OutcomeNoEvidence       = "no_evidence"          // every search returned nothing
	OutcomeNoClaim          = "no_claim"             // nothing in the answer was worth checking
	OutcomeNothingToCheck   = "nothing_to_check"     // claims were found, but each was skipped or rejected
	OutcomeNoVerifierKey    = "no_verifier_key"      // the tenant has no verifier key, and there is no "*" entry
	OutcomeBudgetExhausted  = "budget_exhausted"     // the verifier key's budget refused the first call (402)
	OutcomeSearchFailed     = "search_failed"        // the relay or Tavily failed for every claim tried
	OutcomeExtractionFailed = "extraction_failed"    // the extractor's reply was unusable or its call failed
	OutcomeUnavailable      = "verifier_unavailable" // Tavily or the verifier LLM is down (circuit open) or out of credits; nothing was spent
)

// Claim statuses.
const (
	ClaimPending    = "pending"     // extracted and ranked, not yet searched
	ClaimChecked    = "checked"     // searched; see Evidence
	ClaimNotChecked = "not_checked" // skipped by priority, the per-answer cap or a spent budget; see StatusReason
	ClaimRejected   = "rejected"    // failed the span check; never searched
	ClaimFailed     = "failed"      // its search failed
)

const (
	ReportQueued   = "queued"
	ReportRunning  = "running"
	ReportComplete = "complete"
	VerdictNone    = "not_judged"
)

// ReportTenant names the tenant by its virtual key, never the key's value.
type ReportTenant struct {
	VirtualKeyID   string `json:"virtual_key_id,omitempty"`
	VirtualKeyName string `json:"virtual_key_name,omitempty"`
}

// ReportAnswer describes the audited answer.
type ReportAnswer struct {
	Provider       string `json:"provider"`
	Model          string `json:"model"`
	RequestedModel string `json:"requested_model,omitempty"`
	IsFallback     bool   `json:"is_fallback"`
	Question       string `json:"question,omitempty"`
	Text           string `json:"text"`
	TextRedacted   bool   `json:"text_redacted,omitempty"` // question and text left out of the persistent store (persist_answer_text off)
	Truncated      bool   `json:"truncated,omitempty"`
	CacheID        string `json:"cache_id,omitempty"`
}

// ReportClaim is one claim from the answer, how it was classified, and what
// was done about it. Claims picked by the M1 heuristic (no verifier model)
// carry only Text, Span, Status, Query and Evidence.
type ReportClaim struct {
	Text           string `json:"text"`
	Span           string `json:"span"` // verbatim from the answer
	Subject        string `json:"subject,omitempty"`
	Attribute      string `json:"attribute,omitempty"`
	AssertedValue  string `json:"asserted_value,omitempty"`
	Category       string `json:"category,omitempty"`
	TemporalScope  string `json:"temporal_scope,omitempty"`
	ScopeReason    string `json:"scope_reason,omitempty"`
	AsOf           string `json:"as_of,omitempty"`
	VolatilityBand string `json:"volatility_band,omitempty"`
	Priority       string `json:"priority,omitempty"`
	PriorityReason string `json:"priority_reason,omitempty"`
	Status         string `json:"status"`
	StatusReason   string `json:"status_reason,omitempty"`
	Query          string `json:"query,omitempty"`
	// Search is the request sent to Tavily, policy included.
	Search   *SearchRequest `json:"search,omitempty"`
	Evidence *ReportSources `json:"evidence,omitempty"`
	// Failure is the kind of failure that left the claim unchecked or
	// unjudged (resilience.go), e.g. timeout or circuit_open.
	Failure string `json:"failure,omitempty"`

	// What the evidence says (judge.go): the verdict, why, and the quotes
	// it rests on, each checked against the source it cites.
	Verdict       string       `json:"verdict,omitempty"`
	VerdictReason string       `json:"verdict_reason,omitempty"`
	ObservedValue string       `json:"observed_value,omitempty"` // the current value, as the judge read it from the evidence
	Quote         *ReportQuote `json:"quote,omitempty"`
	HistoryQuote  *ReportQuote `json:"history_quote,omitempty"` // shows the claimed value held before; required for OUTDATED
	OtherQuote    *ReportQuote `json:"other_quote,omitempty"`   // the other side of a CONFLICTING verdict
	Comparison    *Comparison  `json:"comparison,omitempty"`    // the typed comparator's result, when one applied
	Judge         *LLMResult   `json:"judge,omitempty"`

	// How strong the evidence is, and the heuristic score from verdict and
	// strength (strength.go). Never a probability.
	Strength       string `json:"strength,omitempty"`
	StrengthReason string `json:"strength_reason,omitempty"`
	FreshnessScore *int   `json:"freshness_score,omitempty"`
}

// ReportQuote is a passage the judge cited, and whether it was found
// verbatim in that source.
type ReportQuote struct {
	Text     string `json:"text"`
	Source   int    `json:"source"` // index into evidence.sources; -1 when none was cited
	URL      string `json:"url,omitempty"`
	Verified bool   `json:"verified"`
	Flagged  bool   `json:"flagged,omitempty"` // its source held instruction-like text: it backs no verdict
}

// ReportSources is the evidence found for the claim, and what it cost.
type ReportSources struct {
	VerifierKeyID    string         `json:"verifier_virtual_key_id"`
	SearchDepth      string         `json:"search_depth"`
	Credits          float64        `json:"credits"`
	Reused           bool           `json:"reused"`                       // served from an earlier identical search (D34): no new credits
	LiveFetchRefused bool           `json:"live_fetch_refused,omitempty"` // Tavily refused max_age_hours (434); served from its cache instead
	ExtractedURL     string         `json:"extracted_url,omitempty"`      // the page fetched with Extract because the snippets were thin
	GatewayRequestID string         `json:"gateway_request_id,omitempty"` // the relay call, in Bifrost's relay log
	TavilyRequestID  string         `json:"tavily_request_id,omitempty"`
	DroppedSources   int            `json:"dropped_sources,omitempty"` // results without a usable http(s) URL, or over the per-claim cap
	Sources          []ReportSource `json:"sources"`
}

// ReportSource is one search result.
type ReportSource struct {
	Title         string  `json:"title"`
	URL           string  `json:"url"`
	Score         float64 `json:"score"`
	PublishedDate string  `json:"published_date,omitempty"`
	Snippet       string  `json:"snippet"`
	Extracted     string  `json:"extracted,omitempty"` // the page's most relevant chunks, from Extract
	// Flagged sources held instruction-like text (untrusted.go), removed
	// before storing; no verdict rests on a flagged source's quote.
	Flagged    bool   `json:"flagged,omitempty"`
	FlagReason string `json:"flag_reason,omitempty"`
}

// newReport starts the report for a capture about to be queued.
func newReport(c *Capture) *Report {
	return &Report{
		ID: c.ReportID, RequestID: c.RequestID, Status: ReportQueued, Verdict: VerdictNone, CreatedAt: c.CapturedAt,
		Tenant: ReportTenant{VirtualKeyID: c.VirtualKeyID, VirtualKeyName: c.VirtualKeyName},
		Answer: ReportAnswer{
			Provider: string(c.Provider), Model: c.Model, RequestedModel: c.RequestedModel, IsFallback: c.IsFallback,
			Question: c.Question, Text: c.Answer, Truncated: c.Truncated, CacheID: c.CacheID,
		},
		Claims: []ReportClaim{},
	}
}

func (c ReportClaim) clone() ReportClaim {
	if c.Search != nil {
		s := *c.Search
		s.IncludeDomains = append([]string(nil), c.Search.IncludeDomains...)
		if c.Search.MaxAgeHours != nil {
			h := *c.Search.MaxAgeHours
			s.MaxAgeHours = &h
		}
		c.Search = &s
	}
	for _, q := range []**ReportQuote{&c.Quote, &c.HistoryQuote, &c.OtherQuote} {
		if *q != nil {
			cp := **q
			*q = &cp
		}
	}
	if c.Comparison != nil {
		cp := *c.Comparison
		c.Comparison = &cp
	}
	if c.Judge != nil {
		j := *c.Judge
		j.GatewayRequestIDs = append([]string{}, c.Judge.GatewayRequestIDs...)
		c.Judge = &j
	}
	if c.FreshnessScore != nil {
		n := *c.FreshnessScore
		c.FreshnessScore = &n
	}
	if c.Evidence != nil {
		ev := *c.Evidence
		ev.Sources = append(make([]ReportSource, 0, len(c.Evidence.Sources)), c.Evidence.Sources...) // keep [] as [] in JSON
		c.Evidence = &ev
	}
	return c
}

// clone returns a deep copy, so readers never share the store's report.
func (r *Report) clone() Report {
	out := *r
	if r.CompletedAt != nil {
		t := *r.CompletedAt
		out.CompletedAt = &t
	}
	out.Claims = make([]ReportClaim, len(r.Claims))
	for i, c := range r.Claims {
		out.Claims[i] = c.clone()
	}
	if r.Extraction != nil {
		ex := *r.Extraction
		ex.GatewayRequestIDs = append([]string{}, r.Extraction.GatewayRequestIDs...)
		out.Extraction = &ex
	}
	return out
}

// reportStore keeps the most recent reports in memory, evicting the oldest
// once full. On its own (no report_store_path) reports are lost on restart;
// sqliteReports uses it for reports still in flight.
type reportStore struct {
	mu    sync.Mutex
	max   int
	byID  map[string]*list.Element
	order *list.List // front = oldest
}

func newReportStore(max int) *reportStore {
	return &reportStore{max: max, byID: make(map[string]*list.Element), order: list.New()}
}

func (s *reportStore) put(r *Report) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.byID[r.ID]; ok {
		e.Value = r
		return
	}
	s.byID[r.ID] = s.order.PushBack(r)
	for s.order.Len() > s.max {
		oldest := s.order.Front()
		s.order.Remove(oldest)
		delete(s.byID, oldest.Value.(*Report).ID)
	}
}

// update applies fn to a stored report. It reports false if the report is
// gone (never stored, or evicted).
func (s *reportStore) update(id string, fn func(*Report)) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.byID[id]
	if !ok {
		return false
	}
	fn(e.Value.(*Report))
	return true
}

func (s *reportStore) get(id string) (Report, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.byID[id]
	if !ok {
		return Report{}, false
	}
	return e.Value.(*Report).clone(), true
}

func (s *reportStore) remove(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.byID[id]; ok {
		s.order.Remove(e)
		delete(s.byID, id)
	}
}

func (s *reportStore) close() error { return nil }

func (s *reportStore) len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.order.Len()
}
