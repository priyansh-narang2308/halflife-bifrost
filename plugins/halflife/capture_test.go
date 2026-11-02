package halflife

import (
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unsafe"

	"github.com/google/uuid"
	"github.com/maximhq/bifrost/core/schemas"
)

// request drives the plugin through one HTTP request the way the gateway does
// (observed in Task 10): the transport hooks and the core LLM hooks get
// different root contexts, values set by the transport pre-hook are copied
// into the core context, nothing flows back, and every hook gets a
// plugin-scoped context that is released when the hook returns.
type request struct {
	t         *testing.T
	p         *Plugin
	transport *schemas.BifrostContext
	core      *schemas.BifrostContext
	attempts  int
}

func newRequest(t *testing.T, p *Plugin) *request {
	t.Helper()
	r := &request{t: t, p: p, transport: newCtx()}
	scoped := r.scoped(r.transport)
	if resp, err := p.HTTPTransportPreHook(scoped, &schemas.HTTPRequest{Method: "POST", Path: "/v1/chat/completions"}); resp != nil || err != nil {
		t.Fatalf("HTTPTransportPreHook = %v, %v", resp, err)
	}
	scoped.ReleasePluginScope()
	r.core = newCtx()
	for k, v := range r.transport.GetUserValues() {
		r.core.SetValue(k, v)
	}
	// What the transport (request ID, raw key header) and governance
	// (resolved virtual key) put on the core context.
	r.core.SetValue(schemas.BifrostContextKeyRequestID, "req-1")
	r.core.SetValue(schemas.BifrostContextKeyVirtualKey, "sk-bf-secret-value")
	r.core.SetValue(schemas.BifrostContextKeyGovernanceVirtualKeyID, "vk-smoke")
	r.core.SetValue(schemas.BifrostContextKeyGovernanceVirtualKeyName, "Smoke test app")
	return r
}

func (r *request) scoped(root *schemas.BifrostContext) *schemas.BifrostContext {
	name := PluginName
	return root.WithPluginScope(&name)
}

// attempt runs one provider attempt through the LLM hooks. Like core, it
// numbers attempts in the context: 0 for the primary, 1+ for fallbacks.
func (r *request) attempt(req *schemas.BifrostRequest, resp *schemas.BifrostResponse, bErr *schemas.BifrostError) {
	r.t.Helper()
	r.core.SetValue(schemas.BifrostContextKeyFallbackIndex, r.attempts)
	r.attempts++
	scoped := r.scoped(r.core)
	if got, _, _ := r.p.PreLLMHook(scoped, req); got != req {
		r.t.Fatal("PreLLMHook replaced the request")
	}
	scoped.ReleasePluginScope()
	scoped = r.scoped(r.core)
	gotResp, gotErr, _ := r.p.PostLLMHook(scoped, resp, bErr)
	if gotResp != resp || gotErr != bErr {
		r.t.Fatal("PostLLMHook replaced the response or error")
	}
	scoped.ReleasePluginScope()
}

// finish runs the transport post-hook and then the response-headers hook, in
// the gateway's order for buffered responses, and returns the final headers.
func (r *request) finish(status int) map[string]string {
	r.t.Helper()
	scoped := r.scoped(r.transport)
	if err := r.p.HTTPTransportPostHook(scoped, &schemas.HTTPRequest{}, &schemas.HTTPResponse{StatusCode: status}); err != nil {
		r.t.Fatalf("HTTPTransportPostHook: %v", err)
	}
	scoped.ReleasePluginScope()
	return r.headers(status)
}

func (r *request) headers(status int) map[string]string {
	r.t.Helper()
	meta := &schemas.HTTPResponseMetadata{StatusCode: status, Headers: map[string]string{
		"Content-Type":                  "application/json",
		"Access-Control-Expose-Headers": "x-bifrost-request-id, x-request-id",
	}}
	scoped := r.scoped(r.transport)
	if err := r.p.HTTPTransportResponseHeadersHook(scoped, &schemas.HTTPRequest{}, meta); err != nil {
		r.t.Fatalf("HTTPTransportResponseHeadersHook: %v", err)
	}
	scoped.ReleasePluginScope()
	return meta.Headers
}

