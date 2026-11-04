package halflife

import (
	"fmt"
	"maps"
	"slices"
	"sync"
	"time"
)

// Adaptive verification budget (audit §3.3, step 3). A verifier key's budget
// in Bifrost is a hard stop: it answers 402 and everything after it is lost,
// important or not. A tenant's spend_cap_usd sits below it and shapes what is
// bought as the money runs out: LOW claims stop once half the window's cap is
// spent, MEDIUM ones at 80%, and HIGH ones get what is left. Claims passed
// over are NOT_CHECKED and say why.
//
// The ledger is per process, and each worker charges after a claim, so
// concurrent audits can overshoot by a claim per worker; the governance
// budget remains the enforced limit.
const (
	lowClaimsNeed    = 0.5 // share of the cap that must be left to check a LOW claim
	mediumClaimsNeed = 0.2 // and a MEDIUM one
)

// spendLedger adds up what each capped tenant's verification cost in the
// current fixed window, which starts at the tenant's first charge.
type spendLedger struct {
	window time.Duration
	now    func() time.Time

	mu      sync.Mutex
	tenants map[string]*tenantSpend
}

type tenantSpend struct {
	usd    float64
	resets time.Time
}

func newSpendLedger(window time.Duration) *spendLedger {
	return &spendLedger{window: window, now: time.Now, tenants: make(map[string]*tenantSpend)}
}

// entryLocked returns the tenant's current window, or nil if none is open.
func (l *spendLedger) entryLocked(tenant string) *tenantSpend {
	e, ok := l.tenants[tenant]
	if ok && !l.now().Before(e.resets) {
		delete(l.tenants, tenant)
		return nil
	}
	return e
}

func (l *spendLedger) charge(tenant string, usd float64) {
	if usd <= 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	e := l.entryLocked(tenant)
	if e == nil {
		e = &tenantSpend{resets: l.now().Add(l.window)}
		l.tenants[tenant] = e
	}
	e.usd += usd
}

// active lists the tenants with an open window.
func (l *spendLedger) active() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Collect(maps.Keys(l.tenants))
}

func (l *spendLedger) spent(tenant string) (float64, time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if e := l.entryLocked(tenant); e != nil {
		return e.usd, e.resets
	}
	return 0, time.Time{}
}

// passOver says why a claim of this priority is not worth checking with spent
// out of spendCap, or "" if it is. claimUSD is what one more claim is expected
// to cost; a cap of 0 means no cap.
func passOver(priority string, spent, spendCap, claimUSD float64) string {
	if spendCap <= 0 {
		return ""
	}
	left := spendCap - spent
	if left < claimUSD || left <= 0 {
		return fmt.Sprintf("spend cap reached ($%.4f of $%.4f)", spent, spendCap)
	}
	need := 0.0
	switch priority {
	case PriorityLow:
		need = lowClaimsNeed
	case PriorityMedium:
		need = mediumClaimsNeed
	}
	if left/spendCap < need {
		return fmt.Sprintf("spend cap: %s claims need %.0f%% of the cap left ($%.4f of $%.4f spent)", priority, need*100, spent, spendCap)
	}
	return ""
}

// claimUSD is what checking the claim cost.
func (v *verifier) claimUSD(rc *ReportClaim) float64 {
	usd := 0.0
	if rc.Evidence != nil {
		usd = rc.Evidence.Credits * v.creditUSD
	}
	if rc.Judge != nil {
		usd += rc.Judge.CostUSD
	}
	return usd
}

// passOver applies the key's cap to the tenant's spend so far.
func (v *verifier) passOver(key verifierKey, tenant, priority string) string {
	if key.spendCap <= 0 {
		return ""
	}
	spent, _ := v.spend.spent(tenant)
	return passOver(priority, spent, key.spendCap, v.creditUSD)
}

// TenantSpend is a capped tenant's current window.
type TenantSpend struct {
	CapUSD   float64   `json:"cap_usd"`
	SpentUSD float64   `json:"spent_usd"`
	Resets   time.Time `json:"resets_at,omitzero"`
}

// spendState reports the tenants with a cap of their own, and those the "*"
// entry caps once they have spent something.
func (v *verifier) spendState() map[string]TenantSpend {
	out := make(map[string]TenantSpend)
	for tenant, key := range v.keys {
		if tenant != AnyTenant && key.spendCap > 0 {
			spent, resets := v.spend.spent(tenant)
			out[tenant] = TenantSpend{CapUSD: key.spendCap, SpentUSD: spent, Resets: resets}
		}
	}
	shared, capped := v.keys[AnyTenant]
	if !capped || shared.spendCap <= 0 {
		return out
	}
	for _, tenant := range v.spend.active() {
		if _, own := v.keys[tenant]; !own {
			spent, resets := v.spend.spent(tenant)
			out[tenant] = TenantSpend{CapUSD: shared.spendCap, SpentUSD: spent, Resets: resets}
		}
	}
	return out
}
