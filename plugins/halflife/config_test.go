package halflife

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestConfigDefaults(t *testing.T) {
	got := Config{TavilyRelay: "  ", VerifierVirtualKey: " \t"}.withDefaults()
	want := Config{QueueSize: DefaultQueueSize, Workers: DefaultWorkers, MaxClaimsPerResponse: DefaultMaxClaimsPerResponse, TavilyRelay: DefaultTavilyRelay,
		AuditTimeoutSeconds: DefaultAuditTimeoutSeconds, DrainTimeoutSeconds: DefaultDrainTimeoutSeconds}
	if got != want {
		t.Fatalf("withDefaults() = %+v, want %+v", got, want)
	}
	set := Config{QueueSize: 8, Workers: 1, MaxClaimsPerResponse: 3, TavilyRelay: "tavily-eu", AuditTimeoutSeconds: 9, DrainTimeoutSeconds: 2, VerifierVirtualKey: "vk-verifier"}
	if got := set.withDefaults(); got != set {
		t.Errorf("withDefaults() changed explicit values: %+v", got)
	}
}

func TestConfigValidate(t *testing.T) {
	ok := Config{}.withDefaults()
	if err := ok.validate(); err != nil {
		t.Fatalf("defaults invalid: %v", err)
	}
	for name, tc := range map[string]struct {
		mutate func(*Config)
		field  string
	}{
		"queue too small":  {func(c *Config) { c.QueueSize = -1 }, "queue_size"},
		"queue too large":  {func(c *Config) { c.QueueSize = 65537 }, "queue_size"},
		"no workers":       {func(c *Config) { c.Workers = -1 }, "workers"},
		"too many workers": {func(c *Config) { c.Workers = 65 }, "workers"},
		"claims too few":   {func(c *Config) { c.MaxClaimsPerResponse = -1 }, "max_claims_per_response"},
		"claims too many":  {func(c *Config) { c.MaxClaimsPerResponse = 21 }, "max_claims_per_response"},
		"audit timeout":    {func(c *Config) { c.AuditTimeoutSeconds = 601 }, "audit_timeout_seconds"},
		"drain timeout":    {func(c *Config) { c.DrainTimeoutSeconds = -1 }, "drain_timeout_seconds"},
	} {
		c := ok
		tc.mutate(&c)
		err := c.validate()
		if err == nil || !strings.Contains(err.Error(), tc.field) {
			t.Errorf("%s: err = %v, want mention of %s", name, err, tc.field)
		}
	}
}

// The gateway hands plugin config over as JSON from config.json.
func TestConfigJSON(t *testing.T) {
	var c Config
	raw := `{"queue_size":32,"workers":4,"max_claims_per_response":7,"tavily_relay":"tv","audit_timeout_seconds":30,"drain_timeout_seconds":3,"verifier_virtual_key":"vk-v"}`
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		t.Fatal(err)
	}
	if c != (Config{QueueSize: 32, Workers: 4, MaxClaimsPerResponse: 7, TavilyRelay: "tv", AuditTimeoutSeconds: 30, DrainTimeoutSeconds: 3, VerifierVirtualKey: "vk-v"}) {
		t.Errorf("decoded %+v", c)
	}
	out, _ := json.Marshal(Config{})
	if string(out) != "{}" {
		t.Errorf("zero config marshals to %s, want {}", out)
	}
}
