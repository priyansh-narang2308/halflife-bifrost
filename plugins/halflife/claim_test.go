package halflife

import (
	"strings"
	"testing"
)

func TestPickClaim(t *testing.T) {
	for _, tc := range []struct {
		name, answer, want string
		ok                 bool
	}{
		{"price over chatter", "Great question! Acme Cloud Pro costs $49 per month. Let me know if you need more.",
			"Acme Cloud Pro costs $49 per month.", true},
		{"version with temporal word", "Go is a language from Google. The latest stable version of Go is 1.27.3, released in August 2026.",
			"The latest stable version of Go is 1.27.3, released in August 2026.", true},
		{"markdown list", "## Plans\n- **Starter** is free.\n- **Pro** costs $20 per seat, as of 2026.\n",
			"Pro costs $20 per seat, as of 2026.", true},
		{"hedged numbers lose to a plain fact", "It might cost around $30 or $40, I think. The current CEO of Acme is Jane Doe.",
			"The current CEO of Acme is Jane Doe.", true},
		{"questions are skipped", "Did Acme raise prices in 2026? Ask their sales team.", "", false},
		{"nothing checkable", "Recursion is when a function calls itself. It is a useful idea.", "", false},
		{"too short", "Yes, $5.", "", false},
		{"empty", "", "", false},
	} {
		got, ok := pickClaim(tc.answer)
		if got != tc.want || ok != tc.ok {
			t.Errorf("%s: pickClaim = %q, %t; want %q, %t", tc.name, got, ok, tc.want, tc.ok)
		}
	}
}

func TestPickClaimTiesKeepEarlierSentence(t *testing.T) {
	got, _ := pickClaim("Plan A costs $10 per month. Plan B costs $20 per month.")
	if got != "Plan A costs $10 per month." {
		t.Errorf("got %q", got)
	}
}

func TestClaimCutToLimit(t *testing.T) {
	long := "The latest price is $5 " + strings.Repeat("and more words ", 60) + "today."
	got, ok := pickClaim(long)
	if !ok || len(got) > maxClaimBytes {
		t.Errorf("claim of %d bytes (ok=%t)", len(got), ok)
	}
}

func TestBuildQuery(t *testing.T) {
	for _, tc := range []struct{ claim, question, want string }{
		{"Acme Cloud Pro costs $49 per month.", "How much is Acme Cloud Pro?", "Acme Cloud Pro costs $49 per month."},
		{"It costs $49 per month.", "How much is Acme Cloud Pro?", "How much is Acme Cloud Pro? It costs $49 per month."},
		{"It costs $49 per month.", "", "It costs $49 per month."},
		{"Its  latest\n version is 2.4.", "What is Bifrost?", "What is Bifrost? Its latest version is 2.4."},
	} {
		if got := buildQuery(tc.claim, tc.question); got != tc.want {
			t.Errorf("buildQuery(%q, %q) = %q, want %q", tc.claim, tc.question, got, tc.want)
		}
	}
	if q := buildQuery("It is "+strings.Repeat("x", 500), strings.Repeat("q", 200)); len(q) > maxQueryBytes {
		t.Errorf("query of %d bytes", len(q))
	}
}
