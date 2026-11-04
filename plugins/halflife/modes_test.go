package halflife

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/maximhq/bifrost/core/schemas"
)

// protectPlugin is a plugin in mode whose audits wait for release (when it
// is not nil), then complete the report with verdict.
func protectPlugin(t *testing.T, cfg Config, release chan struct{}, verdict string) *Plugin {
	t.Helper()
	p, err := Init(t.Context(), &cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Cleanup() })
	p.verify = func(ctx context.Context, c *Capture) (bool, error) {
		if release != nil {
			select {
			case <-release:
			case <-ctx.Done():
				return false, ctx.Err()
			}
		}
		if verdict == "" {
			return false, errors.New("verifier failed")
		}
		done := time.Now()
		p.reports.update(c.ReportID, func(r *Report) { r.Status, r.Verdict, r.CompletedAt = ReportComplete, verdict, &done })
		return false, nil
	}
	return p
}

var protectOn = map[string]string{"x-halflife-protect": "true"}

func protectedHeaders(t *testing.T, p *Plugin, headers map[string]string) (map[string]string, time.Duration) {
	t.Helper()
	r := newRequestWith(t, p, headers)
	r.attempt(chatReq("q"), chatResp("a", "ok"), nil)
	start := time.Now()
	h := r.finish(200)
	return h, time.Since(start)
}

func TestProtectWaitsForTheVerdict(t *testing.T) {
	p := protectPlugin(t, Config{Mode: ModeProtect}, nil, VerdictOutdated)
	h, _ := protectedHeaders(t, p, protectOn)
	if h[HeaderStatus] != statusVerified || h[HeaderVerdict] != VerdictOutdated || h[HeaderReport] == "" {
		t.Errorf("headers %v", h)
	}
	if !strings.Contains(h["Access-Control-Expose-Headers"], "x-halflife-verdict") {
		t.Errorf("verdict header not exposed: %q", h["Access-Control-Expose-Headers"])
	}
	if s := p.Stats(); s.ProtectVerified != 1 || s.ProtectTimeouts != 0 {
		t.Errorf("stats %+v", s)
	}
}

func TestProtectIsOptIn(t *testing.T) {
	for name, tc := range map[string]struct {
		cfg     Config
		headers map[string]string
		want    bool
	}{
		"protect, header true":       {Config{Mode: ModeProtect}, protectOn, true},
		"protect, header 1":          {Config{Mode: ModeProtect}, map[string]string{"X-Halflife-Protect": "1"}, true},
		"protect, header false":      {Config{Mode: ModeProtect}, map[string]string{"X-Halflife-Protect": "false"}, false},
		"protect, no header":         {Config{Mode: ModeProtect}, nil, false},
		"protect_all, no header":     {Config{Mode: ModeProtect, ProtectAll: true}, nil, true},
		"audit ignores the header":   {Config{Mode: ModeAudit}, protectOn, false},
		"monitor ignores the header": {Config{Mode: ModeMonitor}, protectOn, false},
	} {
		release := make(chan struct{})
		p := protectPlugin(t, tc.cfg, release, VerdictCurrent)
		r := newRequestWith(t, p, tc.headers)
		if got := slotFrom(r.core).protect; got != tc.want {
			t.Errorf("%s: protect = %v", name, got)
		}
		close(release)
	}
}

// Unprotected requests never wait, even while the audit is still running.
func TestUnprotectedRequestDoesNotWait(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	p := protectPlugin(t, Config{Mode: ModeProtect, ProtectTimeoutMs: 5000}, release, VerdictCurrent)
	h, took := protectedHeaders(t, p, nil)
	if took > time.Second || h[HeaderStatus] != statusQueued || h[HeaderVerdict] != "" {
		t.Errorf("took %s, headers %v", took, h)
	}
}

