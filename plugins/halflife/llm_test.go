package halflife

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	bifrost "github.com/maximhq/bifrost/core"
)

var testCheckSchema = MustCompileSchema("claim_check", []byte(`{
  "type": "object",
  "properties": {
    "verdict": {"type": "string", "enum": ["SUPPORTED", "CONTRADICTED", "UNVERIFIABLE"]},
    "quote":   {"type": "string"}
  },
  "required": ["verdict", "quote"],
  "additionalProperties": false
}`))

type claimCheck struct {
	Verdict string `json:"verdict"`
	Quote   string `json:"quote"`
}

// fakeGateway answers /v1/chat/completions from a script, one reply per call
// (the last repeats), and records each request.
type fakeGateway struct {
	t       *testing.T
	srv     *httptest.Server
	mu      sync.Mutex
	replies []gwReply
	bodies  []map[string]any
	headers []http.Header
}

type gwReply struct {
	status  int
	header  map[string]string
	content any // string or text parts; marshalled as message.content
	finish  string
	raw     string
}

func newFakeGateway(t *testing.T, replies ...gwReply) *fakeGateway {
	g := &fakeGateway{t: t, replies: replies}
	g.srv = httptest.NewServer(http.HandlerFunc(g.serve))
	t.Cleanup(g.srv.Close)
	return g
}

func (g *fakeGateway) serve(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	n := len(g.bodies)
	rep := g.replies[min(n, len(g.replies)-1)]
	var body map[string]any
	raw, _ := io.ReadAll(r.Body)
	if r.URL.Path != "/v1/chat/completions" || json.Unmarshal(raw, &body) != nil {
		g.t.Errorf("unexpected request %s %s", r.URL.Path, raw)
	}
	g.bodies = append(g.bodies, body)
	g.headers = append(g.headers, r.Header.Clone())
	g.mu.Unlock()

	w.Header().Set("X-Bifrost-Request-Id", "gw-"+string(rune('1'+n)))
	for k, v := range rep.header {
		w.Header().Set(k, v)
	}
	status := rep.status
	if status == 0 {
		status = http.StatusOK
	}
	w.WriteHeader(status)
	if rep.raw != "" || status != http.StatusOK {
		io.WriteString(w, rep.raw)
		return
	}
	json.NewEncoder(w).Encode(map[string]any{
		"model":   "nemotron:free",
		"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": rep.content}, "finish_reason": rep.finish}},
		"usage":   map[string]int{"prompt_tokens": 40, "completion_tokens": 9},
		"extra_fields": map[string]any{
			"provider": "openrouter", "resolved_model_used": "nvidia/nemotron:free",
			"routing_info": map[string]any{"is_fallback": true},
		},
	})
}

func (g *fakeGateway) calls() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.bodies)
}

func testLLM(g *fakeGateway, models ...string) (*LLM, *[]time.Duration) {
	if len(models) == 0 {
		models = []string{"openrouter/nvidia/nemotron:free", "openrouter/xingchen:free"}
	}
	l := NewLLM(g.srv.URL+"/", "sk-bf-verifier-acme", LLMOptions{Models: models, MaxTokens: 256}, nil)
	var slept []time.Duration
	l.sleep = func(_ context.Context, d time.Duration) error { slept = append(slept, d); return nil }
	return l, &slept
}

var checkPrompt = []LLMMessage{{Role: "system", Content: "Judge the claim."}, {Role: "user", Content: "Claim: Go 1.25 is the latest."}}

const validCheck = `{"verdict":"CONTRADICTED","quote":"Go 1.27 is released"}`

