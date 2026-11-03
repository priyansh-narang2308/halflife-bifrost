package halflife

import (
	"fmt"
	"net/url"
	"strings"

	"golang.org/x/net/publicsuffix"
)

// Evidence strength and the freshness score (audit §3.5). Strength says how
// much stands behind a verdict, from fixed rules; the score is a fixed table
// over verdict and strength. The score is a heuristic for display and
// ranking, never a probability: its buckets are checked for monotonicity on
// held-out data before anything is claimed about them (Milestone 5).

// Evidence strengths.
const (
	StrengthStrong   = "STRONG"
	StrengthModerate = "MODERATE"
	StrengthWeak     = "WEAK"
)

// freshnessScores is the audit's table. CONFLICTING scores 45 at any
// strength; UNVERIFIABLE and NOT_CHECKED have no score.
var freshnessScores = map[string]map[string]int{
	VerdictCurrent:      {StrengthStrong: 90, StrengthModerate: 75, StrengthWeak: 60},
	VerdictOutdated:     {StrengthStrong: 5, StrengthModerate: 15, StrengthWeak: 30},
	VerdictContradicted: {StrengthStrong: 5, StrengthModerate: 15, StrengthWeak: 30},
}

const conflictingScore = 45

// assess sets a judged claim's strength and score. preferred are the
// operator's preferred domains for the claim's category.
func assess(rc *ReportClaim, preferred []string) {
	switch rc.Verdict {
	case VerdictConflicting:
		n := conflictingScore
		rc.Strength, rc.StrengthReason, rc.FreshnessScore = StrengthWeak, "sources disagree", &n
		return
	case VerdictCurrent, VerdictOutdated, VerdictContradicted:
	default:
		return
	}
	rc.Strength, rc.StrengthReason = evidenceStrength(rc, preferred)
	n := freshnessScores[rc.Verdict][rc.Strength]
	rc.FreshnessScore = &n
}

// evidenceStrength applies the rules:
//
//	STRONG    ≥2 independent registrable domains agree, at least one primary, quote verified
//	MODERATE  one primary source agrees, or ≥2 non-primary domains agree
//	WEAK      anything else
//
// A source agrees when it states the observed value (or, with none, the
// quote). A source is primary when it is on a preferred domain for the
// category, or its domain is named after the claim's subject.
func evidenceStrength(rc *ReportClaim, preferred []string) (string, string) {
	if rc.Evidence == nil || rc.Quote == nil {
		return StrengthWeak, "no verified quote"
	}
	kind := comparatorFor(rc.Category)
	agreeing, primary := map[string]bool{}, map[string]bool{}
	for i, s := range rc.Evidence.Sources {
		text := s.Snippet + "\n" + s.Extracted
		agrees := i == rc.Quote.Source && rc.Quote.Verified
		if !agrees && rc.ObservedValue != "" {
			agrees = statesValue(kind, rc.ObservedValue, text)
		}
		if !agrees && rc.Quote.Text != "" {
			agrees = containsFold(text, rc.Quote.Text)
		}
		if !agrees {
			continue
		}
		d := registrableDomain(s.URL)
		agreeing[d] = true
		if onDomain(s.URL, preferred) || namedAfter(d, rc.Subject) {
			primary[d] = true
		}
	}
	summary := fmt.Sprintf("%d agreeing domain(s), %d primary", len(agreeing), len(primary))
	switch {
	case len(agreeing) >= 2 && len(primary) >= 1 && rc.Quote.Verified:
		return StrengthStrong, summary
	case len(primary) >= 1 || len(agreeing) >= 2:
		return StrengthModerate, summary
	}
	return StrengthWeak, summary
}

// registrableDomain is the URL's domain one label below its public suffix:
// docs.acme.example and acme.example are one source.
func registrableDomain(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || u.Hostname() == "" {
		return rawURL
	}
	host := strings.ToLower(u.Hostname())
	if d, err := publicsuffix.EffectiveTLDPlusOne(host); err == nil {
		return d
	}
	return host
}

// namedAfter reports whether a registrable domain is named after the subject:
// its first label is a word of the subject ("acme.example" for "Acme Cloud
// Pro"), or the whole subject without spaces ("go.dev" for "Go").
func namedAfter(domain, subject string) bool {
	label, _, _ := strings.Cut(domain, ".")
	if label == "" {
		return false
	}
	words := strings.Fields(strings.ToLower(nameSplit.ReplaceAllString(subject, " ")))
	if label == strings.Join(words, "") {
		return true
	}
	for _, w := range words {
		if len(w) >= 3 && w == label {
			return true
		}
	}
	return false
}
