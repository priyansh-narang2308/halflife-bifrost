package halflife

import (
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/maximhq/bifrost/core/schemas"
)

// Size caps for copied text. Longer text is cut on a rune boundary and the
// capture is marked Truncated.
const (
	maxQuestionBytes = 8 << 10
	maxAnswerBytes   = 64 << 10
)

// Capture is one completed answer, copied out of the gateway's pooled objects
// so it stays valid after the request has been served.
type Capture struct {
	ReportID   string    // Halflife's ID for the audit report, sent to the client as X-Halflife-Report
	RequestID  string    // the gateway's request ID (X-Bifrost-Request-Id), linking the report to Bifrost's logs
	CapturedAt time.Time // when the request finished

	// The governance virtual key that made the request: the tenant. Its ID and
	// name only, never the key's secret value (the x-bf-vk header).
	VirtualKeyID   string
	VirtualKeyName string

	RequestType    schemas.RequestType
	Provider       schemas.ModelProvider // provider that served the answer
	Model          string                // model that served the answer
	RequestedModel string                // model the caller asked for
	IsFallback     bool                  // served by a fallback after the primary failed
	Question       string                // last user message of the request
	Answer         string                // assistant text of the first choice
	Truncated      bool                  // Question or Answer was cut to its size cap

	// The semantic cache entry behind this answer, when the semantic_cache
	// plugin looked the request up: on a miss, the entry the answer is
	// written to; on a hit, the entry it was served from.
	CacheID      string
	CacheHit     bool
	CacheHitType string // "direct" or "semantic" on a hit
}

// SkipReason says why a request produced no Capture.
type SkipReason int

const (
	SkipNone        SkipReason = iota
	SkipFailed                 // no attempt succeeded, or the final HTTP status is not 2xx
	SkipStreaming              // streaming responses are not audited yet
	SkipUnsupported            // request type without auditable text (embeddings, images, ...)
	SkipVerifier               // made with the verifier's own virtual key; auditing it would recurse
)

func (r SkipReason) String() string {
	switch r {
	case SkipNone:
		return "none"
	case SkipFailed:
		return "failed"
	case SkipStreaming:
		return "streaming"
	case SkipUnsupported:
		return "unsupported"
	case SkipVerifier:
		return "verifier"
	}
	return "unknown"
}

// sticky reports whether a reason holds for every attempt of a request, so a
// later attempt can't replace it. Both depend on the request, not the attempt.
func (r SkipReason) sticky() bool { return r == SkipStreaming || r == SkipVerifier }

// slotKey is the context key for a request's slot.
type slotKey struct{}

// slot carries one request's capture from the core LLM hooks to the HTTP
// transport post-hook.
//
// The gateway runs those hooks on different BifrostContexts: values set by the
// HTTP pre-hook are copied into the core context, but nothing set in the core
// hooks comes back. So the HTTP pre-hook stores a *slot, the core hooks write
// into it through the copied pointer, and the HTTP post-hook reads it.
//
// All attempts of a request (primary and fallbacks) share the slot. Each
// PostLLMHook call replaces the pending result, so the last attempt wins.
type slot struct {
	mu        sync.Mutex
	question  string
	truncated bool
	pending   *Capture
	reason    SkipReason
	finalized bool
	reportID  string // the answer's report; read by the response-headers hook
	cached    bool   // reportID belongs to the answer that filled the cache entry served here
	dropped   bool   // captured, but the audit queue was full
}

func slotFrom(ctx *schemas.BifrostContext) *slot {
	if ctx == nil {
		return nil
	}
	s, _ := ctx.Value(slotKey{}).(*slot)
	return s
}

func (s *slot) setQuestion(q string, truncated bool) {
	s.mu.Lock()
	s.question, s.truncated = q, truncated
	s.mu.Unlock()
}

