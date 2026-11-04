package halflife

import (
	"strings"
	"testing"
	"time"
)

func TestSpendLedgerWindow(t *testing.T) {
	c := newClock()
	l := newSpendLedger(time.Hour)
	l.now = c.now
	if spent, _ := l.spent("a"); spent != 0 {
		t.Fatalf("nothing charged: %v", spent)
	}
	opened := c.now()
	l.charge("a", 0.25)
	c.advance(59 * time.Minute)
	l.charge("a", 0.25)
	l.charge("a", 0) // free claims don't open or extend anything
	l.charge("b", 1)
	spent, resets := l.spent("a")
	if spent != 0.5 || !resets.Equal(opened.Add(time.Hour)) {
		t.Errorf("in the window: %v, resets %s", spent, resets)
	}
	c.advance(time.Minute)
	if spent, _ := l.spent("a"); spent != 0 {
		t.Errorf("after the window: %v", spent)
	}
	if spent, _ := l.spent("b"); spent != 1 {
		t.Errorf("window is per tenant: %v", spent)
	}
}

func TestPassOver(t *testing.T) {
	const claim = 0.01
	for _, tc := range []struct {
		priority     string
		spent, limit float64
		want         string
	}{
		{PriorityLow, 100, 0, ""}, // no cap
		{PriorityLow, 0, 1, ""},
		{PriorityLow, 0.5, 1, ""},
		{PriorityLow, 0.51, 1, "LOW claims need 50%"},
		{PriorityMedium, 0.51, 1, ""},
		{PriorityMedium, 0.79, 1, ""},
		{PriorityMedium, 0.81, 1, "MEDIUM claims need 20%"},
		{PriorityHigh, 0.99, 1, ""},
		{PriorityHigh, 0.995, 1, "spend cap reached"}, // not enough left for one claim
		{PriorityHigh, 1.2, 1, "spend cap reached"},   // workers overshot
	} {
		got := passOver(tc.priority, tc.spent, tc.limit, claim)
		if (tc.want == "") != (got == "") || !strings.Contains(got, tc.want) {
			t.Errorf("%s at %v of %v: %q, want %q", tc.priority, tc.spent, tc.limit, got, tc.want)
		}
	}
}

var (
	highClaim = extractedClaim{Text: "Acme Cloud Pro costs $49 per month.", Span: "Acme Cloud Pro costs $49 per month", Subject: "Acme Cloud Pro",
		Attribute: "monthly price", AssertedValue: "$49", Category: CategoryPrice, TemporalScope: ScopeCurrent, VolatilityBand: BandWeeksMonths}
	otherHighClaim = extractedClaim{Text: "Acme Cloud Team costs $99 per month.", Span: "Acme Cloud Team costs $99 per month", Subject: "Acme Cloud Team",
		Attribute: "monthly price", AssertedValue: "$99", Category: CategoryPrice, TemporalScope: ScopeCurrent, VolatilityBand: BandWeeksMonths}
	mediumClaim = extractedClaim{Text: "Acme is based in Oslo.", Span: "Acme is based in Oslo", Subject: "Acme",
		Attribute: "headquarters", AssertedValue: "Oslo", Category: CategoryOther, TemporalScope: ScopeCurrent, VolatilityBand: BandYears}
	lowClaim = extractedClaim{Text: "Acme has 300 staff.", Span: "Acme has 300 staff", Subject: "Acme", Attribute: "staff", AssertedValue: "300",
		Category: CategoryOther, TemporalScope: ScopeCurrent, VolatilityBand: BandYears, AsOf: "2026-10"}
	mixedAnswer = "Acme Cloud Pro costs $49 per month. Acme Cloud Team costs $99 per month. Acme is based in Oslo. Acme has 300 staff."
)

func cappedKey(capUSD float64) VerifierKey {
	k := acmeKey
	k.SpendCapUSD = capUSD
	return k
}

func cappedVerifier(t *testing.T, capUSD float64, replies ...gwReply) (*verifier, *fakeSearcher, *fakeGateway) {
	t.Helper()
	all := append([]gwReply{extractionReply(t, lowClaim, mediumClaim, otherHighClaim, highClaim)}, replies...)
	return extractingVerifier(t, func(c *Config) {
		c.Verifiers, c.DisableExtract, c.TavilyCreditUSD = []VerifierKey{cappedKey(capUSD)}, true, 0.01
	}, all...)
}

func claimStatus(r Report, text string) ReportClaim {
	for _, c := range r.Claims {
		if c.Text == text {
			return c
		}
	}
	return ReportClaim{}
}

