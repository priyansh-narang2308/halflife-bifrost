package halflife

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestConfigDefaults(t *testing.T) {
	got := Config{TavilyRelay: "  ", VerifierVirtualKey: " \t"}.withDefaults()
	want := Config{QueueSize: DefaultQueueSize, Workers: DefaultWorkers, MaxClaimsPerResponse: DefaultMaxClaimsPerResponse, TavilyRelay: DefaultTavilyRelay,
		AuditTimeoutSeconds: DefaultAuditTimeoutSeconds, DrainTimeoutSeconds: DefaultDrainTimeoutSeconds,
		CacheWriteSettleSeconds: DefaultCacheWriteSettleSeconds, GatewayURL: DefaultGatewayURL, Verifiers: []VerifierKey{},
		SearchMaxResults: DefaultSearchMaxResults, ReportStoreSize: DefaultReportStoreSize,
		VerifierModels: []string{}, VerifierMaxTokens: DefaultVerifierMaxTokens}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("withDefaults() = %+v, want %+v", got, want)
	}
	set := Config{QueueSize: 8, Workers: 1, MaxClaimsPerResponse: 3, TavilyRelay: "tavily-eu", AuditTimeoutSeconds: 9, DrainTimeoutSeconds: 2, VerifierVirtualKey: "vk-verifier",
		CacheTTLSeconds: 600, CacheWriteSettleSeconds: 45, GatewayURL: "https://gw.example:8443", SearchMaxResults: 3, ReportStoreSize: 50,
		Verifiers:      []VerifierKey{{Tenant: "vk-acme", VirtualKeyID: "vk-verifier-acme", VirtualKey: "env.HL_ACME"}},
		VerifierModels: []string{"openrouter/a:free", "openrouter/b:free"}, VerifierMaxTokens: 512, VerifierReasoningEffort: "low"}
	if got := set.withDefaults(); !reflect.DeepEqual(got, set) {
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
		"queue too small":                        {func(c *Config) { c.QueueSize = -1 }, "queue_size"},
		"queue too large":                        {func(c *Config) { c.QueueSize = 65537 }, "queue_size"},
		"no workers":                             {func(c *Config) { c.Workers = -1 }, "workers"},
		"too many workers":                       {func(c *Config) { c.Workers = 65 }, "workers"},
		"claims too few":                         {func(c *Config) { c.MaxClaimsPerResponse = -1 }, "max_claims_per_response"},
		"claims too many":                        {func(c *Config) { c.MaxClaimsPerResponse = 21 }, "max_claims_per_response"},
		"audit timeout":                          {func(c *Config) { c.AuditTimeoutSeconds = 601 }, "audit_timeout_seconds"},
		"drain timeout":                          {func(c *Config) { c.DrainTimeoutSeconds = -1 }, "drain_timeout_seconds"},
		"settle under the cache's write timeout": {func(c *Config) { c.CacheWriteSettleSeconds = 30 }, "cache_write_settle_seconds"},
		"settle too long":                        {func(c *Config) { c.CacheWriteSettleSeconds = 601 }, "cache_write_settle_seconds"},
		"negative ttl":                           {func(c *Config) { c.CacheTTLSeconds = -1 }, "cache_ttl_seconds"},
		"ttl too long":                           {func(c *Config) { c.CacheTTLSeconds = 30*24*3600 + 1 }, "cache_ttl_seconds"},
		"too many sources":                       {func(c *Config) { c.SearchMaxResults = 21 }, "search_max_results"},
		"no report store":                        {func(c *Config) { c.ReportStoreSize = -1 }, "report_store_size"},
		"gateway not a URL":                      {func(c *Config) { c.GatewayURL = "127.0.0.1:8080" }, "gateway_url"},
		"gateway not http":                       {func(c *Config) { c.GatewayURL = "ftp://gw" }, "gateway_url"},
		"verifier without a key":                 {func(c *Config) { c.Verifiers = []VerifierKey{{Tenant: "vk-a", VirtualKeyID: "vk-v"}} }, "verifiers[0]"},
		"tenant listed twice": {func(c *Config) {
			c.Verifiers = []VerifierKey{{Tenant: "*", VirtualKeyID: "vk-v", VirtualKey: "k"}, {Tenant: "*", VirtualKeyID: "vk-w", VirtualKey: "k"}}
		}, "listed twice"},
		"verifier reply too short":              {func(c *Config) { c.VerifierMaxTokens = 63 }, "verifier_max_tokens"},
		"verifier reply too long":               {func(c *Config) { c.VerifierMaxTokens = 16385 }, "verifier_max_tokens"},
		"too many verifier models":              {func(c *Config) { c.VerifierModels = []string{"a/1", "a/2", "a/3", "a/4", "a/5"} }, "at most 4"},
		"verifier model without a provider":     {func(c *Config) { c.VerifierModels = []string{"gpt-4o"} }, "verifier_models[0]"},
		"verifier model with an empty provider": {func(c *Config) { c.VerifierModels = []string{"openai/ok", "/x"} }, "verifier_models[1]"},
		"unknown reasoning effort":              {func(c *Config) { c.VerifierReasoningEffort = "max" }, "verifier_reasoning_effort"},
		"tenant verifies itself":                {func(c *Config) { c.Verifiers = []VerifierKey{{Tenant: "vk-a", VirtualKeyID: "vk-a", VirtualKey: "k"}} }, "own key"},
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
	raw := `{"queue_size":32,"workers":4,"max_claims_per_response":7,"tavily_relay":"tv","audit_timeout_seconds":30,"drain_timeout_seconds":3,"verifier_virtual_key":"vk-v","cache_ttl_seconds":90,"cache_write_settle_seconds":40}`
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(c, Config{QueueSize: 32, Workers: 4, MaxClaimsPerResponse: 7, TavilyRelay: "tv", AuditTimeoutSeconds: 30, DrainTimeoutSeconds: 3, VerifierVirtualKey: "vk-v", CacheTTLSeconds: 90, CacheWriteSettleSeconds: 40}) {
		t.Errorf("decoded %+v", c)
	}
	out, _ := json.Marshal(Config{})
	if string(out) != "{}" {
		t.Errorf("zero config marshals to %s, want {}", out)
	}
}

func TestConfigVerifiersJSON(t *testing.T) {
	var c Config
	raw := `{"gateway_url":"http://127.0.0.1:8090/","verifiers":[{"tenant":" vk-acme ","virtual_key_id":"vk-verifier-acme","virtual_key":"env.HL_VK_ACME"},{"tenant":"*","virtual_key_id":"vk-verifier","virtual_key":"sk-literal"}]}`
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		t.Fatal(err)
	}
	c = c.withDefaults()
	if err := c.validate(); err != nil {
		t.Fatal(err)
	}
	if c.GatewayURL != "http://127.0.0.1:8090" || c.Verifiers[0].Tenant != "vk-acme" {
		t.Errorf("normalized = %+v", c)
	}
	c.VerifierVirtualKey = "vk-legacy"
	if got := c.verifierKeyIDs(); !reflect.DeepEqual(got, map[string]bool{"vk-legacy": true, "vk-verifier-acme": true, "vk-verifier": true}) {
		t.Errorf("guarded keys = %v", got)
	}
}
