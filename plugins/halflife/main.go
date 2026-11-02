// Package halflife is a Bifrost plugin that audits completed LLM answers for
// stale claims, re-verifies them with Tavily through a governed Bifrost relay,
// and attributes the verification spend to the tenant that made the request.
//
// Hooks never change traffic: every hook returns its input unchanged. The
// plugin only observes, copying each completed answer out of the request
// (see capture.go) for auditing off the request path.
package halflife

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	bifrost "github.com/maximhq/bifrost/core"
	"github.com/maximhq/bifrost/core/schemas"
)

// PluginName is the name the gateway registers and configures the plugin by.
const PluginName = "halflife"

// HeaderReport carries the report ID on every captured response.
const HeaderReport = "X-Halflife-Report"

// newReportID returns a time-ordered ID (UUIDv7), so reports sort by creation.
func newReportID() string {
	id, err := uuid.NewV7()
	if err != nil { // only if the system clock or entropy source fails
		id = uuid.New()
	}
	return "hlr_" + id.String()
}

// Plugin implements schemas.LLMPlugin and schemas.HTTPTransportPlugin.
// It is registered once per process; per-request state lives in a slot on the
// request's context.
type Plugin struct {
	config Config
	logger schemas.Logger
	closed atomic.Bool

	pool *pool
	// sink receives each finalized capture and reports whether it was
	// accepted for audit. It runs on the request path, so it must not block.
	sink func(*Capture) bool

	captured, skippedFailed, skippedStreaming, skippedUnsupported atomic.Uint64
}

// Stats counts finalized requests by outcome.
type Stats struct {
	Captured           uint64
	SkippedFailed      uint64
	SkippedStreaming   uint64
	SkippedUnsupported uint64
}

// auditPlaceholder stands in for the verifier until it exists: it only marks
// that the capture reached a worker.
func (p *Plugin) auditPlaceholder(_ context.Context, c *Capture) error {
	p.logger.Debug("halflife: audit report=%s (no verifier yet)", c.ReportID)
	return nil
}

var (
	_ schemas.LLMPlugin           = (*Plugin)(nil)
	_ schemas.HTTPTransportPlugin = (*Plugin)(nil)
)

// Init validates config and returns a ready plugin. A nil config uses the
// defaults.
func Init(_ context.Context, config *Config, logger schemas.Logger) (*Plugin, error) {
	var cfg Config
	if config != nil {
		cfg = *config
	}
	cfg = cfg.withDefaults()
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	if logger == nil {
		logger = bifrost.NewNoOpLogger()
	}
	p := &Plugin{config: cfg, logger: logger}
	p.pool = newPool(cfg.Workers, cfg.QueueSize, cfg.auditTimeout(), p.auditPlaceholder)
	p.sink = p.pool.submit
	logger.Info("halflife: initialized (queue_size=%d workers=%d max_claims=%d tavily_relay=%s)",
		cfg.QueueSize, cfg.Workers, cfg.MaxClaimsPerResponse, cfg.TavilyRelay)
	return p, nil
}

// PoolStats returns the audit queue's counters.
func (p *Plugin) PoolStats() PoolStats { return p.pool.stats() }

// Stats returns the outcome counters since Init.
func (p *Plugin) Stats() Stats {
	return Stats{
		Captured:           p.captured.Load(),
		SkippedFailed:      p.skippedFailed.Load(),
		SkippedStreaming:   p.skippedStreaming.Load(),
		SkippedUnsupported: p.skippedUnsupported.Load(),
	}
}

// GetName returns PluginName.
func (p *Plugin) GetName() string { return PluginName }

// Config returns the effective configuration, defaults included.
func (p *Plugin) Config() Config { return p.config }

// Cleanup is called on gateway shutdown. It stops accepting captures, waits
// up to the drain timeout for queued audits, then cancels the rest. It is
// safe to call more than once.
func (p *Plugin) Cleanup() error {
	if !p.closed.CompareAndSwap(false, true) {
		return nil
	}
	drained := p.pool.close(p.config.drainTimeout())
	p.logger.Info("halflife: stopped (%s)", p.pool.stats())
	if !drained {
		return fmt.Errorf("halflife: audit workers still running after shutdown")
	}
	return nil
}

// LLM hooks. PreLLMHook and PostLLMHook run once per provider attempt, so
// fallbacks and retries call them more than once per request.

func (p *Plugin) PreRequestHook(_ *schemas.BifrostContext, _ *schemas.BifrostRequest) error {
	return nil
}

// PreLLMHook records the caller's question; it is the only hook that sees the
// request in Bifrost's typed form.
func (p *Plugin) PreLLMHook(ctx *schemas.BifrostContext, req *schemas.BifrostRequest) (*schemas.BifrostRequest, *schemas.LLMPluginShortCircuit, error) {
	if s := slotFrom(ctx); s != nil {
		if q, ok := questionFromRequest(req); ok {
			s.setQuestion(clip(q, maxQuestionBytes))
		}
	}
	return req, nil, nil
}

