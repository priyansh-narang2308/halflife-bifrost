package halflife

import (
	"context"
	"testing"
	"time"

	"github.com/maximhq/bifrost/core/schemas"
)

func newPlugin(t *testing.T) *Plugin {
	t.Helper()
	p, err := Init(context.Background(), nil, nil)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() { p.Cleanup() })
	return p
}

func newCtx() *schemas.BifrostContext {
	return schemas.NewBifrostContext(context.Background(), time.Now().Add(time.Minute))
}

func TestInit(t *testing.T) {
	p := newPlugin(t)
	if p.GetName() != "halflife" {
		t.Errorf("GetName() = %q", p.GetName())
	}
	if p.Config() != (Config{}).withDefaults() {
		t.Errorf("nil config did not apply defaults: %+v", p.Config())
	}
	if _, err := Init(context.Background(), &Config{Workers: 100}, nil); err == nil {
		t.Error("Init accepted workers=100")
	}
	p, err := Init(context.Background(), &Config{Workers: 3}, nil)
	if err != nil || p.Config().Workers != 3 || p.Config().QueueSize != DefaultQueueSize {
		t.Errorf("partial config: %+v, %v", p.Config(), err)
	}
}

// The gateway decides which hooks to call by type assertion
// (transports/bifrost-http/server/plugins.go, InferPluginTypes).
func TestImplementsGatewayInterfaces(t *testing.T) {
	var p schemas.BasePlugin = newPlugin(t)
	if _, ok := p.(schemas.LLMPlugin); !ok {
		t.Error("not an LLMPlugin")
	}
	if _, ok := p.(schemas.HTTPTransportPlugin); !ok {
		t.Error("not an HTTPTransportPlugin")
	}
	for name, ok := range map[string]bool{
		"MCPPlugin":   isA[schemas.MCPPlugin](p),
		"RelayPlugin": isA[schemas.RelayPlugin](p),
	} {
		if ok {
			t.Errorf("unexpectedly implements %s; the gateway would call its hooks", name)
		}
	}
}

func isA[T any](v any) bool { _, ok := v.(T); return ok }

func TestLLMHooksPassThrough(t *testing.T) {
	p, ctx := newPlugin(t), newCtx()
	req := &schemas.BifrostRequest{RequestType: schemas.ChatCompletionRequest, ChatRequest: &schemas.BifrostChatRequest{Provider: schemas.OpenAI, Model: "ok"}}

	if err := p.PreRequestHook(ctx, req); err != nil {
		t.Errorf("PreRequestHook: %v", err)
	}
	gotReq, sc, err := p.PreLLMHook(ctx, req)
	if gotReq != req || sc != nil || err != nil {
		t.Errorf("PreLLMHook = %p, %v, %v; want same request, nil, nil", gotReq, sc, err)
	}

	resp := &schemas.BifrostResponse{}
	bErr := &schemas.BifrostError{Error: &schemas.ErrorField{Message: "boom"}}
	for _, tc := range []struct {
		resp *schemas.BifrostResponse
		err  *schemas.BifrostError
	}{{resp, nil}, {nil, bErr}, {nil, nil}} {
		r, e, hookErr := p.PostLLMHook(ctx, tc.resp, tc.err)
		if r != tc.resp || e != tc.err || hookErr != nil {
			t.Errorf("PostLLMHook(%p, %p) = %p, %p, %v", tc.resp, tc.err, r, e, hookErr)
		}
	}
}

func TestHTTPHooksPassThrough(t *testing.T) {
	p, ctx := newPlugin(t), newCtx()
	req := &schemas.HTTPRequest{Method: "POST", Path: "/v1/chat/completions", Headers: map[string]string{"x-bf-vk": "k"}, Body: []byte(`{"a":1}`)}
	resp := &schemas.HTTPResponse{StatusCode: 200, Headers: map[string]string{"content-type": "application/json"}, Body: []byte(`{"b":2}`)}

	if r, err := p.HTTPTransportPreAuthHook(ctx, req); r != nil || err != nil {
		t.Errorf("PreAuthHook = %v, %v", r, err)
	}
	if r, err := p.HTTPTransportPreHook(ctx, req); r != nil || err != nil {
		t.Errorf("PreHook = %v, %v", r, err)
	}
	if err := p.HTTPTransportPostHook(ctx, req, resp); err != nil {
		t.Errorf("PostHook: %v", err)
	}
	if string(resp.Body) != `{"b":2}` || resp.StatusCode != 200 || len(resp.Headers) != 1 {
		t.Errorf("PostHook modified response: %+v", resp)
	}
	if string(req.Body) != `{"a":1}` || len(req.Headers) != 1 {
		t.Errorf("hooks modified request: %+v", req)
	}
	meta := &schemas.HTTPResponseMetadata{StatusCode: 200, Headers: map[string]string{}}
	if err := p.HTTPTransportResponseHeadersHook(ctx, req, meta); err != nil || len(meta.Headers) != 0 {
		t.Errorf("ResponseHeadersHook: %v, headers %v", err, meta.Headers)
	}
	chunk := &schemas.BifrostStreamChunk{}
	if got, err := p.HTTPTransportStreamChunkHook(ctx, req, chunk); got != chunk || err != nil {
		t.Errorf("StreamChunkHook = %p, %v; want same chunk", got, err)
	}
}

func TestCleanupIdempotent(t *testing.T) {
	p := newPlugin(t)
	for i := 0; i < 3; i++ {
		if err := p.Cleanup(); err != nil {
			t.Fatalf("Cleanup #%d: %v", i+1, err)
		}
	}
}