// record stores the outcome of one attempt: a capture, or why there is none.
func (s *slot) record(c *Capture, reason SkipReason) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.reason.sticky() {
		return
	}
	s.pending, s.reason = c, reason
}

// finalize returns the request's outcome exactly once; ok is false for every
// later call. The caller then attaches a report with setReport, or drop.
func (s *slot) finalize(statusCode int) (c *Capture, reason SkipReason, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.finalized {
		return nil, SkipNone, false
	}
	s.finalized = true
	switch {
	case s.reason != SkipNone:
		return nil, s.reason, true
	case s.pending == nil || statusCode < 200 || statusCode > 299:
		s.reason = SkipFailed
		return nil, SkipFailed, true
	}
	c = s.pending
	c.Question = s.question
	c.Truncated = c.Truncated || s.truncated
	s.pending = nil
	return c, SkipNone, true
}

// setReport records the report the response-headers hook announces. cached
// says it is the report of an earlier answer, served again from the cache.
func (s *slot) setReport(id string, cached bool) {
	s.mu.Lock()
	s.reportID, s.cached = id, cached
	s.mu.Unlock()
}

// drop withdraws the report ID when the capture could not be queued.
func (s *slot) drop() {
	s.mu.Lock()
	s.reportID, s.dropped = "", true
	s.mu.Unlock()
}

// report returns the report ID, or "" when the request was not captured.
func (s *slot) report() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reportID
}

// status returns the X-Halflife-Status value: "queued" with a new report,
// "cached" with the report of the answer that filled the cache entry served,
// or "not-audited; reason=<reason>". It is "" while the outcome is still open,
// which is the case for a stream whose post-hooks have not run yet.
func (s *slot) status() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case s.reportID != "" && s.cached:
		return statusCached
	case s.reportID != "":
		return statusQueued
	case s.dropped:
		return statusNotAudited + "queue-full"
	case s.finalized || s.reason.sticky():
		return statusNotAudited + s.reason.String()
	}
	return ""
}

// Values of the X-Halflife-Status header.
const (
	statusQueued     = "queued"
	statusCached     = "cached"
	statusNotAudited = "not-audited; reason="
)

// identify records who made the request and which gateway request it is.
// Governance's PreLLMHook (which runs after Halflife's) has resolved the
// virtual key by the time PostLLMHook runs.
func identify(c *Capture, ctx *schemas.BifrostContext) {
	c.RequestID = ctxString(ctx, schemas.BifrostContextKeyRequestID)
	c.VirtualKeyID = ctxString(ctx, schemas.BifrostContextKeyGovernanceVirtualKeyID)
	c.VirtualKeyName = ctxString(ctx, schemas.BifrostContextKeyGovernanceVirtualKeyName)
}

func ctxString(ctx *schemas.BifrostContext, key schemas.BifrostContextKey) string {
	v, _ := ctx.Value(key).(string)
	return strings.Clone(v)
}

// captureFromResponse copies the auditable parts of resp. It returns
// SkipUnsupported for response types that carry no assistant text.
func captureFromResponse(resp *schemas.BifrostResponse) (*Capture, SkipReason) {
	var (
		text   string
		found  bool
		extras *schemas.BifrostResponseExtraFields
	)
	switch {
	case resp.ChatResponse != nil:
		extras = &resp.ChatResponse.ExtraFields
		text, found = chatAnswer(resp.ChatResponse)
	case resp.ResponsesResponse != nil:
		extras = &resp.ResponsesResponse.ExtraFields
		text, found = responsesAnswer(resp.ResponsesResponse)
	default:
		return nil, SkipUnsupported
	}
	if !found {
		return nil, SkipUnsupported
	}
	answer, truncated := clip(text, maxAnswerBytes)
	ri := extras.RoutingInfo
	c := &Capture{
		RequestType:    extras.RequestType,
		Provider:       ri.Provider,
		Model:          strings.Clone(ri.Model),
		RequestedModel: strings.Clone(ri.RequestedModel),
		IsFallback:     ri.IsFallback,
		Answer:         answer,
		Truncated:      truncated,
	}
	// The semantic cache stamps this in its own hooks, which run before
	// Halflife's PostLLMHook.
	if cd := extras.CacheDebug; cd != nil && cd.CacheID != nil {
		c.CacheID = strings.Clone(*cd.CacheID)
		c.CacheHit = cd.CacheHit
		if cd.HitType != nil {
			c.CacheHitType = strings.Clone(*cd.HitType)
		}
	}
	return c, SkipNone
}

