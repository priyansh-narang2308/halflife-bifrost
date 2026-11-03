package halflife

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// The verifier's own LLM calls go through this gateway with the tenant's
// verifier key, like any caller's: governance prices and caps them, Bifrost's
// request fallbacks move to the next model when one fails, and the recursion
// guard keeps them from being audited. Every reply must be one JSON object
// matching a schema. A malformed reply gets one corrective retry, then the
// call fails with ErrLLMMalformed and the claim is reported unverifiable
// (audit §3.4); a malformed reply is never turned into a verdict.

const (
	maxLLMResponseBytes = 1 << 20
	maxEchoedReplyBytes = 2000 // a malformed reply sent back with the correction
	maxSchemaErrorBytes = 300
	llmFormatAttempts   = 2 // the first reply, plus one corrective retry
)

var (
	// ErrLLMMalformed means the model replied twice without a JSON object
	// that matches the schema.
	ErrLLMMalformed = errors.New("verifier llm: reply did not match the schema")
	// ErrLLMTruncated means the reply hit the token limit before it was a
	// valid object. Reasoning models can spend the whole limit thinking
	// (measured live: 512 tokens, empty content), and a correction at the
	// same limit would only spend it again, so it is not retried.
	ErrLLMTruncated = errors.New("verifier llm: reply cut off at the token limit")
)

// LLMError is a non-2xx reply from the gateway's chat endpoint.
type LLMError struct {
	Status     int
	Message    string
	RetryAfter time.Duration // from Retry-After, else 0
}

func (e *LLMError) Error() string {
	return fmt.Sprintf("verifier llm: %d %s", e.Status, e.Message)
}

// Retryable reports whether the same call may succeed later: a rate limit
// or a server error after Bifrost's own fallbacks were exhausted.
func (e *LLMError) Retryable() bool {
	return e.Status == http.StatusTooManyRequests || e.Status >= 500
}

// BudgetSpent reports a governance budget refusal. The verifier key's budget
// covers its searches and its LLM calls (M1 report, D), so a 402 here means
// verification is over for this window; it is never retried (D36).
func (e *LLMError) BudgetSpent() bool { return e.Status == http.StatusPaymentRequired }

// JSONSchema is a compiled schema together with the source sent to the model
// as response_format.
type JSONSchema struct {
	Name     string
	source   any
	compiled *jsonschema.Schema
}

// CompileSchema compiles a JSON Schema document. name identifies it to the
// model (letters, digits, '_' and '-').
func CompileSchema(name string, src []byte) (*JSONSchema, error) {
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(src))
	if err != nil {
		return nil, fmt.Errorf("verifier llm: schema %s: %w", name, err)
	}
	loc := "halflife://schemas/" + name + ".json"
	c := jsonschema.NewCompiler()
	if err := c.AddResource(loc, doc); err != nil {
		return nil, fmt.Errorf("verifier llm: schema %s: %w", name, err)
	}
	compiled, err := c.Compile(loc)
	if err != nil {
		return nil, fmt.Errorf("verifier llm: schema %s: %w", name, err)
	}
	var source any
	if err := json.Unmarshal(src, &source); err != nil {
		return nil, fmt.Errorf("verifier llm: schema %s: %w", name, err)
	}
	return &JSONSchema{Name: name, source: source, compiled: compiled}, nil
}

// MustCompileSchema is CompileSchema for schemas built into the plugin.
func MustCompileSchema(name string, src []byte) *JSONSchema {
	s, err := CompileSchema(name, src)
	if err != nil {
		panic(err)
	}
	return s
}

// check parses text as one JSON value and validates it.
func (s *JSONSchema) check(text string) error {
	v, err := jsonschema.UnmarshalJSON(strings.NewReader(text))
	if err != nil {
		return fmt.Errorf("not JSON: %w", err)
	}
	return s.compiled.Validate(v)
}

