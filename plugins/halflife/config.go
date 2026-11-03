package halflife

import (
	"fmt"
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
	}
	return nil
}