// PostLLMHook copies the answer of a successful attempt into the request's
// slot. Halflife runs last among post-hooks, so this is the response the
// client receives for this attempt. For streams it runs once per chunk; those
// requests are only marked.
func (p *Plugin) PostLLMHook(ctx *schemas.BifrostContext, resp *schemas.BifrostResponse, bifrostErr *schemas.BifrostError) (*schemas.BifrostResponse, *schemas.BifrostError, error) {
	s := slotFrom(ctx)
	switch {
	case s == nil:
	case ctx.Value(schemas.BifrostContextKeyStreamStartTime) != nil:
		s.record(nil, SkipStreaming)
	case bifrostErr != nil || resp == nil:
		s.record(nil, SkipFailed)
	default:
		c, reason := captureFromResponse(resp)
		if c != nil {
			identify(c, ctx)
		}
		// Core layers IsFallback onto RoutingInfo only after post-hooks run, so
		// read the attempt's fallback index instead (0 = primary).
		if idx, _ := ctx.Value(schemas.BifrostContextKeyFallbackIndex).(int); c != nil && idx > 0 {
			c.IsFallback = true
		}
		s.record(c, reason)
	}
	return resp, bifrostErr, nil
}

// HTTP transport hooks. HTTPTransportPostHook runs once per request, after
// fallbacks are resolved, which makes it the place to capture the final answer.

func (p *Plugin) HTTPTransportPreAuthHook(_ *schemas.BifrostContext, _ *schemas.HTTPRequest) (*schemas.HTTPResponse, error) {
	return nil, nil
}

// HTTPTransportPreHook opens the request's slot. The gateway copies context
// values set here into the core context, so the LLM hooks find the same slot.
func (p *Plugin) HTTPTransportPreHook(ctx *schemas.BifrostContext, _ *schemas.HTTPRequest) (*schemas.HTTPResponse, error) {
	ctx.SetValue(slotKey{}, &slot{})
	return nil, nil
}

// HTTPTransportPostHook finalizes the request's capture, once.
func (p *Plugin) HTTPTransportPostHook(ctx *schemas.BifrostContext, _ *schemas.HTTPRequest, resp *schemas.HTTPResponse) error {
	s := slotFrom(ctx)
	if s == nil || resp == nil {
		return nil
	}
	c, reason, ok := s.finalize(resp.StatusCode, newReportID)
	if !ok {
		return nil
	}
	switch reason {
	case SkipNone:
		c.CapturedAt = time.Now().UTC()
		if c.RequestID == "" {
			c.RequestID = strings.Clone(headerValue(resp.Headers, "X-Bifrost-Request-Id"))
		}
		p.captured.Add(1)
		p.logger.Debug("halflife: captured %s answer from %s/%s (%d bytes, fallback=%t) report=%s request=%s vk=%s",
			c.RequestType, c.Provider, c.Model, len(c.Answer), c.IsFallback, c.ReportID, c.RequestID, c.VirtualKeyID)
		if !p.sink(c) {
			// No report will exist, so the client gets no report header.
			s.clearReport()
			p.logger.Warn("halflife: audit queue full, report %s dropped", c.ReportID)
		}
	case SkipFailed:
		p.skippedFailed.Add(1)
	case SkipStreaming:
		p.skippedStreaming.Add(1)
		p.logger.Debug("halflife: streaming response not audited")
	case SkipUnsupported:
		p.skippedUnsupported.Add(1)
	}
	return nil
}

// HTTPTransportResponseHeadersHook tells the client where the audit report
// for this answer will be. For buffered responses the gateway runs it after
// HTTPTransportPostHook, so the capture decision has been made. Streaming
// responses get no header: they are not captured.
func (p *Plugin) HTTPTransportResponseHeadersHook(ctx *schemas.BifrostContext, _ *schemas.HTTPRequest, resp *schemas.HTTPResponseMetadata) error {
	s := slotFrom(ctx)
	if s == nil || resp == nil || resp.Headers == nil {
		return nil
	}
	id := s.report()
	if id == "" {
		return nil
	}
	resp.Headers[HeaderReport] = id
	// Let browser clients (such as the dashboard) read the header cross-origin.
	if key, v, found := headerEntry(resp.Headers, "Access-Control-Expose-Headers"); found && !strings.Contains(strings.ToLower(v), strings.ToLower(HeaderReport)) {
		resp.Headers[key] = v + ", " + strings.ToLower(HeaderReport)
	}
	return nil
}

// headerEntry finds a header case-insensitively and returns its stored key.
func headerEntry(h map[string]string, name string) (key, value string, found bool) {
	if v, ok := h[name]; ok {
		return name, v, true
	}
	for k, v := range h {
		if strings.EqualFold(k, name) {
			return k, v, true
		}
	}
	return "", "", false
}

func headerValue(h map[string]string, name string) string {
	_, v, _ := headerEntry(h, name)
	return v
}

func (p *Plugin) HTTPTransportStreamChunkHook(_ *schemas.BifrostContext, _ *schemas.HTTPRequest, chunk *schemas.BifrostStreamChunk) (*schemas.BifrostStreamChunk, error) {
	return chunk, nil
}
