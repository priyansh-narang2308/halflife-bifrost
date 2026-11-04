package halflife

import (
	"strings"
	"time"

	"github.com/maximhq/bifrost/core/schemas"
)

// Modes (audit §5.2), selected by Config.Mode.
//
//	audit    (default) Answers are verified off the request path; nothing
//	         waits for verification. The semantic cache TTL is capped, an
//	         answer found stale is evicted, and responses carry
//	         X-Halflife-Report / X-Halflife-Status. The first user to get a
//	         stale answer still gets it.
//	monitor  Observe only. Answers are verified and reported exactly as in
//	         audit, but Halflife never touches the response or the cache: no
//	         headers, no TTL cap, no eviction, no bypass. Reports are found
//	         through GET /api/halflife/reports (by request_id, tenant, model).
//	protect  Audit, plus synchronous verification for requests that ask
//	         for it with X-Halflife-Protect: true (every auditable request
//	         with protect_all). The response waits up to protect_timeout_ms
//	         for the verdict, then goes out with X-Halflife-Verdict, or
//	         without it and X-Halflife-Status: verify-timeout (fail-open: the
//	         audit carries on and completes the report) or verify-failed (the
//	         audit ended without a verdict). The body is never changed.
//	         Streams are not protected (they are not audited).
const (
	ModeAudit   = "audit"
	ModeMonitor = "monitor"
	ModeProtect = "protect"
)

// HeaderProtect opts a request into protect mode; HeaderVerdict carries the
// answer's verdict when it was reached in time.
const (
	HeaderProtect = "X-Halflife-Protect"
	HeaderVerdict = "X-Halflife-Verdict"
)

// X-Halflife-Status values in protect mode.
const (
	statusVerified      = "verified"
	statusVerifyTimeout = "verify-timeout"
	statusVerifyFailed  = "verify-failed"
)

// touchesTraffic reports whether Halflife may add headers or act on the
// semantic cache: every mode but monitor.
func (p *Plugin) touchesTraffic() bool { return p.config.Mode != ModeMonitor }

// protects reports whether req asked for (or gets) synchronous verification.
func (p *Plugin) protects(req *schemas.HTTPRequest) bool {
	if p.config.Mode != ModeProtect {
		return false
	}
	if p.config.ProtectAll {
		return true
	}
	switch strings.ToLower(strings.TrimSpace(req.CaseInsensitiveHeaderLookup(HeaderProtect))) {
	case "true", "1", "yes":
		return true
	}
	return false
}

// awaitVerdict holds a protected request until its audit completes or the
// protect timeout passes, and records what the headers will say.
func (p *Plugin) awaitVerdict(s *slot, done <-chan struct{}, reportID string) {
	timer := time.NewTimer(time.Duration(p.config.ProtectTimeoutMs) * time.Millisecond)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
		p.protectTimeouts.Add(1)
		s.setProtected(statusVerifyTimeout, "")
		return
	}
	r, ok := p.reports.get(reportID)
	if !ok || r.Status != ReportComplete {
		s.setProtected(statusVerifyFailed, "")
		return
	}
	p.protectVerified.Add(1)
	s.setProtected(statusVerified, r.Verdict)
}

// protectCachedHit gives a protected cache hit the verdict of the report it
// resolved to, when that report is already complete; it never waits.
func (p *Plugin) protectCachedHit(s *slot, reportID string) {
	if r, ok := p.reports.get(reportID); ok && r.Status == ReportComplete {
		s.setProtected(statusCached, r.Verdict)
	}
}