func TestLLMStructuredReply(t *testing.T) {
	g := newFakeGateway(t, gwReply{content: validCheck})
	l, _ := testLLM(g)
	var out claimCheck
	res, err := l.Complete(context.Background(), checkPrompt, testCheckSchema, &out)
	if err != nil || out != (claimCheck{"CONTRADICTED", "Go 1.27 is released"}) {
		t.Fatalf("Complete = %+v, %v", out, err)
	}
	want := LLMResult{Model: "nvidia/nemotron:free", Provider: "openrouter", Fallback: true, Calls: 1, PromptTokens: 40, CompletionTokens: 9}
	if got := *res; got.Model != want.Model || got.Provider != want.Provider || !got.Fallback || got.Calls != 1 || got.Malformed != 0 ||
		got.PromptTokens != 40 || got.CompletionTokens != 9 || strings.Join(got.GatewayRequestIDs, ",") != "gw-1" {
		t.Errorf("result %+v", got)
	}

	body, hdr := g.bodies[0], g.headers[0]
	if got := hdr.Get("x-bf-cache-type"); got != string(bypassCacheType) {
		t.Errorf("verifier calls must skip the semantic cache: x-bf-cache-type = %q", got)
	}
	if hdr.Get("x-bf-vk") != "sk-bf-verifier-acme" || hdr.Get("Content-Type") != "application/json" {
		t.Errorf("headers %v", hdr)
	}
	if body["model"] != "openrouter/nvidia/nemotron:free" || body["temperature"] != 0.0 || body["max_tokens"] != 256.0 {
		t.Errorf("body %v", body)
	}
	if fb, _ := json.Marshal(body["fallbacks"]); string(fb) != `["openrouter/xingchen:free"]` {
		t.Errorf("fallbacks %s", fb)
	}
	rf, _ := json.Marshal(body["response_format"])
	if !strings.Contains(string(rf), `"type":"json_schema"`) || !strings.Contains(string(rf), `"name":"claim_check","schema":{`) ||
		!strings.Contains(string(rf), `"strict":true`) || !strings.Contains(string(rf), `"additionalProperties":false`) {
		t.Errorf("response_format %s", rf)
	}
}

func TestLLMSingleModelSendsNoFallbacks(t *testing.T) {
	g := newFakeGateway(t, gwReply{content: validCheck})
	l, _ := testLLM(g, "openai/ok")
	var out claimCheck
	if _, err := l.Complete(context.Background(), checkPrompt, testCheckSchema, &out); err != nil {
		t.Fatal(err)
	}
	if _, ok := g.bodies[0]["fallbacks"]; ok {
		t.Errorf("fallbacks sent for one model: %v", g.bodies[0])
	}
	if _, ok := g.bodies[0]["reasoning"]; ok {
		t.Errorf("reasoning sent without an effort: %v", g.bodies[0])
	}
}

func TestLLMSendsReasoningEffort(t *testing.T) {
	g := newFakeGateway(t, gwReply{content: validCheck})
	l := NewLLM(g.srv.URL, "k", LLMOptions{Models: []string{"openrouter/m"}, MaxTokens: 64, ReasoningEffort: "low"}, nil)
	if _, err := l.Complete(context.Background(), checkPrompt, testCheckSchema, &claimCheck{}); err != nil {
		t.Fatal(err)
	}
	if r, _ := json.Marshal(g.bodies[0]["reasoning"]); string(r) != `{"effort":"low"}` {
		t.Errorf("reasoning %s", r)
	}
}

func TestLLMAcceptedReplyShapes(t *testing.T) {
	for name, content := range map[string]any{
		"fenced":     "```json\n" + validCheck + "\n```",
		"bare fence": "```\n" + validCheck + "\n```",
		"padded":     "\n  " + validCheck + "  \n",
		"text parts": []any{map[string]any{"type": "text", "text": `{"verdict":"CONTRADICTED",`}, map[string]any{"type": "text", "text": `"quote":"Go 1.27 is released"}`}},
	} {
		g := newFakeGateway(t, gwReply{content: content})
		l, _ := testLLM(g)
		var out claimCheck
		if res, err := l.Complete(context.Background(), checkPrompt, testCheckSchema, &out); err != nil || out.Verdict != "CONTRADICTED" || res.Malformed != 0 {
			t.Errorf("%s: %+v %v", name, out, err)
		}
	}
}

