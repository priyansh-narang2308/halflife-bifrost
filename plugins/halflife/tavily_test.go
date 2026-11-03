package halflife

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// fakeRelay stands in for the gateway's relay route and records what the
// client sent.
type fakeRelay struct {
	srv    *httptest.Server
	path   string
	header http.Header
	body   map[string]any
	reply  func(w http.ResponseWriter)
}

func newFakeRelay(t *testing.T) *fakeRelay {
	f := &fakeRelay{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.path, f.header = r.URL.Path, r.Header.Clone()
		raw, _ := io.ReadAll(r.Body)
		f.body = nil
		_ = json.Unmarshal(raw, &f.body)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Bifrost-Request-Id", "gw-1")
		if f.reply != nil {
			f.reply(w)
			return
		}
		w.Write([]byte(`{"results":[{"title":"T","url":"https://e.com","content":"C","score":0.9,"published_date":"2026-10-01"}],"response_time":0.5,"request_id":"tv-1","usage":{"credits":1}}`))
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func relayReply(status int, body string, headers ...string) func(http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		for i := 0; i+1 < len(headers); i += 2 {
			w.Header().Set(headers[i], headers[i+1])
		}
		w.WriteHeader(status)
		w.Write([]byte(body))
	}
}

func TestTavilySearchRequestContract(t *testing.T) {
	f := newFakeRelay(t)
	tv := NewTavily(f.srv.URL+"/", "tavily", "sk-bf-verifier", nil)
	got, err := tv.Search(context.Background(), SearchRequest{Query: "acme pro price", MaxResults: 3}, Attribution{Tenant: "vk-a", Report: "hlr_1"})
	if err != nil {
		t.Fatal(err)
	}
	if f.path != "/relay/tavily/search" {
		t.Errorf("path %q", f.path)
	}
	for k, want := range map[string]string{"x-bf-vk": "sk-bf-verifier", "X-Project-ID": "vk-a", "X-Session-Id": "hlr_1", "Content-Type": "application/json"} {
		if f.header.Get(k) != want {
			t.Errorf("header %s = %q, want %q", k, f.header.Get(k), want)
		}
	}
	if f.header.Get("Authorization") != "" {
		t.Error("the client must not send a Tavily credential; the relay injects it")
	}
	if f.body["include_usage"] != true || f.body["search_depth"] != "basic" || f.body["max_results"] != float64(3) {
		t.Errorf("body %v", f.body)
	}
	if _, ok := f.body["auto_parameters"]; ok {
		t.Error("auto_parameters sent")
	}
	if len(got.Results) != 1 || got.Results[0].PublishedDate != "2026-10-01" || got.Usage.Credits != 1 || got.RequestID != "tv-1" || got.GatewayRequestID != "gw-1" {
		t.Errorf("decoded %+v", got)
	}

	// No attribution: the headers are omitted, not sent empty.
	if _, err := tv.Search(context.Background(), SearchRequest{Query: "q", SearchDepth: "advanced"}, Attribution{}); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.header["X-Project-Id"]; ok || f.body["search_depth"] != "advanced" {
		t.Errorf("headers %v body %v", f.header, f.body)
	}
	if _, err := tv.Search(context.Background(), SearchRequest{Query: "  "}, Attribution{}); err == nil {
		t.Error("empty query accepted")
	}
}

func TestTavilyExtractRequestContract(t *testing.T) {
	f := newFakeRelay(t)
	f.reply = relayReply(200, `{"results":[{"url":"https://e.com","raw_content":"page"}],"failed_results":[{"url":"https://bad","error":"Failed to fetch url"}],"usage":{"credits":0}}`)
	tv := NewTavily(f.srv.URL, "tavily", "k", nil)
	got, err := tv.Extract(context.Background(), ExtractRequest{URLs: []string{"https://e.com", "https://bad"}}, Attribution{})
	if err != nil {
		t.Fatal(err)
	}
	if f.path != "/relay/tavily/extract" || f.body["include_usage"] != true || f.body["extract_depth"] != "basic" {
		t.Errorf("path %q body %v", f.path, f.body)
	}
	if len(got.Results) != 1 || len(got.FailedResults) != 1 || got.Usage.Credits != 0 {
		t.Errorf("decoded %+v", got)
	}
	for _, n := range []int{0, 21} {
		if _, err := tv.Extract(context.Background(), ExtractRequest{URLs: make([]string, n)}, Attribution{}); err == nil {
			t.Errorf("%d URLs accepted", n)
		}
	}
}

func TestTavilyErrors(t *testing.T) {
	for name, tc := range map[string]struct {
		reply                   func(http.ResponseWriter)
		status                  int
		msg                     string
		retryable, outOfCredits bool
		retryAfter              time.Duration
	}{
		"plan limit":      {relayReply(432, `{"detail":{"error":"This request exceeds your plan's set usage limit."}}`), 432, "This request exceeds your plan's set usage limit.", false, true, 0},
		"paygo limit":     {relayReply(433, `{"detail":{"error":"pay-as-you-go limit"}}`), 433, "pay-as-you-go limit", false, true, 0},
		"rate limited":    {relayReply(429, `{"detail":{"error":"slow down"}}`, "Retry-After", "2"), 429, "slow down", true, false, 2 * time.Second},
		"live fetch busy": {relayReply(434, `{"detail":{"error":"busy"}}`), 434, "busy", true, false, 0},
		"server error":    {relayReply(500, `{"detail":{"error":"Internal server error."}}`), 500, "Internal server error.", true, false, 0},
		"no grant":        {relayReply(403, `{"is_bifrost_error":false,"status_code":403,"error":{"message":"Relay 'tavily' is not allowed"}}`), 403, "Relay 'tavily' is not allowed", false, false, 0},
		"validation":      {relayReply(422, `{"detail":[{"type":"int_type","loc":["body","max_results"]}]}`), 422, `[{"type":"int_type","loc":["body","max_results"]}]`, false, false, 0},
		"not json":        {relayReply(502, `bad gateway`), 502, "bad gateway", true, false, 0},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFakeRelay(t)
			f.reply = tc.reply
			_, err := NewTavily(f.srv.URL, "tavily", "k", nil).Search(context.Background(), SearchRequest{Query: "q"}, Attribution{})
			var te *TavilyError
			if !errors.As(err, &te) {
				t.Fatalf("err = %v", err)
			}
			if te.Status != tc.status || te.Message != tc.msg || te.Retryable() != tc.retryable || te.OutOfCredits() != tc.outOfCredits || te.RetryAfter != tc.retryAfter {
				t.Errorf("got %+v retryable=%v outOfCredits=%v", te, te.Retryable(), te.OutOfCredits())
			}
		})
	}

	f := newFakeRelay(t)
	f.reply = relayReply(200, `{"results": [`)
	if _, err := NewTavily(f.srv.URL, "tavily", "k", nil).Search(context.Background(), SearchRequest{Query: "q"}, Attribution{}); err == nil || !strings.Contains(err.Error(), "decode") {
		t.Errorf("truncated reply: %v", err)
	}
	f.reply = relayReply(200, `{"results":[{"content":"`+strings.Repeat("x", maxTavilyResponseBytes)+`"}]}`)
	if _, err := NewTavily(f.srv.URL, "tavily", "k", nil).Search(context.Background(), SearchRequest{Query: "q"}, Attribution{}); err == nil || !strings.Contains(err.Error(), "bytes") {
		t.Errorf("oversized reply: %v", err)
	}
}