// recordingPlugin returns a plugin whose sink appends to the returned slice.
func recordingPlugin(t *testing.T) (*Plugin, *[]*Capture) {
	t.Helper()
	p := newPlugin(t)
	var mu sync.Mutex
	got := &[]*Capture{}
	p.sink = func(c *Capture) { mu.Lock(); *got = append(*got, c); mu.Unlock() }
	return p, got
}

func chatReq(question string) *schemas.BifrostRequest {
	return &schemas.BifrostRequest{RequestType: schemas.ChatCompletionRequest, ChatRequest: &schemas.BifrostChatRequest{
		Provider: schemas.OpenAI, Model: "ok",
		Input: []schemas.ChatMessage{
			{Role: schemas.ChatMessageRoleSystem, Content: &schemas.ChatMessageContent{ContentStr: schemas.Ptr("be brief")}},
			{Role: schemas.ChatMessageRoleUser, Content: &schemas.ChatMessageContent{ContentStr: schemas.Ptr(question)}},
		},
	}}
}

// chatResp is a response as PostLLMHook sees it. RoutingInfo.IsFallback is
// never set at that point: core layers it on after the post-hooks (observed
// in Task 10), and marks fallback attempts with BifrostContextKeyFallbackIndex.
func chatResp(answer, model string) *schemas.BifrostResponse {
	return &schemas.BifrostResponse{ChatResponse: &schemas.BifrostChatResponse{
		Model: model,
		Choices: []schemas.BifrostResponseChoice{{ChatNonStreamResponseChoice: &schemas.ChatNonStreamResponseChoice{
			Message: &schemas.ChatMessage{Role: schemas.ChatMessageRoleAssistant, Content: &schemas.ChatMessageContent{ContentStr: schemas.Ptr(answer)}},
		}}},
		ExtraFields: schemas.BifrostResponseExtraFields{
			RequestType: schemas.ChatCompletionRequest,
			RoutingInfo: schemas.RoutingInfo{Provider: schemas.OpenAI, Model: model, RequestedModel: "fail-500"},
		},
	}}
}

var errAttempt = &schemas.BifrostError{StatusCode: schemas.Ptr(500), Error: &schemas.ErrorField{Message: "upstream failed"}}

func TestCaptureSuccessfulRequest(t *testing.T) {
	p, got := recordingPlugin(t)
	r := newRequest(t, p)
	r.attempt(chatReq("How much is Acme Pro?"), chatResp("Acme Pro costs $20.", "ok"), nil)
	if len(*got) != 0 {
		t.Fatal("captured before the HTTP post-hook")
	}
	r.finish(200)

	if len(*got) != 1 {
		t.Fatalf("captures = %d, want 1", len(*got))
	}
	c := (*got)[0]
	if c.CapturedAt.IsZero() || !strings.HasPrefix(c.ReportID, "hlr_") {
		t.Errorf("identity not assigned: report=%q at=%v", c.ReportID, c.CapturedAt)
	}
	generated := *c
	generated.ReportID, generated.CapturedAt = "", time.Time{}
	want := Capture{RequestID: "req-1", VirtualKeyID: "vk-smoke", VirtualKeyName: "Smoke test app",
		RequestType: schemas.ChatCompletionRequest, Provider: schemas.OpenAI, Model: "ok", RequestedModel: "fail-500",
		Question: "How much is Acme Pro?", Answer: "Acme Pro costs $20."}
	if generated != want {
		t.Errorf("capture = %+v\nwant      %+v", generated, want)
	}
	if s := p.Stats(); s != (Stats{Captured: 1}) {
		t.Errorf("stats = %+v", s)
	}
}

