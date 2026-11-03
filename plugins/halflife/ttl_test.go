package halflife

import (
	"testing"
	"time"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/plugins/semanticcache"
)

// Halflife's cap reaches the cache's write: the stack's cache TTL is 1m, and
// with cache_ttl_seconds=10 the entry expires 10s after it is written.
func TestCacheTTLCapsEntryExpiry(t *testing.T) {
	for name, tc := range map[string]struct {
		cap    int
		header time.Duration // the client's x-bf-cache-ttl, 0 if absent
		want   time.Duration
	}{
		"no cap: the cache's TTL":        {0, 0, time.Minute},
		"cap below the cache's TTL":      {10, 0, 10 * time.Second},
		"cap above the cache's TTL":      {120, 0, 2 * time.Minute},
		"longer client TTL is capped":    {10, time.Hour, 10 * time.Second},
		"shorter client TTL is kept":     {10, 5 * time.Second, 5 * time.Second},
		"client TTL alone, cap disabled": {0, 20 * time.Second, 20 * time.Second},
	} {
		t.Run(name, func(t *testing.T) {
			st := newStack(t, stackOptions{halflife: &Config{CacheTTLSeconds: tc.cap}})
			r := st.askWith("vk-a", priceQ, func(ctx *schemas.BifrostContext) {
				if tc.header > 0 {
					ctx.SetValue(semanticcache.CacheTTLKey, tc.header)
				}
			})
			st.settle()
			got := st.store.expiresIn(t, r.cacheID())
			// expires_at has one-second resolution.
			if got > tc.want || got < tc.want-2*time.Second {
				t.Errorf("entry expires in %s, want ~%s", got.Round(time.Second), tc.want)
			}
		})
	}
}

// An entry written under the cap stops being served once it expires.
func TestCappedEntryExpires(t *testing.T) {
	st := newStack(t, stackOptions{halflife: &Config{CacheTTLSeconds: 1}})
	first := st.ask("vk-a", priceQ)
	st.settle()
	if hit := st.ask("vk-a", priceQ); hit.hit() != "direct" {
		t.Fatalf("before expiry: %s", hit.hit())
	}
	time.Sleep(st.store.expiresIn(t, first.cacheID()) + 1100*time.Millisecond)
	if after := st.ask("vk-a", priceQ); after.hit() != "miss" || st.provider.chats.Load() != 2 {
		t.Errorf("after expiry: %s, provider calls %d", after.hit(), st.provider.chats.Load())
	}
}