// LLMMessage is one chat message.
type LLMMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// LLMResult describes how a structured call was answered.
type LLMResult struct {
	Model             string   `json:"model"`    // the model that answered
	Provider          string   `json:"provider"` // its provider
	Fallback          bool     `json:"fallback"` // answered by one of Bifrost's fallbacks
	Calls             int      `json:"calls"`    // HTTP calls made, including retries
	Malformed         int      `json:"malformed"`
	Truncated         bool     `json:"truncated"`
	PromptTokens      int      `json:"prompt_tokens"`
	CompletionTokens  int      `json:"completion_tokens"`
	GatewayRequestIDs []string `json:"gateway_request_ids"`
}

// LLMOptions configures an LLM client.
type LLMOptions struct {
	// Models are "provider/model": the first is asked, the rest are sent as
	// Bifrost request fallbacks. At least one is required.
	Models []string
	// MaxTokens caps each reply, reasoning included.
	MaxTokens int
	// ReasoningEffort, when set, is sent as reasoning.effort ("none" to
	// "high"). Reasoning models can otherwise spend the whole token limit
	// thinking; models without reasoning may reject the parameter.
	ReasoningEffort string
}

// LLM calls the gateway's chat endpoint for structured JSON replies.
type LLM struct {
	baseURL    string
	virtualKey string // the verifier key's secret value; sent only to the gateway
	opts       LLMOptions
	client     *http.Client
	sleep      func(context.Context, time.Duration) error
}

// NewLLM returns a client for baseURL (this gateway's origin).
func NewLLM(baseURL, virtualKey string, opts LLMOptions, client *http.Client) *LLM {
	if client == nil {
		client = &http.Client{Timeout: 90 * time.Second}
	}
	opts.Models = append([]string(nil), opts.Models...)
	return &LLM{baseURL: strings.TrimRight(baseURL, "/"), virtualKey: virtualKey, opts: opts, client: client, sleep: sleepCtx}
}

// Complete sends messages and decodes the reply into out, which must match
// schema. The result is returned even on error, so callers can record what
// was spent.
func (l *LLM) Complete(ctx context.Context, messages []LLMMessage, schema *JSONSchema, out any) (*LLMResult, error) {
	res := &LLMResult{GatewayRequestIDs: []string{}}
	if len(l.opts.Models) == 0 {
		return res, errors.New("verifier llm: no model configured")
	}
	msgs := append([]LLMMessage(nil), messages...)
	var problem error
	for range llmFormatAttempts {
		reply, truncated, err := l.callWithRetry(ctx, msgs, schema, res)
		if err != nil {
			return res, err
		}
		text := unfence(reply)
		if problem = schema.check(text); problem == nil {
			if err := json.Unmarshal([]byte(text), out); err != nil {
				return res, fmt.Errorf("verifier llm: decode: %w", err)
			}
			return res, nil
		}
		res.Malformed++
		if truncated {
			res.Truncated = true
			return res, fmt.Errorf("%w (%d tokens)", ErrLLMTruncated, l.opts.MaxTokens)
		}
		echoed, _ := clip(reply, maxEchoedReplyBytes)
		msgs = append(msgs,
			LLMMessage{Role: "assistant", Content: echoed},
			LLMMessage{Role: "user", Content: "That reply was not one JSON object matching the required schema (" +
				clipString(problem.Error(), maxSchemaErrorBytes) + "). Reply again with only the JSON object: no prose, no code fences."})
	}
	return res, fmt.Errorf("%w: %s", ErrLLMMalformed, clipString(problem.Error(), maxSchemaErrorBytes))
}

// callWithRetry makes one call, retrying once when the gateway says a retry
// may succeed. Bifrost has already tried every fallback model by then.
func (l *LLM) callWithRetry(ctx context.Context, msgs []LLMMessage, schema *JSONSchema, res *LLMResult) (string, bool, error) {
	reply, truncated, err := l.call(ctx, msgs, schema, res)
	var le *LLMError
	if err == nil || !errors.As(err, &le) || !le.Retryable() {
		return reply, truncated, err
	}
	wait := min(le.RetryAfter, maxRetryAfter)
	if wait <= 0 {
		wait = defaultRetryWait
	}
	if serr := l.sleep(ctx, wait); serr != nil {
		return "", false, err
	}
	return l.call(ctx, msgs, schema, res)
}