func TestCaptureFallbackKeepsOnlySuccessfulAttempt(t *testing.T) {
	p, got := recordingPlugin(t)
	r := newRequest(t, p)
	r.attempt(chatReq("q"), nil, errAttempt)
	r.attempt(chatReq("q"), chatResp("from fallback", "ok"), nil)
	r.finish(200)
	if len(*got) != 1 || (*got)[0].Answer != "from fallback" || !(*got)[0].IsFallback {
		t.Fatalf("captures = %+v", *got)
	}
}

func TestNoCaptureWhenRequestFails(t *testing.T) {
	for name, run := range map[string]func(r *request){
		"all attempts fail": func(r *request) {
			r.attempt(chatReq("q"), nil, errAttempt)
			r.attempt(chatReq("q"), nil, errAttempt)
			r.finish(500)
		},
		// e.g. governance rejecting the virtual key in its own PreLLMHook
		"rejected before provider": func(r *request) {
			r.attempt(chatReq("q"), nil, &schemas.BifrostError{StatusCode: schemas.Ptr(401)})
			r.finish(401)
		},
		"success but non-2xx response": func(r *request) {
			r.attempt(chatReq("q"), chatResp("a", "ok"), nil)
			r.finish(502)
		},
		"no LLM hooks ran": func(r *request) { r.finish(404) },
	} {
		t.Run(name, func(t *testing.T) {
			p, got := recordingPlugin(t)
			run(newRequest(t, p))
			if len(*got) != 0 || p.Stats() != (Stats{SkippedFailed: 1}) {
				t.Errorf("captures=%d stats=%+v", len(*got), p.Stats())
			}
		})
	}
}

func TestStreamingIsMarkedNotCaptured(t *testing.T) {
	p, got := recordingPlugin(t)
	r := newRequest(t, p)
	r.core.SetValue(schemas.BifrostContextKeyStreamStartTime, time.Now())
	for i := 0; i < 5; i++ { // PostLLMHook runs once per chunk
		r.attempt(chatReq("q"), chatResp(fmt.Sprint("chunk", i), "ok"), nil)
	}
	r.finish(200)
	if len(*got) != 0 || p.Stats() != (Stats{SkippedStreaming: 1}) {
		t.Errorf("captures=%d stats=%+v", len(*got), p.Stats())
	}
}

func TestUnsupportedResponseTypes(t *testing.T) {
	for name, resp := range map[string]*schemas.BifrostResponse{
		"embedding":       {EmbeddingResponse: &schemas.BifrostEmbeddingResponse{}},
		"chat, no text":   {ChatResponse: &schemas.BifrostChatResponse{Choices: []schemas.BifrostResponseChoice{{}}}},
		"text completion": {TextCompletionResponse: &schemas.BifrostTextCompletionResponse{}},
	} {
		t.Run(name, func(t *testing.T) {
			p, got := recordingPlugin(t)
			r := newRequest(t, p)
			r.attempt(chatReq("q"), resp, nil)
			r.finish(200)
			if len(*got) != 0 || p.Stats() != (Stats{SkippedUnsupported: 1}) {
				t.Errorf("captures=%d stats=%+v", len(*got), p.Stats())
			}
		})
	}
}

func TestFinalizeOnce(t *testing.T) {
	p, got := recordingPlugin(t)
	r := newRequest(t, p)
	r.attempt(chatReq("q"), chatResp("a", "ok"), nil)
	r.finish(200)
	r.finish(200)
	if len(*got) != 1 || p.Stats().Captured != 1 {
		t.Errorf("captures=%d stats=%+v", len(*got), p.Stats())
	}
}

