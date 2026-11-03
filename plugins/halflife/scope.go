package halflife

import (
	"regexp"
	"strconv"
	"time"
)

// Temporal scope: whether a claim asserts the present (and so can go stale)
// or is about the past, the future or nothing time-bound. The extractor
// labels each claim; lexical cues in its span then confirm or overrule the
// label, because freshness only applies to the present: "As of 2023, X was
// CEO" stays true after X leaves (audit §3.1, point 4).

var (
	// A past-tense verb, or a marker that the fact no longer holds.
	pastCue   = regexp.MustCompile(`(?i)\b(used to|formerly|previously|was|were|had been|became|founded|launched|released|introduced|acquired|retired|resigned|stepped down)\b`)
	formerCue = regexp.MustCompile(`(?i)\b(used to|formerly|previously|at the time|back then)\b`)
	sinceCue  = regexp.MustCompile(`(?i)\bsince\b`)
	// A year in the span: 1900-2099.
	yearCue   = regexp.MustCompile(`\b(19|20)\d{2}\b`)
	futureCue = regexp.MustCompile(`(?i)\b(will|is (going|expected|scheduled|set|planned) to|are (going|expected|scheduled|set|planned) to|plans? to|upcoming|next (year|month|week|quarter))\b`)
	// "latest", "current", "now" and the like assert the present.
	presentCue = regexp.MustCompile(`(?i)\b(latest|current|currently|now|today|newest|at the moment|as of today|this (year|month|week)|still)\b`)
)

// Scope reasons recorded on the report.
const (
	ScopeReasonModel          = "extractor's label"
	ScopeReasonDated          = "tied to a past date (as_of)"
	ScopeReasonPastCue        = "past-tense or 'formerly' wording"
	ScopeReasonPresentCue     = "says latest/current/now"
	ScopeReasonFutureCue      = "future wording"
	ScopeReasonUnknownLabel   = "no usable label"
	ScopeReasonHistoricalNoAs = "labelled historical, reads as current"
)

// classifyScope returns the claim's temporal scope and why.
func classifyScope(c extractedClaim, now time.Time) (string, string) {
	span := c.Span
	present, future := presentCue.MatchString(span), futureCue.MatchString(span)
	// Past tense alone is weak ("X was named CEO in March" can still hold);
	// with a year, or "formerly"/"used to", it places the claim in the past.
	past := pastCue.MatchString(span) && (yearCue.MatchString(span) || formerCue.MatchString(span))
	year := asOfYear(c.AsOf)
	// "Since 2019, X has led Y" is tied to a date but asserts the present.
	dated := year > 0 && year < now.Year() && !sinceCue.MatchString(span)

	label := c.TemporalScope
	switch {
	case dated && !present:
		// "In 2019 the plan cost $10" and "As of 2023, X was CEO" are
		// statements about that date, true or false for good.
		return ScopeHistorical, ScopeReasonDated
	case label == ScopeHistorical && present && !past:
		return ScopeUnclear, ScopeReasonHistoricalNoAs
	case (label == ScopeCurrent || label == ScopeUnclear) && past && !present:
		return ScopeHistorical, ScopeReasonPastCue
	case (label == ScopeTimeless || label == ScopeUnclear) && present:
		// "The latest Go release is 1.25" cannot be timeless.
		return ScopeCurrent, ScopeReasonPresentCue
	case label == ScopeUnclear && future && !present:
		return ScopeFuture, ScopeReasonFutureCue
	}
	for _, s := range temporalScopes {
		if label == s {
			return label, ScopeReasonModel
		}
	}
	return ScopeUnclear, ScopeReasonUnknownLabel
}

// asOfYear returns the year of an as_of value ("2024", "2024-05",
// "2024-05-01"), or 0.
func asOfYear(asOf string) int {
	if len(asOf) < 4 {
		return 0
	}
	y, err := strconv.Atoi(asOf[:4])
	if err != nil {
		return 0
	}
	return y
}

// asOfTime returns the start of the period an as_of value names, or false.
func asOfTime(asOf string) (time.Time, bool) {
	for _, layout := range []string{"2006-01-02", "2006-01", "2006"} {
		if t, err := time.Parse(layout, asOf); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}