func TestLLMCorrectsOneMalformedReply(t *testing.T) {
	g := newFakeGateway(t, gwReply{content: `Sure! The verdict is {"verdict":"CONTRADICTED"}`}, gwReply{content: validCheck})
	l, _ := testLLM(g)
	var out claimCheck
	res, err := l.Complete(context.Background(), checkPrompt, testCheckSchema, &out)
	if err != nil || out.Verdict != "CONTRADICTED" || res.Calls != 2 || res.Malformed != 1 ||
		strings.Join(res.GatewayRequestIDs, ",") != "gw-1,gw-2" || res.PromptTokens != 80 {
		t.Fatalf("Complete = %+v, %+v, %v", out, res, err)
	}
	msgs, _ := json.Marshal(g.bodies[1]["messages"])
	if !strings.Contains(string(msgs), `{"content":"Sure! The verdict is {\"verdict\":\"CONTRADICTED\"}","role":"assistant"}`) ||
		!strings.Contains(string(msgs), `not one JSON object matching the required schema`) {
		t.Errorf("retry messages %s", msgs)
	}
	if first, _ := json.Marshal(g.bodies[0]["messages"]); strings.Contains(string(first), "assistant") {
		t.Errorf("first call already had the correction: %s", first)
	}
}

func TestLLMGivesUpAfterTwoMalformedReplies(t *testing.T) {
	for name, content := range map[string]string{
		"prose":          "I think it is outdated.",
		"wrong enum":     `{"verdict":"STALE","quote":"x"}`,
		"missing field":  `{"verdict":"SUPPORTED"}`,
		"extra field":    `{"verdict":"SUPPORTED","quote":"x","confidence":0.9}`,
		"two objects":    validCheck + validCheck,
		"array":          `[` + validCheck + `]`,
		"empty":          "",
		"fence and text": "```json\n" + validCheck + "\n``` hope this helps",
	} {
		g := newFakeGateway(t, gwReply{content: content})
		l, _ := testLLM(g)
		out := claimCheck{Verdict: "untouched"}
		res, err := l.Complete(context.Background(), checkPrompt, testCheckSchema, &out)
		if !errors.Is(err, ErrLLMMalformed) || res.Calls != 2 || res.Malformed != 2 || out.Verdict != "untouched" {
			t.Errorf("%s: err %v result %+v out %+v", name, err, res, out)
		}
	}
}

func TestLLMTruncatedReplyIsNotRetried(t *testing.T) {
	for name, content := range map[string]any{
		"thinking ate the limit": nil, // measured live: content null, finish_reason length
		"object cut off":         `{"verdict":"CONTRA`,
	} {
		g := newFakeGateway(t, gwReply{content: content, finish: "length"}, gwReply{content: validCheck})
		l, _ := testLLM(g)
		var out claimCheck
		res, err := l.Complete(context.Background(), checkPrompt, testCheckSchema, &out)
		if !errors.Is(err, ErrLLMTruncated) || errors.Is(err, ErrLLMMalformed) || !res.Truncated || res.Calls != 1 || out.Verdict != "" {
			t.Errorf("%s: err %v result %+v", name, err, res)
		}
		if !strings.Contains(err.Error(), "(256 tokens)") {
			t.Errorf("%s: message %q", name, err)
		}
	}
	// A complete object that happens to end at the limit is still valid.
	g := newFakeGateway(t, gwReply{content: validCheck, finish: "length"})
	l, _ := testLLM(g)
	var out claimCheck
	if res, err := l.Complete(context.Background(), checkPrompt, testCheckSchema, &out); err != nil || res.Truncated || out.Verdict != "CONTRADICTED" {
		t.Errorf("valid at the limit: err %v result %+v", err, res)
	}
}

func TestLLMRefusalIsMalformed(t *testing.T) {
	g := newFakeGateway(t, gwReply{raw: `{"model":"m","choices":[{"message":{"role":"assistant","content":null,"refusal":"I can't help with that."}}]}`})
	l, _ := testLLM(g)
	var out claimCheck
	if _, err := l.Complete(context.Background(), checkPrompt, testCheckSchema, &out); !errors.Is(err, ErrLLMMalformed) {
		t.Errorf("refusal: %v", err)
	}
}