// Used as a Go SDK, or on a path without the HTTP pre-hook, there is no slot:
// the hooks must do nothing.
func TestNoSlotIsNoOp(t *testing.T) {
	p, got := recordingPlugin(t)
	ctx := newCtx()
	p.PreLLMHook(ctx, chatReq("q"))
	p.PostLLMHook(ctx, chatResp("a", "ok"), nil)
	if err := p.HTTPTransportPostHook(ctx, &schemas.HTTPRequest{}, &schemas.HTTPResponse{StatusCode: 200}); err != nil {
		t.Fatal(err)
	}
	if err := p.HTTPTransportPostHook(ctx, &schemas.HTTPRequest{}, nil); err != nil {
		t.Fatal(err)
	}
	if len(*got) != 0 || p.Stats() != (Stats{}) {
		t.Errorf("captures=%d stats=%+v", len(*got), p.Stats())
	}
}

func TestResponsesAPIAnswer(t *testing.T) {
	msg, reasoning := schemas.ResponsesMessageTypeMessage, schemas.ResponsesMessageType("reasoning")
	assistant, user := schemas.ResponsesInputMessageRoleAssistant, schemas.ResponsesInputMessageRoleUser
	resp := &schemas.BifrostResponse{ResponsesResponse: &schemas.BifrostResponsesResponse{
		Output: []schemas.ResponsesMessage{
			{Type: &reasoning, Content: &schemas.ResponsesMessageContent{ContentStr: schemas.Ptr("thinking")}},
			{Type: &msg, Role: &assistant, Content: &schemas.ResponsesMessageContent{ContentBlocks: []schemas.ResponsesMessageContentBlock{
				{Type: schemas.ResponsesOutputMessageContentTypeText, Text: schemas.Ptr("Pro is $29.")},
				{Type: schemas.ResponsesMessageContentBlockType("refusal"), Text: schemas.Ptr("ignored")},
				{Type: schemas.ResponsesOutputMessageContentTypeText, Text: schemas.Ptr("Since October.")},
			}}},
		},
		ExtraFields: schemas.BifrostResponseExtraFields{RequestType: schemas.ResponsesRequest, RoutingInfo: schemas.RoutingInfo{Provider: schemas.OpenAI, Model: "ok"}},
	}}
	req := &schemas.BifrostRequest{RequestType: schemas.ResponsesRequest, ResponsesRequest: &schemas.BifrostResponsesRequest{Input: []schemas.ResponsesMessage{
		{Role: &user, Content: &schemas.ResponsesMessageContent{ContentStr: schemas.Ptr("old question")}},
		{Role: &assistant, Content: &schemas.ResponsesMessageContent{ContentStr: schemas.Ptr("old answer")}},
		{Role: &user, Content: &schemas.ResponsesMessageContent{ContentBlocks: []schemas.ResponsesMessageContentBlock{
			{Type: schemas.ResponsesInputMessageContentBlockTypeText, Text: schemas.Ptr("What does Pro cost?")},
		}}},
	}}}

	p, got := recordingPlugin(t)
	r := newRequest(t, p)
	r.attempt(req, resp, nil)
	r.finish(200)
	if len(*got) != 1 {
		t.Fatalf("captures = %d", len(*got))
	}
	if c := (*got)[0]; c.Answer != "Pro is $29.\nSince October." || c.Question != "What does Pro cost?" || c.RequestType != schemas.ResponsesRequest {
		t.Errorf("capture = %+v", c)
	}
}

