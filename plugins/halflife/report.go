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
	ID          string     `json:"id"`
	RequestID   string     `json:"request_id"` // the gateway request (X-Bifrost-Request-Id) whose answer this audits
	Status      string     `json:"status"`     // queued, running or complete
	Outcome     string     `json:"outcome,omitempty"`
	Verdict     string     `json:"verdict"` // not_judged: M1 collects evidence but judges nothing
	CreatedAt   time.Time  `json:"created_at"`
	CompletedAt *time.Time `json:"completed_at,omitempty"`

	Tenant   ReportTenant   `json:"tenant"`
	Answer   ReportAnswer   `json:"answer"`
	Claim    *ReportClaim   `json:"claim,omitempty"`
	Evidence *ReportSources `json:"evidence,omitempty"`
	Error    string         `json:"error,omitempty"`
}

// Report outcomes, set when the report completes.
const (
	OutcomeEvidenceFound   = "evidence_found"   // the claim's search returned sources
	OutcomeNoEvidence      = "no_evidence"      // the search returned nothing
	OutcomeNoClaim         = "no_claim"         // no sentence was worth checking
	OutcomeNoVerifierKey   = "no_verifier_key"  // the tenant has no verifier key, and there is no "*" entry
	OutcomeBudgetExhausted = "budget_exhausted" // the verifier key's budget refused the search (402)
	OutcomeSearchFailed    = "search_failed"    // the relay or Tavily failed
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
	Truncated      bool   `json:"truncated,omitempty"`
	CacheID        string `json:"cache_id,omitempty"`
}

// ReportClaim is the sentence checked and the query searched for it.
type ReportClaim struct {
	Text  string `json:"text"`
	Query string `json:"query"`
}

// ReportSources is the evidence found for the claim, and what it cost.
type ReportSources struct {
	VerifierKeyID    string         `json:"verifier_virtual_key_id"`
	SearchDepth      string         `json:"search_depth"`
	Credits          float64        `json:"credits"`
	Reused           bool           `json:"reused"`                       // served from an earlier identical search (D34): no new credits
	GatewayRequestID string         `json:"gateway_request_id,omitempty"` // the relay call, in Bifrost's relay log
	TavilyRequestID  string         `json:"tavily_request_id,omitempty"`
	Sources          []ReportSource `json:"sources"`
}

// ReportSource is one search result.
type ReportSource struct {
	Title         string  `json:"title"`
	URL           string  `json:"url"`
	Score         float64 `json:"score"`
	PublishedDate string  `json:"published_date,omitempty"`
	Snippet       string  `json:"snippet"`
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
	}
}

// clone returns a deep copy, so readers never share the store's report.
func (r *Report) clone() Report {
	out := *r
	if r.CompletedAt != nil {
		t := *r.CompletedAt
		out.CompletedAt = &t
	}
	if r.Claim != nil {
		cl := *r.Claim
		out.Claim = &cl
	}
	if r.Evidence != nil {
		ev := *r.Evidence
		if r.Evidence.Sources != nil { // keep [] as [] in JSON
			ev.Sources = append(make([]ReportSource, 0, len(r.Evidence.Sources)), r.Evidence.Sources...)
		}
		out.Evidence = &ev
	}
	return out
}

// reportStore keeps the most recent reports in memory, evicting the oldest
// once full. Reports are lost on restart (a persistent store is later work).
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

func (s *reportStore) len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.order.Len()
}
