package halflife

import (
	"fmt"
	"net/url"
	"strings"
	"time"
)

// Defaults applied by Init when a field is left at its zero value.
const (
	DefaultQueueSize            = 256
	DefaultWorkers              = 2
	DefaultMaxClaimsPerResponse = 5
	DefaultTavilyRelay          = "tavily"
	DefaultAuditTimeoutSeconds  = 60
	DefaultDrainTimeoutSeconds  = 5
	// The semantic cache's write timeout (semanticcache.CacheSetTimeout, 30s)
	// plus a margin.
	DefaultCacheWriteSettleSeconds = 35
	DefaultGatewayURL              = "http://127.0.0.1:8080" // Bifrost's default listen address
	DefaultSearchMaxResults        = 5
	DefaultReportStoreSize         = 10000
	DefaultVerifierMaxTokens       = 1024

	maxVerifierModels = 4

	// AnyTenant is the Verifiers entry used for tenants without their own.
	AnyTenant = "*"

	maxCacheTTLSeconds = 30 * 24 * 3600
)

// Config is the plugin's configuration, read from the "halflife" entry of the
// gateway's plugins list.
type Config struct {
	// QueueSize bounds the number of completed responses waiting for audit.
	// When the queue is full, new responses are dropped from auditing (never
	// delayed), so the request path is never blocked by Halflife.
	QueueSize int `json:"queue_size,omitempty"`
	// Workers is the number of background audit workers.
	Workers int `json:"workers,omitempty"`
	// MaxClaimsPerResponse caps how many claims are checked per audited
	// answer, which bounds Tavily spend per response.
	MaxClaimsPerResponse int `json:"max_claims_per_response,omitempty"`
	// TavilyRelay is the name of the Bifrost relay that forwards evidence
	// searches to Tavily, so their cost is governed and attributed by Bifrost.
	TavilyRelay string `json:"tavily_relay,omitempty"`
	// AuditTimeoutSeconds bounds one audit, end to end.
	AuditTimeoutSeconds int `json:"audit_timeout_seconds,omitempty"`
	// DrainTimeoutSeconds is how long shutdown waits for queued audits before
	// cancelling them.
	DrainTimeoutSeconds int `json:"drain_timeout_seconds,omitempty"`
	// VerifierVirtualKey is the ID (not the secret value) of the governance
	// virtual key Halflife's verifier uses for its own LLM calls through the
	// gateway. Requests made with it are never audited: auditing the
	// verifier's answers would recurse. Empty disables the guard.
	VerifierVirtualKey string `json:"verifier_virtual_key,omitempty"`
	// CacheTTLSeconds caps how long Bifrost's semantic cache keeps an answer
	// written through Halflife, so a stale answer cannot outlive it even if
	// no audit flags it. A shorter per-request x-bf-cache-ttl still wins.
	// Zero leaves the cache's own TTL in place.
	CacheTTLSeconds int `json:"cache_ttl_seconds,omitempty"`
	// CacheWriteSettleSeconds bounds how long after a response the semantic
	// cache may still be writing it (its write timeout is 30s). An answer
	// found stale within this window is deleted now and again once the
	// window closes; until then its cache partition bypasses the cache, so
	// a write landing after the first delete is never served.
	CacheWriteSettleSeconds int `json:"cache_write_settle_seconds,omitempty"`

	// GatewayURL is this gateway's own origin, which the verifier sends its
	// relay calls to (they must pass through governance like any caller's).
	GatewayURL string `json:"gateway_url,omitempty"`
	// Verifiers maps each tenant to the virtual key its verification is
	// charged to (D37: one verifier key per tenant, inside the tenant's
	// customer). Tenant "*" covers tenants without an entry; leave it out and
	// those tenants' answers are reported as not verified.
	Verifiers []VerifierKey `json:"verifiers,omitempty"`
	// SearchMaxResults is how many sources one evidence search asks for.
	SearchMaxResults int `json:"search_max_results,omitempty"`
	// ReportStoreSize bounds the reports kept in memory; the oldest go first.
	ReportStoreSize int `json:"report_store_size,omitempty"`
	// VerifierModels are the models the verifier's own LLM calls use, as
	// "provider/model": the first is asked, the rest are sent as Bifrost
	// request fallbacks. The calls go through this gateway with the tenant's
	// verifier key. Empty disables model-based verification.
	VerifierModels []string `json:"verifier_models,omitempty"`
	// VerifierMaxTokens caps each verifier LLM reply, reasoning included.
	VerifierMaxTokens int `json:"verifier_max_tokens,omitempty"`
	// ModelCutoffs maps the model that answered ("provider/model" or
	// "model", a trailing "*" matching a prefix) to its knowledge cutoff, as
	// "YYYY", "YYYY-MM" or "YYYY-MM-DD". Verification priority measures how
	// old the answer's information is from it; unknown models are assumed
	// 18 months old. None are built in: vendors publish cutoffs
	// inconsistently, so the operator supplies the ones they trust.
	ModelCutoffs map[string]string `json:"model_cutoffs,omitempty"`
	// PriorityBoostCategories raises claims of these categories one
	// priority level (e.g. price, law: costly when wrong).
	PriorityBoostCategories []string `json:"priority_boost_categories,omitempty"`
	// PreferredDomains maps a claim category to domains its searches prefer
	// (include_domains_mode "prefer": boosted, never exclusive), e.g.
	// {"version": ["go.dev", "github.com"]}.
	PreferredDomains map[string][]string `json:"preferred_domains,omitempty"`
	// DisableExtract turns off the one Tavily Extract per claim made when
	// the search snippets don't mention the claim's subject and value.
	DisableExtract bool `json:"disable_extract,omitempty"`
	// VerifierReasoningEffort, when set, is sent with each verifier LLM call
	// as reasoning.effort: none, minimal, low, medium or high. Empty sends
	// nothing, because models without reasoning may reject the parameter.
	VerifierReasoningEffort string `json:"verifier_reasoning_effort,omitempty"`
}

