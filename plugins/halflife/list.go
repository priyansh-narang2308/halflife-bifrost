package halflife

import (
	"strings"
	"time"
)

// Report listing (Monitor, audit §5.2): newest first, filtered by tenant,
// model, verdict, outcome or request, paged by report ID (IDs are UUIDv7,
// so they sort by creation time).

const (
	DefaultReportListLimit = 50
	MaxReportListLimit     = 500
)

// ReportFilter selects reports. Empty fields match everything.
type ReportFilter struct {
	Tenant    string // tenant virtual key ID
	Provider  string
	Model     string
	Verdict   string
	Outcome   string
	RequestID string
	Before    string // only reports with an ID below this one (the next page)
	Limit     int    // DefaultReportListLimit when 0, at most MaxReportListLimit
}

// ReportSummary is one report in a list.
type ReportSummary struct {
	ID            string     `json:"id"`
	RequestID     string     `json:"request_id"`
	Status        string     `json:"status"`
	Outcome       string     `json:"outcome,omitempty"`
	Verdict       string     `json:"verdict"`
	CreatedAt     time.Time  `json:"created_at"`
	CompletedAt   *time.Time `json:"completed_at,omitempty"`
	Tenant        string     `json:"tenant_virtual_key_id"`
	Provider      string     `json:"provider"`
	Model         string     `json:"model"`
	Claims        int        `json:"claims"`
	ClaimsChecked int        `json:"claims_checked"`
	Credits       float64    `json:"credits"`
}

func (f ReportFilter) limit() int {
	switch {
	case f.Limit <= 0:
		return DefaultReportListLimit
	case f.Limit > MaxReportListLimit:
		return MaxReportListLimit
	}
	return f.Limit
}

func (f ReportFilter) matches(r *Report) bool {
	return (f.Tenant == "" || r.Tenant.VirtualKeyID == f.Tenant) &&
		(f.Provider == "" || strings.EqualFold(r.Answer.Provider, f.Provider)) &&
		(f.Model == "" || r.Answer.Model == f.Model) &&
		(f.Verdict == "" || strings.EqualFold(r.Verdict, f.Verdict)) &&
		(f.Outcome == "" || r.Outcome == f.Outcome) &&
		(f.RequestID == "" || r.RequestID == f.RequestID) &&
		(f.Before == "" || r.ID < f.Before)
}

func summarise(r *Report) ReportSummary {
	s := ReportSummary{ID: r.ID, RequestID: r.RequestID, Status: r.Status, Outcome: r.Outcome, Verdict: r.Verdict,
		CreatedAt: r.CreatedAt, Tenant: r.Tenant.VirtualKeyID, Provider: r.Answer.Provider, Model: r.Answer.Model, Claims: len(r.Claims)}
	if r.CompletedAt != nil {
		t := *r.CompletedAt
		s.CompletedAt = &t
	}
	for _, c := range r.Claims {
		if c.Status == ClaimChecked {
			s.ClaimsChecked++
		}
		if c.Evidence != nil {
			s.Credits += c.Evidence.Credits
		}
	}
	return s
}

func (s *reportStore) list(f ReportFilter) []ReportSummary {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []ReportSummary{}
	for e := s.order.Back(); e != nil && len(out) < f.limit(); e = e.Prev() {
		if r := e.Value.(*Report); f.matches(r) {
			out = append(out, summarise(r))
		}
	}
	return out
}

// Reports lists reports, newest first.
func (p *Plugin) Reports(f ReportFilter) []ReportSummary { return p.reports.list(f) }