// TestTavilyRelayContract runs the client against a real gateway relay in
// front of mock Tavily. testenv/relay-tavily.sh sets the environment; without
// it the test is skipped.
func TestTavilyRelayContract(t *testing.T) {
	gw, vk, mock := os.Getenv("HALFLIFE_RELAY_GATEWAY"), os.Getenv("HALFLIFE_RELAY_VK"), os.Getenv("HALFLIFE_RELAY_MOCK")
	if gw == "" || vk == "" || mock == "" {
		t.Skip("needs a gateway with the tavily relay (testenv/relay-tavily.sh)")
	}
	ctx := context.Background()
	tv := NewTavily(gw, "tavily", vk, nil)
	last := func() map[string]any {
		resp, err := http.Get(mock + "/_mock/requests")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		json.NewDecoder(resp.Body).Decode(&out)
		return out["last"].(map[string]any)
	}

	s, err := tv.Search(ctx, SearchRequest{Query: "How much does the Acme Cloud Pro plan cost?", MaxResults: 2}, Attribution{Tenant: "vk-smoke", Report: "hlr_contract"})
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Results) == 0 || s.Usage.Credits != 1 || s.GatewayRequestID == "" {
		t.Errorf("basic search: %+v", s)
	}
	seen := last()
	if seen["path"] != "/search" || seen["x_project_id"] != "vk-smoke" || seen["x_session_id"] != "hlr_contract" || seen["include_usage"] != true || seen["x_bf_vk_present"] != false {
		t.Errorf("upstream saw %v", seen)
	}
	if s, err := tv.Search(ctx, SearchRequest{Query: "q", SearchDepth: "advanced"}, Attribution{}); err != nil || s.Usage.Credits != 2 {
		t.Errorf("advanced search: %+v %v", s, err)
	}
	urls := []string{"https://a.example/1", "https://a.example/2", "https://a.example/3", "https://a.example/4", "https://a.example/5", "https://a.example/mock-fail"}
	if e, err := tv.Extract(ctx, ExtractRequest{URLs: urls}, Attribution{}); err != nil || len(e.Results) != 5 || len(e.FailedResults) != 1 || e.Usage.Credits != 1 {
		t.Errorf("extract: %+v %v", e, err)
	}
	var te *TavilyError
	if _, err := tv.Search(ctx, SearchRequest{Query: "q [mock:432]"}, Attribution{}); !errors.As(err, &te) || !te.OutOfCredits() {
		t.Errorf("432: %v", err)
	}
	if _, err := tv.Search(ctx, SearchRequest{Query: "q [mock:429]"}, Attribution{}); !errors.As(err, &te) || !te.Retryable() || te.RetryAfter != 2*time.Second {
		t.Errorf("429: %v", err)
	}
	if _, err := NewTavily(gw, "tavily", "sk-bf-halflife-smoke", nil).Search(ctx, SearchRequest{Query: "q"}, Attribution{}); !errors.As(err, &te) || te.Status != 403 {
		t.Errorf("ungranted key: %v", err)
	}
	if _, err := NewTavily(gw, "no-such-relay", vk, nil).Search(ctx, SearchRequest{Query: "q"}, Attribution{}); !errors.As(err, &te) || te.Status != 404 {
		t.Errorf("unknown relay: %v", err)
	}
}