func TestLLMBudgetRefusalIsNotRetried(t *testing.T) {
	g := newFakeGateway(t, gwReply{status: http.StatusPaymentRequired, raw: `{"error":{"message":"Budget exceeded"}}`})
	l, slept := testLLM(g)
	var out claimCheck
	res, err := l.Complete(context.Background(), checkPrompt, testCheckSchema, &out)
	var le *LLMError
	if !errors.As(err, &le) || !le.BudgetSpent() || le.Retryable() || le.Message != "Budget exceeded" || res.Calls != 1 || len(*slept) != 0 {
		t.Fatalf("402: err %v result %+v slept %v", err, res, *slept)
	}
	if !strings.Contains(err.Error(), "402 Budget exceeded") {
		t.Errorf("message %q", err)
	}
}

func TestLLMRetriesOnceWhenRetryable(t *testing.T) {
	for name, tc := range map[string]struct {
		first gwReply
		wait  time.Duration
	}{
		"rate limited":          {gwReply{status: 429, header: map[string]string{"Retry-After": "3"}, raw: `{"error":{"message":"rate limited"}}`}, 3 * time.Second},
		"server error":          {gwReply{status: 503, raw: `{"error":{"message":"all fallbacks failed"}}`}, defaultRetryWait},
		"rate limited, no wait": {gwReply{status: 429}, defaultRetryWait},
	} {
		g := newFakeGateway(t, tc.first, gwReply{content: validCheck})
		l, slept := testLLM(g)
		var out claimCheck
		res, err := l.Complete(context.Background(), checkPrompt, testCheckSchema, &out)
		if err != nil || out.Verdict != "CONTRADICTED" || res.Calls != 2 || len(*slept) != 1 || (*slept)[0] != tc.wait {
			t.Errorf("%s: err %v result %+v slept %v", name, err, res, *slept)
		}
	}

	// A Retry-After longer than maxRetryAfter is not waited out: the verifier
	// opens the LLM circuit instead (resilience.go).
	g := newFakeGateway(t, gwReply{status: 429, header: map[string]string{"Retry-After": "120"}}, gwReply{content: validCheck})
	l, slept := testLLM(g)
	var le429 *LLMError
	if res, err := l.Complete(context.Background(), checkPrompt, testCheckSchema, &claimCheck{}); !errors.As(err, &le429) || le429.RetryAfter != 2*time.Minute || res.Calls != 1 || len(*slept) != 0 {
		t.Errorf("long retry-after: err %v result %+v slept %v", err, res, *slept)
	}

	g = newFakeGateway(t, gwReply{status: 500, raw: `{"error":{"message":"boom"}}`})
	l, _ = testLLM(g)
	var out claimCheck
	var le *LLMError
	if res, err := l.Complete(context.Background(), checkPrompt, testCheckSchema, &out); !errors.As(err, &le) || le.Status != 500 || res.Calls != 2 {
		t.Errorf("two 500s: err %v result %+v", err, res)
	}
}

func TestLLMClientErrorsAreNotRetried(t *testing.T) {
	for _, status := range []int{400, 401, 403, 404} {
		g := newFakeGateway(t, gwReply{status: status, raw: `{"error":{"message":"no"}}`})
		l, _ := testLLM(g)
		var out claimCheck
		var le *LLMError
		if res, err := l.Complete(context.Background(), checkPrompt, testCheckSchema, &out); !errors.As(err, &le) || le.Status != status || res.Calls != 1 {
			t.Errorf("%d: err %v result %+v", status, err, res)
		}
	}
}

func TestLLMCancelledDuringRetryWait(t *testing.T) {
	g := newFakeGateway(t, gwReply{status: 429, header: map[string]string{"Retry-After": "1"}}, gwReply{content: validCheck})
	l, _ := testLLM(g)
	l.sleep = sleepCtx
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var out claimCheck
	if _, err := l.Complete(ctx, checkPrompt, testCheckSchema, &out); err == nil || g.calls() > 1 {
		t.Errorf("cancelled: err %v, %d calls", err, g.calls())
	}
}