// VerifierKey is the governance virtual key that pays for one tenant's
// verification.
type VerifierKey struct {
	// Tenant is the virtual key ID of the tenant's own requests, or "*".
	Tenant string `json:"tenant"`
	// VirtualKeyID is the verifier key's ID. Requests made with it are never
	// audited (recursion guard).
	VirtualKeyID string `json:"virtual_key_id"`
	// VirtualKey is the verifier key's secret value, or env.VAR to read it
	// from the environment (recommended: plugin configs are returned by the
	// gateway's plugins API).
	VirtualKey string `json:"virtual_key"`
}

func (c Config) auditTimeout() time.Duration {
	return time.Duration(c.AuditTimeoutSeconds) * time.Second
}
func (c Config) drainTimeout() time.Duration {
	return time.Duration(c.DrainTimeoutSeconds) * time.Second
}
func (c Config) cacheWriteSettle() time.Duration {
	return time.Duration(c.CacheWriteSettleSeconds) * time.Second
}
func (c Config) cacheTTL() time.Duration {
	return time.Duration(c.CacheTTLSeconds) * time.Second
}

// withDefaults returns a copy of c with zero fields set to their defaults.
func (c Config) withDefaults() Config {
	if c.QueueSize == 0 {
		c.QueueSize = DefaultQueueSize
	}
	if c.Workers == 0 {
		c.Workers = DefaultWorkers
	}
	if c.MaxClaimsPerResponse == 0 {
		c.MaxClaimsPerResponse = DefaultMaxClaimsPerResponse
	}
	if c.AuditTimeoutSeconds == 0 {
		c.AuditTimeoutSeconds = DefaultAuditTimeoutSeconds
	}
	if c.DrainTimeoutSeconds == 0 {
		c.DrainTimeoutSeconds = DefaultDrainTimeoutSeconds
	}
	if c.CacheWriteSettleSeconds == 0 {
		c.CacheWriteSettleSeconds = DefaultCacheWriteSettleSeconds
	}
	c.VerifierVirtualKey = strings.TrimSpace(c.VerifierVirtualKey)
	c.TavilyRelay = strings.TrimSpace(c.TavilyRelay)
	if c.TavilyRelay == "" {
		c.TavilyRelay = DefaultTavilyRelay
	}
	c.GatewayURL = strings.TrimRight(strings.TrimSpace(c.GatewayURL), "/")
	if c.GatewayURL == "" {
		c.GatewayURL = DefaultGatewayURL
	}
	if c.SearchMaxResults == 0 {
		c.SearchMaxResults = DefaultSearchMaxResults
	}
	if c.ReportStoreSize == 0 {
		c.ReportStoreSize = DefaultReportStoreSize
	}
	if c.VerifierMaxTokens == 0 {
		c.VerifierMaxTokens = DefaultVerifierMaxTokens
	}
	models := make([]string, len(c.VerifierModels))
	for i, m := range c.VerifierModels {
		models[i] = strings.TrimSpace(m)
	}
	c.VerifierModels = models
	c.VerifierReasoningEffort = strings.TrimSpace(c.VerifierReasoningEffort)
	if c.PriorityBoostCategories != nil {
		cats := make([]string, len(c.PriorityBoostCategories))
		for i, cat := range c.PriorityBoostCategories {
			cats[i] = strings.TrimSpace(cat)
		}
		c.PriorityBoostCategories = cats
	}
	if c.PreferredDomains != nil {
		domains := make(map[string][]string, len(c.PreferredDomains))
		for cat, ds := range c.PreferredDomains {
			clean := make([]string, len(ds))
			for i, d := range ds {
				clean[i] = strings.ToLower(strings.TrimSpace(d))
			}
			domains[strings.TrimSpace(cat)] = clean
		}
		c.PreferredDomains = domains
	}
	verifiers := make([]VerifierKey, len(c.Verifiers))
	for i, v := range c.Verifiers {
		verifiers[i] = VerifierKey{Tenant: strings.TrimSpace(v.Tenant), VirtualKeyID: strings.TrimSpace(v.VirtualKeyID), VirtualKey: strings.TrimSpace(v.VirtualKey)}
	}
	c.Verifiers = verifiers
	return c
}