func chatAnswer(r *schemas.BifrostChatResponse) (string, bool) {
	for _, ch := range r.Choices {
		if ch.ChatNonStreamResponseChoice == nil || ch.Message == nil {
			continue
		}
		if text, ok := chatText(ch.Message.Content); ok {
			return text, true
		}
	}
	return "", false
}

func responsesAnswer(r *schemas.BifrostResponsesResponse) (string, bool) {
	var b strings.Builder
	found := false
	for _, m := range r.Output {
		if m.Type != nil && *m.Type != schemas.ResponsesMessageTypeMessage {
			continue // reasoning, tool calls, ...
		}
		if m.Role != nil && *m.Role != schemas.ResponsesInputMessageRoleAssistant {
			continue
		}
		if m.Content == nil {
			continue
		}
		if m.Content.ContentStr != nil {
			appendPart(&b, *m.Content.ContentStr)
			found = true
		}
		for _, blk := range m.Content.ContentBlocks {
			if blk.Type == schemas.ResponsesOutputMessageContentTypeText && blk.Text != nil {
				appendPart(&b, *blk.Text)
				found = true
			}
		}
	}
	return b.String(), found
}

// questionFromRequest returns the text of the last user message.
func questionFromRequest(req *schemas.BifrostRequest) (string, bool) {
	switch {
	case req == nil:
	case req.ChatRequest != nil:
		in := req.ChatRequest.Input
		for i := len(in) - 1; i >= 0; i-- {
			if in[i].Role == schemas.ChatMessageRoleUser {
				return chatText(in[i].Content)
			}
		}
	case req.ResponsesRequest != nil:
		in := req.ResponsesRequest.Input
		for i := len(in) - 1; i >= 0; i-- {
			if in[i].Role != nil && *in[i].Role == schemas.ResponsesInputMessageRoleUser && in[i].Content != nil {
				return responsesInputText(in[i].Content)
			}
		}
	}
	return "", false
}

func chatText(c *schemas.ChatMessageContent) (string, bool) {
	if c == nil {
		return "", false
	}
	if c.ContentStr != nil {
		return *c.ContentStr, true
	}
	var b strings.Builder
	found := false
	for _, blk := range c.ContentBlocks {
		if blk.Type == schemas.ChatContentBlockTypeText && blk.Text != nil {
			appendPart(&b, *blk.Text)
			found = true
		}
	}
	return b.String(), found
}

func responsesInputText(c *schemas.ResponsesMessageContent) (string, bool) {
	if c.ContentStr != nil {
		return *c.ContentStr, true
	}
	var b strings.Builder
	found := false
	for _, blk := range c.ContentBlocks {
		if blk.Type == schemas.ResponsesInputMessageContentBlockTypeText && blk.Text != nil {
			appendPart(&b, *blk.Text)
			found = true
		}
	}
	return b.String(), found
}

func appendPart(b *strings.Builder, s string) {
	if b.Len() > 0 {
		b.WriteByte('\n')
	}
	b.WriteString(s)
}

// clip returns an independent copy of s, cut to at most max bytes on a rune
// boundary. The copy matters: the source may live in a pooled buffer that the
// gateway reuses after the request.
func clip(s string, max int) (string, bool) {
	if len(s) <= max {
		return strings.Clone(s), false
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return strings.Clone(s[:cut]), true
}
