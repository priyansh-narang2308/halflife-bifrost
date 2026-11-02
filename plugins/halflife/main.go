// Package halflife is a Bifrost plugin that audits completed LLM answers for
// stale claims, re-verifies them with Tavily through a governed Bifrost relay,
// and attributes the verification spend to the tenant that made the request.
//
// This version is the skeleton: every hook passes its input through
// unchanged. Response capture, auditing and cache freshness are added on top
// of these hooks.
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
// It is registered once per process and holds no per-request state.
type Plugin struct {
	config Config
	logger schemas.Logger
	closed atomic.Bool
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
	return &Plugin{config: cfg, logger: logger}, nil
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

func (p *Plugin) PreLLMHook(_ *schemas.BifrostContext, req *schemas.BifrostRequest) (*schemas.BifrostRequest, *schemas.LLMPluginShortCircuit, error) {
	return req, nil, nil
}

func (p *Plugin) PostLLMHook(_ *schemas.BifrostContext, resp *schemas.BifrostResponse, bifrostErr *schemas.BifrostError) (*schemas.BifrostResponse, *schemas.BifrostError, error) {
	return resp, bifrostErr, nil
}

// HTTP transport hooks. HTTPTransportPostHook runs once per request, after
// fallbacks are resolved, which makes it the place to capture the final answer.

func (p *Plugin) HTTPTransportPreAuthHook(_ *schemas.BifrostContext, _ *schemas.HTTPRequest) (*schemas.HTTPResponse, error) {
	return nil, nil
}

func (p *Plugin) HTTPTransportPreHook(_ *schemas.BifrostContext, _ *schemas.HTTPRequest) (*schemas.HTTPResponse, error) {
	return nil, nil
}

func (p *Plugin) HTTPTransportPostHook(_ *schemas.BifrostContext, _ *schemas.HTTPRequest, _ *schemas.HTTPResponse) error {
	return nil
}

func (p *Plugin) HTTPTransportResponseHeadersHook(_ *schemas.BifrostContext, _ *schemas.HTTPRequest, _ *schemas.HTTPResponseMetadata) error {
	return nil
}

func (p *Plugin) HTTPTransportStreamChunkHook(_ *schemas.BifrostContext, _ *schemas.HTTPRequest, chunk *schemas.BifrostStreamChunk) (*schemas.BifrostStreamChunk, error) {
	return chunk, nil
}