// validate reports the first out-of-range field.
func (c Config) validate() error {
	switch {
	case c.QueueSize < 1 || c.QueueSize > 65536:
		return fmt.Errorf("halflife: queue_size must be between 1 and 65536, got %d", c.QueueSize)
	case c.Workers < 1 || c.Workers > 64:
		return fmt.Errorf("halflife: workers must be between 1 and 64, got %d", c.Workers)
	case c.MaxClaimsPerResponse < 1 || c.MaxClaimsPerResponse > 20:
		return fmt.Errorf("halflife: max_claims_per_response must be between 1 and 20, got %d", c.MaxClaimsPerResponse)
	case c.AuditTimeoutSeconds < 1 || c.AuditTimeoutSeconds > 600:
		return fmt.Errorf("halflife: audit_timeout_seconds must be between 1 and 600, got %d", c.AuditTimeoutSeconds)
	case c.DrainTimeoutSeconds < 1 || c.DrainTimeoutSeconds > 60:
		return fmt.Errorf("halflife: drain_timeout_seconds must be between 1 and 60, got %d", c.DrainTimeoutSeconds)
	case c.CacheWriteSettleSeconds < 31 || c.CacheWriteSettleSeconds > 600:
		return fmt.Errorf("halflife: cache_write_settle_seconds must be between 31 and 600 (the cache writes for up to 30s), got %d", c.CacheWriteSettleSeconds)
	case c.CacheTTLSeconds < 0 || c.CacheTTLSeconds > maxCacheTTLSeconds:
		return fmt.Errorf("halflife: cache_ttl_seconds must be between 0 and %d, got %d", maxCacheTTLSeconds, c.CacheTTLSeconds)
	case c.SearchMaxResults < 1 || c.SearchMaxResults > 20:
		return fmt.Errorf("halflife: search_max_results must be between 1 and 20, got %d", c.SearchMaxResults)
	case c.ReportStoreSize < 1 || c.ReportStoreSize > 1_000_000:
		return fmt.Errorf("halflife: report_store_size must be between 1 and 1000000, got %d", c.ReportStoreSize)
	case c.VerifierMaxTokens < 64 || c.VerifierMaxTokens > 16384:
		return fmt.Errorf("halflife: verifier_max_tokens must be between 64 and 16384, got %d", c.VerifierMaxTokens)
	case len(c.PriorityBoostCategories) > len(claimCategories):
		return fmt.Errorf("halflife: priority_boost_categories lists %d categories; there are %d", len(c.PriorityBoostCategories), len(claimCategories))
	case len(c.VerifierModels) > maxVerifierModels:
		return fmt.Errorf("halflife: verifier_models takes at most %d models, got %d", maxVerifierModels, len(c.VerifierModels))
	}
	switch c.VerifierReasoningEffort {
	case "", "none", "minimal", "low", "medium", "high":
	default:
		return fmt.Errorf("halflife: verifier_reasoning_effort must be none, minimal, low, medium or high, got %q", c.VerifierReasoningEffort)
	}
	if _, err := newCutoffTable(c.ModelCutoffs); err != nil {
		return err
	}
	for i, cat := range c.PriorityBoostCategories {
		if !isCategory(cat) {
			return fmt.Errorf("halflife: priority_boost_categories[%d] must be one of %s, got %q", i, strings.Join(claimCategories, ", "), cat)
		}
	}
	for cat, domains := range c.PreferredDomains {
		if !isCategory(cat) {
			return fmt.Errorf("halflife: preferred_domains key must be one of %s, got %q", strings.Join(claimCategories, ", "), cat)
		}
		for _, d := range domains {
			if d == "" || strings.ContainsAny(d, "/: ") {
				return fmt.Errorf("halflife: preferred_domains[%s] must hold bare domains like go.dev, got %q", cat, d)
			}
		}
	}
	for i, m := range c.VerifierModels {
		if provider, model, ok := strings.Cut(m, "/"); !ok || provider == "" || model == "" {
			return fmt.Errorf("halflife: verifier_models[%d] must be provider/model, got %q", i, m)
		}
	}
	if u, err := url.Parse(c.GatewayURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("halflife: gateway_url must be an http(s) origin, got %q", c.GatewayURL)
	}
	seen := map[string]bool{}
	for i, v := range c.Verifiers {
		switch {
		case v.Tenant == "" || v.VirtualKeyID == "" || v.VirtualKey == "":
			return fmt.Errorf("halflife: verifiers[%d] needs tenant, virtual_key_id and virtual_key", i)
		case seen[v.Tenant]:
			return fmt.Errorf("halflife: verifiers[%d]: tenant %q is listed twice", i, v.Tenant)
		case v.VirtualKeyID == v.Tenant:
			return fmt.Errorf("halflife: verifiers[%d]: tenant %q cannot verify with its own key", i, v.Tenant)
		}
		seen[v.Tenant] = true
	}
	return nil
}

func isCategory(c string) bool {
	for _, k := range claimCategories {
		if c == k {
			return true
		}
	}
	return false
}

// verifierKeyIDs returns every key whose requests the recursion guard skips.
func (c Config) verifierKeyIDs() map[string]bool {
	ids := map[string]bool{}
	if c.VerifierVirtualKey != "" {
		ids[c.VerifierVirtualKey] = true
	}
	for _, v := range c.Verifiers {
		ids[v.VirtualKeyID] = true
	}
	return ids
}