// Fail-open: past the timeout the response goes out without a verdict, and
// the audit carries on to complete the report.
func TestProtectTimeoutFailsOpen(t *testing.T) {
	release := make(chan struct{})
	p := protectPlugin(t, Config{Mode: ModeProtect, ProtectTimeoutMs: 100}, release, VerdictContradicted)
	h, took := protectedHeaders(t, p, protectOn)
	if took < 100*time.Millisecond || took > 2*time.Second {
		t.Errorf("waited %s for a 100ms timeout", took)
	}
	if h[HeaderStatus] != statusVerifyTimeout || h[HeaderVerdict] != "" || h[HeaderReport] == "" {
		t.Errorf("headers %v", h)
	}
	close(release)
	id := h[HeaderReport]
	eventually(t, "the report to complete", func() bool { r, _ := p.Report(id); return r.Status == ReportComplete })
	if r, _ := p.Report(id); r.Verdict != VerdictContradicted {
		t.Errorf("report verdict %s", r.Verdict)
	}
	if s := p.Stats(); s.ProtectTimeouts != 1 || s.ProtectVerified != 0 {
		t.Errorf("stats %+v", s)
	}
}

func TestProtectAuditFailure(t *testing.T) {
	p := protectPlugin(t, Config{Mode: ModeProtect}, nil, "")
	h, took := protectedHeaders(t, p, protectOn)
	if took > time.Second || h[HeaderStatus] != statusVerifyFailed || h[HeaderVerdict] != "" {
		t.Errorf("took %s, headers %v", took, h)
	}
}

// A cache hit gets the verdict of the report it resolves to, without waiting.
func TestProtectCachedHit(t *testing.T) {
	p := protectPlugin(t, Config{Mode: ModeProtect}, nil, VerdictOutdated)
	first := newRequestWith(t, p, protectOn)
	first.attempt(chatReq("q"), cachedResp("entry-1", false), nil)
	h := first.finish(200)
	if h[HeaderVerdict] != VerdictOutdated {
		t.Fatalf("first: %v", h)
	}
	hit := newRequestWith(t, p, protectOn)
	hit.attempt(chatReq("q"), cachedResp("entry-1", true), nil)
	h = hit.finish(200)
	if h[HeaderStatus] != statusCached || h[HeaderVerdict] != VerdictOutdated || h[HeaderReport] != first.headers(200)[HeaderReport] {
		t.Errorf("hit: %v", h)
	}
}

// cachedResp is an answer as the semantic cache stamps it: written to entry
// id on a miss, served from it on a hit.
func cachedResp(id string, hit bool) *schemas.BifrostResponse {
	resp := chatResp("a", "ok")
	resp.ChatResponse.ExtraFields.CacheDebug = &schemas.BifrostCacheMetadata{CacheID: schemas.Ptr(id), CacheHit: hit}
	return resp
}

// Monitor observes only: the answer is audited and reported, but the
// response and the cache are left exactly as they were.
func TestMonitorLeavesTrafficAlone(t *testing.T) {
	st := newStack(t, stackOptions{halflife: &Config{Mode: ModeMonitor, CacheTTLSeconds: 10}})
	var audited atomic.Int32
	st.hl.verify = func(context.Context, *Capture) (bool, error) { audited.Add(1); return true, nil } // always stale
	var evicts atomic.Int32
	st.hl.SetCacheEvictor(func(string) error { evicts.Add(1); return nil })

	first := st.ask("vk-a", priceQ)
	if len(first.headers) != 1 {
		t.Errorf("monitor added headers: %v", first.headers)
	}
	st.settle()
	// The cache's own TTL, not Halflife's cap.
	if got := st.store.expiresIn(t, first.cacheID()); got < 50*time.Second {
		t.Errorf("entry expires in %s: the TTL was capped", got.Round(time.Second))
	}
	eventually(t, "the audit", func() bool { return st.hl.PoolStats().Audited == 1 })
	second := st.ask("vk-a", priceQ)
	if second.hit() == "miss" || second.answer != first.answer || len(second.headers) != 1 {
		t.Errorf("second request: hit %s, answer %q, headers %v", second.hit(), second.answer, second.headers)
	}
	if evicts.Load() != 0 || st.hl.Stats().Invalidated != 0 || st.hl.Stats().BypassedRequests != 0 {
		t.Errorf("monitor touched the cache: %d evictions, stats %+v", evicts.Load(), st.hl.Stats())
	}
	// The audit still ran and is reported.
	if audited.Load() != 1 || len(st.hl.Reports(ReportFilter{Tenant: "vk-a"})) != 1 {
		t.Errorf("audited %d, reports %v", audited.Load(), st.hl.Reports(ReportFilter{}))
	}
}
