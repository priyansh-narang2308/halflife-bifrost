package halflife

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Tavily is Halflife's client for Tavily Search and Extract. It never calls
// Tavily directly: every request goes to the gateway's relay
// (/relay/{name}/search), authenticated with the verifier's virtual key, so
// Bifrost injects the Tavily credential, enforces the key's relay grant and
// budgets, and prices the call from the response's usage.credits.
//
// Requests always carry include_usage (the relay's units_path reads it) and an
// explicit search depth; auto_parameters is never sent, since it can switch a
// search to advanced and double its cost.
type Tavily struct {
	baseURL    string // gateway origin, e.g. http://127.0.0.1:8080
	relay      string // relay name, Config.TavilyRelay
	virtualKey string // the verifier key's secret value; sent only to the gateway
	client     *http.Client
}

// maxTavilyResponseBytes bounds a relayed reply read into memory.
const maxTavilyResponseBytes = 4 << 20

// NewTavily returns a client for the relay named relay on the gateway at
// baseURL. virtualKey is the secret value of the verifier's virtual key.
func NewTavily(baseURL, relay, virtualKey string, client *http.Client) *Tavily {
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	return &Tavily{baseURL: strings.TrimRight(baseURL, "/"), relay: relay, virtualKey: virtualKey, client: client}
}

// Attribution tags a call in Tavily's own usage records: the tenant whose
// answer is being verified (X-Project-ID) and the report (X-Session-Id).
type Attribution struct {
	Tenant string
	Report string
}

// SearchRequest is the subset of Tavily Search parameters Halflife uses.
type SearchRequest struct {
	Query          string   `json:"query"`
	SearchDepth    string   `json:"search_depth"` // basic when empty
	MaxResults     int      `json:"max_results,omitempty"`
	Topic          string   `json:"topic,omitempty"`
	TimeRange      string   `json:"time_range,omitempty"`
	IncludeDomains []string `json:"include_domains,omitempty"`
	MaxAgeHours    int      `json:"max_age_hours,omitempty"`
}

// SearchResult is one source returned by Search.
type SearchResult struct {
	Title         string  `json:"title"`
	URL           string  `json:"url"`
	Content       string  `json:"content"`
	Score         float64 `json:"score"`
	PublishedDate string  `json:"published_date,omitempty"`
}

// SearchResponse is Tavily's reply, with the credits it reported.
type SearchResponse struct {
	Results      []SearchResult `json:"results"`
	ResponseTime float64        `json:"response_time"`
	RequestID    string         `json:"request_id"`
	Usage        TavilyUsage    `json:"usage"`
	// GatewayRequestID is the gateway's id for the relayed call
	// (X-Bifrost-Request-Id), which its relay log is keyed on.
	GatewayRequestID string `json:"-"`
}

// TavilyUsage is the usage block include_usage adds.
type TavilyUsage struct {
	Credits float64 `json:"credits"`
}

// ExtractResult is one page Extract fetched.
type ExtractResult struct {
	URL        string `json:"url"`
	RawContent string `json:"raw_content"`
}

// ExtractFailure is one URL Extract could not fetch; it is not billed.
type ExtractFailure struct {
	URL   string `json:"url"`
	Error string `json:"error"`
}

// ExtractResponse is Tavily's Extract reply. Extract bills per 5 successful
// URLs accrued across calls, so one call can report 0 credits.
type ExtractResponse struct {
	Results          []ExtractResult  `json:"results"`
	FailedResults    []ExtractFailure `json:"failed_results"`
	ResponseTime     float64          `json:"response_time"`
	RequestID        string           `json:"request_id"`
	Usage            TavilyUsage      `json:"usage"`
	GatewayRequestID string           `json:"-"`
}

// TavilyError is a non-2xx reply, from Tavily or from the gateway in front of
// it (a missing grant, an exhausted budget, an unknown route).
type TavilyError struct {
	Status     int
	Message    string
	RetryAfter time.Duration // from Retry-After on 429, else 0
}

