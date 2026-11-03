package halflife

import "testing"

func judgedClaim(verdict, observed string, quoteSource int, sources ...ReportSource) *ReportClaim {
	return &ReportClaim{
		Subject: "Acme Cloud Pro", Category: CategoryPrice, AssertedValue: "$49", Verdict: verdict, ObservedValue: observed,
		Quote:    &ReportQuote{Text: "Pro plan: $59 per month", Source: quoteSource, Verified: quoteSource >= 0},
		Evidence: &ReportSources{Sources: sources},
	}
}

var (
	official   = ReportSource{URL: "https://www.acme.example/pricing", Snippet: "Pro plan: $59 per month"}
	officialEU = ReportSource{URL: "https://eu.acme.example/pricing", Snippet: "Pro plan: $59 per month"} // same registrable domain
	news       = ReportSource{URL: "https://news.example/acme", Snippet: "Acme Pro now costs $59 per month."}
	review     = ReportSource{URL: "https://reviews.example/acme", Snippet: "We paid $59 per month for Pro."}
	unrelated  = ReportSource{URL: "https://other.example/x", Snippet: "Acme opened an office."}
)

func TestEvidenceStrength(t *testing.T) {
	cases := []struct {
		name      string
		rc        *ReportClaim
		preferred []string
		want      string
	}{
		{"primary and an independent domain", judgedClaim(VerdictCurrent, "$59 per month", 0, official, news), nil, StrengthStrong},
		{"one primary only", judgedClaim(VerdictCurrent, "$59 per month", 0, official, unrelated), nil, StrengthModerate},
		{"two hosts, one registrable domain", judgedClaim(VerdictCurrent, "$59 per month", 0, official, officialEU), nil, StrengthModerate},
		{"two non-primary domains", judgedClaim(VerdictCurrent, "$59 per month", 1, unrelated, news, review), nil, StrengthModerate},
		{"one non-primary domain", judgedClaim(VerdictCurrent, "$59 per month", 1, unrelated, news), nil, StrengthWeak},
		{"preferred domain is primary", judgedClaim(VerdictContradicted, "$59 per month", 1, unrelated, news, review), []string{"news.example"}, StrengthStrong},
		{"unverified quote never STRONG", judgedClaim(VerdictCurrent, "$59 per month", -1, official, news), nil, StrengthModerate},
		{"no quote", &ReportClaim{Verdict: VerdictCurrent, Evidence: &ReportSources{}}, nil, StrengthWeak},
	}
	for _, tc := range cases {
		got, reason := evidenceStrength(tc.rc, tc.preferred)
		if got != tc.want {
			t.Errorf("%s: got %s (%s), want %s", tc.name, got, reason, tc.want)
		}
	}
}

func TestFreshnessScoreTable(t *testing.T) {
	for _, tc := range []struct {
		verdict string
		sources []ReportSource
		score   int
	}{
		{VerdictCurrent, []ReportSource{official, news}, 90},
		{VerdictCurrent, []ReportSource{official}, 75},
		{VerdictOutdated, []ReportSource{official, news}, 5},
		{VerdictContradicted, []ReportSource{official}, 15},
	} {
		rc := judgedClaim(tc.verdict, "$59 per month", 0, tc.sources...)
		assess(rc, nil)
		if rc.FreshnessScore == nil || *rc.FreshnessScore != tc.score {
			t.Errorf("%s with %d sources: score %v (%s)", tc.verdict, len(tc.sources), rc.FreshnessScore, rc.Strength)
		}
	}
	rc := judgedClaim(VerdictCurrent, "$59", 1, unrelated, news)
	assess(rc, nil)
	if rc.Strength != StrengthWeak || *rc.FreshnessScore != 60 {
		t.Errorf("weak current: %s %v", rc.Strength, rc.FreshnessScore)
	}
	rc = &ReportClaim{Verdict: VerdictConflicting}
	assess(rc, nil)
	if *rc.FreshnessScore != 45 {
		t.Errorf("conflicting score %v", rc.FreshnessScore)
	}
	for _, v := range []string{VerdictUnverifiable, VerdictNotChecked, ""} {
		rc = &ReportClaim{Verdict: v}
		assess(rc, nil)
		if rc.FreshnessScore != nil || rc.Strength != "" {
			t.Errorf("%q scored %v %s", v, rc.FreshnessScore, rc.Strength)
		}
	}
}

func TestRegistrableDomainAndSubject(t *testing.T) {
	for u, want := range map[string]string{
		"https://docs.acme.example/x":    "acme.example",
		"https://www.bbc.co.uk/news":     "bbc.co.uk",
		"https://go.dev/doc/devel":       "go.dev",
		"https://user.github.io/project": "user.github.io", // github.io is a public suffix
		"https://localhost:8080/x":       "localhost",
		"not a url":                      "not a url",
	} {
		if got := registrableDomain(u); got != want {
			t.Errorf("registrableDomain(%s) = %s, want %s", u, got, want)
		}
	}
	for _, tc := range []struct {
		domain, subject string
		want            bool
	}{
		{"acme.example", "Acme Cloud Pro", true},
		{"go.dev", "Go", true},
		{"openai.com", "OpenAI", true},
		{"go.dev", "Google Go compiler", false}, // two letters is too weak a match
		{"news.example", "Acme Cloud", false},
	} {
		if got := namedAfter(tc.domain, tc.subject); got != tc.want {
			t.Errorf("namedAfter(%s, %s) = %v", tc.domain, tc.subject, got)
		}
	}
}