func TestChatContentBlocks(t *testing.T) {
	blocks := &schemas.ChatMessageContent{ContentBlocks: []schemas.ChatContentBlock{
		{Type: schemas.ChatContentBlockTypeText, Text: schemas.Ptr("part one")},
		{Type: schemas.ChatContentBlockTypeImage},
		{Type: schemas.ChatContentBlockTypeText, Text: schemas.Ptr("part two")},
	}}
	req := &schemas.BifrostRequest{ChatRequest: &schemas.BifrostChatRequest{Input: []schemas.ChatMessage{
		{Role: schemas.ChatMessageRoleUser, Content: &schemas.ChatMessageContent{ContentStr: schemas.Ptr("first question")}},
		{Role: schemas.ChatMessageRoleUser, Content: blocks},
		{Role: schemas.ChatMessageRoleAssistant, Content: &schemas.ChatMessageContent{ContentStr: schemas.Ptr("not a question")}},
	}}}
	if q, ok := questionFromRequest(req); !ok || q != "part one\npart two" {
		t.Errorf("question = %q, %t", q, ok)
	}
	if q, ok := questionFromRequest(&schemas.BifrostRequest{EmbeddingRequest: &schemas.BifrostEmbeddingRequest{}}); ok || q != "" {
		t.Errorf("embedding question = %q, %t", q, ok)
	}
}

func TestClip(t *testing.T) {
	if s, cut := clip("short", 10); s != "short" || cut {
		t.Errorf("clip short = %q %t", s, cut)
	}
	s, cut := clip("ab€cd", 4) // € is 3 bytes at offsets 2..4: cut must not split it
	if s != "ab" || !cut {
		t.Errorf("clip = %q %t, want \"ab\" true", s, cut)
	}

	p, got := recordingPlugin(t)
	r := newRequest(t, p)
	r.attempt(chatReq(strings.Repeat("q", maxQuestionBytes+1)), chatResp(strings.Repeat("a", maxAnswerBytes+1), "ok"), nil)
	r.finish(200)
	if c := (*got)[0]; len(c.Question) != maxQuestionBytes || len(c.Answer) != maxAnswerBytes || !c.Truncated {
		t.Errorf("question=%d answer=%d truncated=%t", len(c.Question), len(c.Answer), c.Truncated)
	}
}

// The gateway pools and reuses response buffers. A capture must not alias
// them: overwrite the bytes behind the answer after capture and check the
// capture still holds the original text.
func TestCaptureDoesNotAliasSourceBuffers(t *testing.T) {
	buf := []byte("The answer lives in a pooled buffer.")
	answer := unsafe.String(unsafe.SliceData(buf), len(buf))

	p, got := recordingPlugin(t)
	r := newRequest(t, p)
	r.attempt(chatReq("q"), chatResp(answer, "ok"), nil)
	for i := range buf {
		buf[i] = 'X'
	}
	r.finish(200)
	if (*got)[0].Answer != "The answer lives in a pooled buffer." {
		t.Errorf("capture aliases the source buffer: %q", (*got)[0].Answer)
	}
}

// Concurrent requests must never see each other's slot.
func TestConcurrentRequestsAreIsolated(t *testing.T) {
	p, got := recordingPlugin(t)
	const n = 200
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := newRequest(t, p)
			q := fmt.Sprintf("question %d", i)
			if i%3 == 0 {
				r.attempt(chatReq(q), nil, errAttempt)
			}
			r.attempt(chatReq(q), chatResp(fmt.Sprintf("answer %d", i), "ok"), nil)
			r.finish(200)
		}()
	}
	wg.Wait()
	if len(*got) != n {
		t.Fatalf("captures = %d, want %d", len(*got), n)
	}
	for _, c := range *got {
		var qi, ai int
		fmt.Sscanf(c.Question, "question %d", &qi)
		fmt.Sscanf(c.Answer, "answer %d", &ai)
		if qi != ai || c.IsFallback != (qi%3 == 0) {
			t.Errorf("mixed capture: %+v", c)
		}
	}
}