func (e *TavilyError) Error() string {
	return fmt.Sprintf("tavily relay: %d %s", e.Status, e.Message)
}

// Retryable reports whether the same call may succeed later unchanged: rate
// limits, Tavily's live-fetch capacity (434) and server errors.
func (e *TavilyError) Retryable() bool {
	return e.Status == http.StatusTooManyRequests || e.Status == 434 || e.Status >= 500
}

// OutOfCredits reports Tavily's plan (432) or pay-as-you-go (433) limit.
// Nothing should be retried until the limit changes.
func (e *TavilyError) OutOfCredits() bool { return e.Status == 432 || e.Status == 433 }

// Search runs one Tavily search through the relay.
func (t *Tavily) Search(ctx context.Context, req SearchRequest, attr Attribution) (*SearchResponse, error) {
	if strings.TrimSpace(req.Query) == "" {
		return nil, errors.New("tavily relay: empty query")
	}
	if req.SearchDepth == "" {
		req.SearchDepth = "basic"
	}
	body := struct {
		SearchRequest
		IncludeUsage bool `json:"include_usage"`
	}{req, true}
	var out SearchResponse
	id, err := t.post(ctx, "search", body, attr, &out)
	if err != nil {
		return nil, err
	}
	out.GatewayRequestID = id
	return &out, nil
}

// Extract fetches the given pages through the relay at basic depth.
func (t *Tavily) Extract(ctx context.Context, urls []string, attr Attribution) (*ExtractResponse, error) {
	if len(urls) == 0 || len(urls) > 20 {
		return nil, fmt.Errorf("tavily relay: extract takes 1 to 20 URLs, got %d", len(urls))
	}
	body := map[string]any{"urls": urls, "extract_depth": "basic", "include_usage": true}
	var out ExtractResponse
	id, err := t.post(ctx, "extract", body, attr, &out)
	if err != nil {
		return nil, err
	}
	out.GatewayRequestID = id
	return &out, nil
}

func (t *Tavily) post(ctx context.Context, op string, body any, attr Attribution, out any) (string, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return "", err
	}
	url := t.baseURL + "/relay/" + t.relay + "/" + op
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-bf-vk", t.virtualKey)
	if attr.Tenant != "" {
		req.Header.Set("X-Project-ID", attr.Tenant)
	}
	if attr.Report != "" {
		req.Header.Set("X-Session-Id", attr.Report)
	}
	resp, err := t.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("tavily relay: %s: %w", op, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxTavilyResponseBytes+1))
	if err != nil {
		return "", fmt.Errorf("tavily relay: %s: read: %w", op, err)
	}
	if len(raw) > maxTavilyResponseBytes {
		return "", fmt.Errorf("tavily relay: %s: reply over %d bytes", op, maxTavilyResponseBytes)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return "", &TavilyError{Status: resp.StatusCode, Message: errorMessage(raw), RetryAfter: retryAfter(resp.Header.Get("Retry-After"))}
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return "", fmt.Errorf("tavily relay: %s: decode: %w", op, err)
	}
	return resp.Header.Get("X-Bifrost-Request-Id"), nil
}

// errorMessage reads the message from Tavily's {"detail":{"error":...}} or
// the gateway's {"error":{"message":...}}, else a clipped body.
func errorMessage(raw []byte) string {
	var e struct {
		Detail json.RawMessage `json:"detail"`
		Error  struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(raw, &e) == nil {
		var detail struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(e.Detail, &detail) == nil && detail.Error != "" {
			return detail.Error
		}
		if e.Error.Message != "" {
			return e.Error.Message
		}
		if len(e.Detail) > 0 { // validation errors: a list
			return clipString(string(e.Detail), 200)
		}
	}
	return clipString(strings.TrimSpace(string(raw)), 200)
}

func retryAfter(v string) time.Duration {
	if secs, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && secs > 0 {
		return time.Duration(secs) * time.Second
	}
	return 0
}

func clipString(s string, n int) string {
	if c, cut := clip(s, n); cut {
		return c + "…"
	}
	return s
}
