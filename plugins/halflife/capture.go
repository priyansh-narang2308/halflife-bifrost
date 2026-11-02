package halflife

import (
	"strings"
	"sync"
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
	RequestType    schemas.RequestType
	Provider       schemas.ModelProvider // provider that served the answer
	Model          string                // model that served the answer
	RequestedModel string                // model the caller asked for
	IsFallback     bool                  // served by a fallback after the primary failed
	Question       string                // last user message of the request
	Answer         string                // assistant text of the first choice
	Truncated      bool                  // Question or Answer was cut to its size cap
}

// SkipReason says why a request produced no Capture.
type SkipReason int

const (
	SkipNone        SkipReason = iota
	SkipFailed                 // no attempt succeeded, or the final HTTP status is not 2xx
	SkipStreaming              // streaming responses are not audited yet
	SkipUnsupported            // request type without auditable text (embeddings, images, ...)
)

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
	if s.reason == SkipStreaming {
		return // a streaming request stays streaming across attempts
	}
	s.pending, s.reason = c, reason
}

// finalize returns the request's capture exactly once. ok is false for every
// later call, and when the request ended without an auditable answer.
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
		return nil, SkipFailed, true
	}
	c = s.pending
	c.Question = s.question
	c.Truncated = c.Truncated || s.truncated
	s.pending = nil
	return c, SkipNone, true
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
	return &Capture{
		RequestType:    extras.RequestType,
		Provider:       ri.Provider,
		Model:          strings.Clone(ri.Model),
		RequestedModel: strings.Clone(ri.RequestedModel),
		IsFallback:     ri.IsFallback,
		Answer:         answer,
		Truncated:      truncated,
	}, SkipNone
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