func TestReportHeader(t *testing.T) {
	p, got := recordingPlugin(t)
	r := newRequest(t, p)
	r.attempt(chatReq("q"), chatResp("a", "ok"), nil)
	h := r.finish(200)
	id := (*got)[0].ReportID
	if h[HeaderReport] != id {
		t.Errorf("%s = %q, want %q", HeaderReport, h[HeaderReport], id)
	}
	if h["Access-Control-Expose-Headers"] != "x-bifrost-request-id, x-request-id, x-halflife-report" {
		t.Errorf("expose = %q", h["Access-Control-Expose-Headers"])
	}
	if h := r.headers(200); h["Access-Control-Expose-Headers"] != "x-bifrost-request-id, x-request-id, x-halflife-report" {
		t.Errorf("header hook run twice: expose = %q", h["Access-Control-Expose-Headers"])
	}

	// No report, no header: failed, streaming, unsupported, and no slot.
	for name, run := range map[string]func(r *request){
		"failed":      func(r *request) { r.attempt(chatReq("q"), nil, errAttempt) },
		"unsupported": func(r *request) { r.attempt(chatReq("q"), &schemas.BifrostResponse{}, nil) },
		"streaming": func(r *request) {
			r.core.SetValue(schemas.BifrostContextKeyStreamStartTime, time.Now())
			r.attempt(chatReq("q"), chatResp("a", "ok"), nil)
		},
	} {
		r := newRequest(t, p)
		run(r)
		if h := r.finish(200); h[HeaderReport] != "" || len(h) != 2 {
			t.Errorf("%s: headers = %v", name, h)
		}
	}
	meta := &schemas.HTTPResponseMetadata{StatusCode: 200, Headers: map[string]string{}}
	if err := p.HTTPTransportResponseHeadersHook(newCtx(), nil, meta); err != nil || len(meta.Headers) != 0 {
		t.Errorf("no slot: %v %v", err, meta.Headers)
	}
}

// For streaming, the gateway runs the headers hook before the stream's
// post-hooks, so there is nothing to report yet.
func TestReportHeaderBeforeFinalize(t *testing.T) {
	p, _ := recordingPlugin(t)
	r := newRequest(t, p)
	r.attempt(chatReq("q"), chatResp("a", "ok"), nil)
	if h := r.headers(200); h[HeaderReport] != "" {
		t.Errorf("header set before finalize: %v", h)
	}
}

func TestRequestIDFallsBackToResponseHeader(t *testing.T) {
	p, got := recordingPlugin(t)
	r := newRequest(t, p)
	r.core.ClearValue(schemas.BifrostContextKeyRequestID)
	r.attempt(chatReq("q"), chatResp("a", "ok"), nil)
	scoped := r.scoped(r.transport)
	resp := &schemas.HTTPResponse{StatusCode: 200, Headers: map[string]string{"x-bifrost-request-id": "from-header"}}
	if err := p.HTTPTransportPostHook(scoped, &schemas.HTTPRequest{}, resp); err != nil {
		t.Fatal(err)
	}
	if (*got)[0].RequestID != "from-header" {
		t.Errorf("request id = %q", (*got)[0].RequestID)
	}
}

// The x-bf-vk header value is a credential; only the resolved key's ID and
// name may be recorded.
func TestVirtualKeySecretNeverRecorded(t *testing.T) {
	p, got := recordingPlugin(t)
	r := newRequest(t, p)
	r.attempt(chatReq("q"), chatResp("a", "ok"), nil)
	r.finish(200)
	if dump := fmt.Sprintf("%+v", *(*got)[0]); strings.Contains(dump, "sk-bf-secret-value") {
		t.Errorf("capture contains the virtual key secret: %s", dump)
	}
}

func TestReportIDsUniqueAndTimeOrdered(t *testing.T) {
	const n = 10000
	ids := make([]string, n)
	seen := make(map[string]bool, n)
	for i := range ids {
		ids[i] = newReportID()
		if seen[ids[i]] {
			t.Fatalf("duplicate id %s", ids[i])
		}
		seen[ids[i]] = true
	}
	if !slices.IsSorted(ids) {
		t.Error("ids are not in creation order")
	}
	if u, err := uuid.Parse(strings.TrimPrefix(ids[0], "hlr_")); err != nil || u.Version() != 7 {
		t.Errorf("id %s: %v, version %d", ids[0], err, u.Version())
	}
}