type chatReply struct {
	Model   string `json:"model"`
	Choices []struct {
		Message struct {
			Content json.RawMessage `json:"content"`
			Refusal string          `json:"refusal"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
	ExtraFields struct {
		Provider          string `json:"provider"`
		ResolvedModelUsed string `json:"resolved_model_used"`
		RoutingInfo       struct {
			IsFallback bool `json:"is_fallback"`
		} `json:"routing_info"`
	} `json:"extra_fields"`
}

// call makes one HTTP call and returns the reply text and whether it was cut
// off at the token limit.
func (l *LLM) call(ctx context.Context, msgs []LLMMessage, schema *JSONSchema, res *LLMResult) (string, bool, error) {
	body := map[string]any{
		"model":       l.opts.Models[0],
		"messages":    msgs,
		"temperature": 0,
		"max_tokens":  l.opts.MaxTokens,
		"response_format": map[string]any{
			"type":        "json_schema",
			"json_schema": map[string]any{"name": schema.Name, "strict": true, "schema": schema.source},
		},
	}
	if len(l.opts.Models) > 1 {
		body["fallbacks"] = l.opts.Models[1:]
	}
	if l.opts.ReasoningEffort != "" {
		body["reasoning"] = map[string]string{"effort": l.opts.ReasoningEffort}
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return "", false, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, l.baseURL+"/v1/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return "", false, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-bf-vk", l.virtualKey)
	res.Calls++
	resp, err := l.client.Do(req)
	if err != nil {
		return "", false, fmt.Errorf("verifier llm: %w", err)
	}
	defer resp.Body.Close()
	if id := resp.Header.Get("X-Bifrost-Request-Id"); id != "" {
		res.GatewayRequestIDs = append(res.GatewayRequestIDs, id)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxLLMResponseBytes+1))
	if err != nil {
		return "", false, fmt.Errorf("verifier llm: read: %w", err)
	}
	if len(raw) > maxLLMResponseBytes {
		return "", false, fmt.Errorf("verifier llm: reply over %d bytes", maxLLMResponseBytes)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return "", false, &LLMError{Status: resp.StatusCode, Message: errorMessage(raw), RetryAfter: retryAfter(resp.Header.Get("Retry-After"))}
	}
	var r chatReply
	if err := json.Unmarshal(raw, &r); err != nil {
		return "", false, fmt.Errorf("verifier llm: decode: %w", err)
	}
	if len(r.Choices) == 0 {
		return "", false, errors.New("verifier llm: reply has no choices")
	}
	res.Model = r.Model
	if r.ExtraFields.ResolvedModelUsed != "" {
		res.Model = r.ExtraFields.ResolvedModelUsed
	}
	res.Provider, res.Fallback = r.ExtraFields.Provider, r.ExtraFields.RoutingInfo.IsFallback
	res.PromptTokens += r.Usage.PromptTokens
	res.CompletionTokens += r.Usage.CompletionTokens
	choice := r.Choices[0]
	truncated := choice.FinishReason == "length"
	if text := contentText(choice.Message.Content); text != "" {
		return text, truncated, nil
	}
	return choice.Message.Refusal, truncated, nil // a refusal is malformed: it fails the schema check
}

// contentText reads message content given as a string or as text parts.
func contentText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) != nil {
		return ""
	}
	var b strings.Builder
	for _, p := range parts {
		if p.Type == "text" {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

// unfence strips surrounding whitespace and one Markdown code fence, which
// some models add around JSON despite response_format. Nothing else is
// repaired: prose around the object fails the schema check.
func unfence(s string) string {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "```") || !strings.HasSuffix(s, "```") || len(s) < 6 {
		return s
	}
	s = strings.TrimSuffix(strings.TrimPrefix(s, "```"), "```")
	if nl := strings.IndexByte(s, '\n'); nl >= 0 && !strings.ContainsAny(s[:nl], "{[") {
		s = s[nl+1:] // the language tag line, e.g. "json"
	}
	return strings.TrimSpace(s)
}
