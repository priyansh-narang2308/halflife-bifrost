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
	"sync/atomic"

	bifrost "github.com/maximhq/bifrost/core"
	"github.com/maximhq/bifrost/core/schemas"
)

// PluginName is the name the gateway registers and configures the plugin by.
const PluginName = "halflife"

// Plugin implements schemas.LLMPlugin and schemas.HTTPTransportPlugin.
// It is registered once per process; per-request state lives in a slot on the
// request's context.
type Plugin struct {
	config Config
	logger schemas.Logger
	closed atomic.Bool

	// sink receives each finalized capture. It runs on the request path, so it
	// must not block.
	sink func(*Capture)

	captured, skippedFailed, skippedStreaming, skippedUnsupported atomic.Uint64
}

// Stats counts finalized requests by outcome.
type Stats struct {
	Captured           uint64
	SkippedFailed      uint64
	SkippedStreaming   uint64
	SkippedUnsupported uint64
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
	logger.Info("halflife: initialized (queue_size=%d workers=%d max_claims=%d tavily_relay=%s)",
		cfg.QueueSize, cfg.Workers, cfg.MaxClaimsPerResponse, cfg.TavilyRelay)
	return &Plugin{config: cfg, logger: logger, sink: func(*Capture) {}}, nil
}

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

// Cleanup is called on gateway shutdown. It is safe to call more than once.
func (p *Plugin) Cleanup() error {
	if p.closed.CompareAndSwap(false, true) {
		p.logger.Info("halflife: stopped")
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
	c, reason, ok := s.finalize(resp.StatusCode)
	if !ok {
		return nil
	}
	switch reason {
	case SkipNone:
		p.captured.Add(1)
		p.logger.Debug("halflife: captured %s answer from %s/%s (%d bytes, fallback=%t)",
			c.RequestType, c.Provider, c.Model, len(c.Answer), c.IsFallback)
		p.sink(c)
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

func (p *Plugin) HTTPTransportResponseHeadersHook(_ *schemas.BifrostContext, _ *schemas.HTTPRequest, _ *schemas.HTTPResponseMetadata) error {
	return nil
}

func (p *Plugin) HTTPTransportStreamChunkHook(_ *schemas.BifrostContext, _ *schemas.HTTPRequest, chunk *schemas.BifrostStreamChunk) (*schemas.BifrostStreamChunk, error) {
	return chunk, nil
}
