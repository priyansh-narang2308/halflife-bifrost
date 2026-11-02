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
}

func (c Config) auditTimeout() time.Duration {
	return time.Duration(c.AuditTimeoutSeconds) * time.Second
}
func (c Config) drainTimeout() time.Duration {
	return time.Duration(c.DrainTimeoutSeconds) * time.Second
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
	}
	return nil
}