func TestLLMBadReplies(t *testing.T) {
	for name, rep := range map[string]gwReply{
		"not JSON":   {raw: "<html>gateway</html>"},
		"no choices": {raw: `{"model":"m","choices":[]}`},
		"oversized":  {raw: `{"model":"` + strings.Repeat("x", maxLLMResponseBytes) + `"}`},
	} {
		g := newFakeGateway(t, rep)
		l, _ := testLLM(g)
		var out claimCheck
		if res, err := l.Complete(context.Background(), checkPrompt, testCheckSchema, &out); err == nil || errors.Is(err, ErrLLMMalformed) || res.Calls != 1 {
			t.Errorf("%s: err %v result %+v", name, err, res)
		}
	}
	l := NewLLM("http://127.0.0.1:1", "k", LLMOptions{MaxTokens: 64}, nil)
	if _, err := l.Complete(context.Background(), checkPrompt, testCheckSchema, &claimCheck{}); err == nil {
		t.Error("no model: want an error")
	}
	l = NewLLM("http://127.0.0.1:1", "k", LLMOptions{Models: []string{"openai/ok"}, MaxTokens: 64}, &http.Client{Timeout: time.Second})
	if res, err := l.Complete(context.Background(), checkPrompt, testCheckSchema, &claimCheck{}); err == nil || res.Calls != 1 {
		t.Errorf("unreachable gateway: err %v result %+v", err, res)
	}
}

func TestCompileSchemaErrors(t *testing.T) {
	for name, src := range map[string]string{
		"not JSON":   `{"type":`,
		"bad schema": `{"type": 7}`,
	} {
		if _, err := CompileSchema("s", []byte(src)); err == nil || !strings.Contains(err.Error(), "schema s") {
			t.Errorf("%s: err %v", name, err)
		}
	}
	defer func() {
		if recover() == nil {
			t.Error("MustCompileSchema did not panic on a bad schema")
		}
	}()
	MustCompileSchema("s", []byte(`{"type": 7}`))
}

func TestUnfence(t *testing.T) {
	for in, want := range map[string]string{
		"```json\n{}\n```": "{}",
		"```{}```":         "{}",
		"```\n[1]\n```":    "[1]",
		"  {}  ":           "{}",
		"```":              "```",
		"```json {} ```":   "json {}", // a tag on the same line is not stripped; the schema check rejects it
	} {
		if got := unfence(in); got != want {
			t.Errorf("unfence(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestVerifierLLMFor(t *testing.T) {
	cfg := Config{Verifiers: []VerifierKey{acmeKey}}.withDefaults()
	v, err := newVerifier(cfg, newReportStore(4), bifrost.NewNoOpLogger())
	if err != nil {
		t.Fatal(err)
	}
	if _, _, ok := v.llmFor("vk-acme"); ok {
		t.Error("no verifier models: want no LLM")
	}

	g := newFakeGateway(t, gwReply{content: validCheck})
	cfg = Config{GatewayURL: g.srv.URL, Verifiers: []VerifierKey{acmeKey}, VerifierModels: []string{"openai/ok"}, VerifierReasoningEffort: "minimal"}.withDefaults()
	if v, err = newVerifier(cfg, newReportStore(4), bifrost.NewNoOpLogger()); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := v.llmFor("vk-initech"); ok {
		t.Error("tenant without a verifier key got an LLM")
	}
	l, key, ok := v.llmFor("vk-acme")
	if !ok || key.id != "vk-verifier-acme" {
		t.Fatalf("llmFor(vk-acme) = %v %+v", ok, key)
	}
	var out claimCheck
	if _, err := l.Complete(context.Background(), checkPrompt, testCheckSchema, &out); err != nil {
		t.Fatal(err)
	}
	if g.headers[0].Get("x-bf-vk") != "sk-bf-verifier-acme" || g.bodies[0]["max_tokens"] != float64(DefaultVerifierMaxTokens) ||
		g.bodies[0]["reasoning"].(map[string]any)["effort"] != "minimal" {
		t.Errorf("request %v %v", g.headers[0], g.bodies[0])
	}
}