// $0.01 a claim: the two HIGH claims and the MEDIUM one leave 40% of $0.05, so
// the LOW claim is passed over, and it is the only one.
func TestSpendCapDropsLowClaimsFirst(t *testing.T) {
	supports := judgeReply(judgement{Verdict: judgeSupports, Quote: "Pro plan: $49 per month.", Source: 1})
	v, f, _ := cappedVerifier(t, 0.05, supports, supports, supports)
	r, err := runVerify(t, v, acmeCapture("hlr_1", mixedAnswer))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []extractedClaim{highClaim, otherHighClaim, mediumClaim} {
		if c := claimStatus(r, want.Text); c.Status != ClaimChecked {
			t.Errorf("%s: %s (%s), priority %s", want.Text, c.Status, c.StatusReason, c.Priority)
		}
	}
	low := claimStatus(r, lowClaim.Text)
	if low.Priority != PriorityLow || low.Status != ClaimNotChecked || low.Failure != FailureSpendCap || !strings.Contains(low.StatusReason, "LOW claims need 50%") {
		t.Errorf("low: %+v", low)
	}
	if len(f.calls) != 3 || r.Outcome != OutcomeEvidenceFound {
		t.Errorf("searches %d, outcome %s", len(f.calls), r.Outcome)
	}
	if spent, _ := v.spend.spent("vk-acme"); !near(spent, 0.03) {
		t.Errorf("spent %v", spent)
	}
}

// With less to spend, MEDIUM goes too and HIGH keeps what is left, in order.
func TestSpendCapKeepsTheMoneyForHighClaims(t *testing.T) {
	v, f, _ := cappedVerifier(t, 0.02)
	r, _ := runVerify(t, v, acmeCapture("hlr_1", mixedAnswer))
	if len(f.calls) != 2 {
		t.Fatalf("searches: %d", len(f.calls))
	}
	if high := claimStatus(r, highClaim.Text); high.Status == ClaimNotChecked {
		t.Errorf("high: %+v", high)
	}
	for _, text := range []string{mediumClaim.Text, lowClaim.Text} {
		if c := claimStatus(r, text); c.Status != ClaimNotChecked || !strings.Contains(c.StatusReason, "spend cap reached") {
			t.Errorf("%s: %+v", text, c)
		}
	}
}

// Once nothing can be checked, not even the extraction is bought; the next
// window starts again, and a capped tenant doesn't affect another.
func TestSpendCapSkipsTheAuditUntilTheWindowEnds(t *testing.T) {
	v, f, g := cappedVerifier(t, 0.01)
	clock := newClock()
	v.spend.now = clock.now
	v.spend.charge("vk-acme", 0.01)
	calls := len(g.bodies)
	r, err := runVerify(t, v, acmeCapture("hlr_1", mixedAnswer))
	if err != nil || r.Outcome != OutcomeBudgetExhausted || !strings.Contains(r.OutcomeReason, "spend cap reached") {
		t.Fatalf("capped: %s (%s), err %v", r.Outcome, r.OutcomeReason, err)
	}
	if len(g.bodies) != calls || len(f.calls) != 0 {
		t.Errorf("a capped audit made calls: llm %d, search %d", len(g.bodies)-calls, len(f.calls))
	}
	clock.advance(DefaultSpendWindowSeconds * time.Second)
	if r, _ := runVerify(t, v, acmeCapture("hlr_2", mixedAnswer)); r.Outcome != OutcomeEvidenceFound {
		t.Errorf("next window: %s (%s)", r.Outcome, r.OutcomeReason)
	}
}

// Every claim passed over reads as the budget, not as a search failure.
func TestSpendCapOutcome(t *testing.T) {
	claims := []ReportClaim{{Status: ClaimNotChecked, Failure: FailureSpendCap}}
	if got := outcomeOf(claims, nil); got != OutcomeBudgetExhausted {
		t.Errorf("outcome %s", got)
	}
}

func TestSpendState(t *testing.T) {
	v, _, _ := cappedVerifier(t, 2)
	v.spend.charge("vk-acme", 0.5)
	got, ok := v.spendState()["vk-acme"]
	if !ok || got.CapUSD != 2 || got.SpentUSD != 0.5 || got.Resets.IsZero() {
		t.Errorf("spend %+v", v.spendState())
	}
}

// A "*" entry caps each tenant on its own, and they show once they've spent.
func TestSpendStateOfSharedEntry(t *testing.T) {
	v, _, _ := extractingVerifier(t, func(c *Config) {
		k := sharedKey
		k.SpendCapUSD = 3
		c.Verifiers = []VerifierKey{k}
	})
	v.spend.charge("vk-a", 1)
	v.spend.charge("vk-b", 2)
	s := v.spendState()
	if len(s) != 2 || s["vk-a"].CapUSD != 3 || s["vk-b"].SpentUSD != 2 {
		t.Errorf("spend %+v", s)
	}
}

func TestSpendConfigValidation(t *testing.T) {
	for name, c := range map[string]Config{
		"negative cap":   {Verifiers: []VerifierKey{cappedKey(-1)}},
		"window too low": {SpendWindowSeconds: 59},
		"window too big": {SpendWindowSeconds: maxSpendWindowSeconds + 1},
	} {
		if err := c.withDefaults().validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
