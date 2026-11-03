package halflife

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Verification priority: whether a claim is worth paying to check, from
// deterministic rules (audit §3.3, step 2). It is not a probability of being
// stale. A claim's volatility band says how soon facts of its kind change;
// the time elapsed since the answer's information was current says how much
// of that has passed.

// Priorities, highest first.
const (
	PriorityHigh   = "HIGH"
	PriorityMedium = "MEDIUM"
	PriorityLow    = "LOW"
	PrioritySkip   = "SKIP"
)

// DefaultCutoffAge is how old an answer's information is assumed to be when
// its model's knowledge cutoff is unknown: conservative, because vendors
// publish cutoffs inconsistently and effective knowledge lags them.
const DefaultCutoffAge = 18 * 30 * 24 * time.Hour

// bandChange is the shortest time in which facts of each band typically
// change: the lower bound of the RecencyQA classes the band collapses.
var bandChange = map[string]time.Duration{
	BandHoursDays:   time.Hour,
	BandWeeksMonths: 7 * 24 * time.Hour,
	BandYear:        180 * 24 * time.Hour,
	BandYears:       2 * 365 * 24 * time.Hour,
}

var priorityRank = map[string]int{PriorityHigh: 3, PriorityMedium: 2, PriorityLow: 1, PrioritySkip: 0}

// cutoffTable maps the model that answered to its knowledge cutoff. Keys are
// "provider/model" or "model", optionally ending in "*" to match a prefix;
// the longest match wins.
type cutoffTable map[string]time.Time

func newCutoffTable(cfg map[string]string) (cutoffTable, error) {
	t := make(cutoffTable, len(cfg))
	for model, date := range cfg {
		at, ok := asOfTime(strings.TrimSpace(date))
		if !ok || strings.TrimSpace(model) == "" {
			return nil, fmt.Errorf("halflife: model_cutoffs[%q] must be %s, got %q", model, asOfLayoutDescription, date)
		}
		t[strings.TrimSpace(model)] = at
	}
	return t, nil
}

// lookup returns the cutoff for a model, trying "provider/model" first.
func (t cutoffTable) lookup(provider, model string) (time.Time, string, bool) {
	best, bestKey := time.Time{}, ""
	for _, name := range []string{provider + "/" + model, model} {
		for key, at := range t {
			prefix, wild := strings.CutSuffix(key, "*")
			if (key == name || (wild && strings.HasPrefix(name, prefix))) && len(key) > len(bestKey) {
				best, bestKey = at, key
			}
		}
		if bestKey != "" {
			return best, bestKey, true
		}
	}
	return time.Time{}, "", false
}

// priorityInput is what the rules need to know about a claim.
type priorityInput struct {
	Scope    string
	Band     string
	Category string
	AsOf     string
	Span     string
	Provider string // the model that answered
	Model    string
}

// prioritise returns a claim's priority and the rule that set it.
func prioritise(in priorityInput, cutoffs cutoffTable, boosted map[string]bool, now time.Time) (string, string) {
	switch {
	case in.Scope == ScopeHistorical:
		return PrioritySkip, "historical: not a freshness question"
	case in.Scope == ScopeTimeless:
		return PrioritySkip, "timeless"
	case in.Scope == ScopeFuture:
		return PrioritySkip, "about the future: nothing to be stale yet"
	case in.Band == BandNever:
		return PrioritySkip, "never changes"
	}
	change, ok := bandChange[in.Band]
	if !ok {
		change = bandChange[BandYear] // an unknown band is treated as middling
	}

	ref, source := now.Add(-DefaultCutoffAge), "default 18-month cutoff age"
	if at, ok := asOfTime(in.AsOf); ok {
		ref, source = at, "as_of "+in.AsOf
	} else if at, key, ok := cutoffs.lookup(in.Provider, in.Model); ok {
		ref, source = at, "cutoff of "+key
	}
	elapsed := now.Sub(ref)

	var prio, reason string
	switch {
	case presentCue.MatchString(in.Span):
		prio, reason = PriorityHigh, "says latest/current/now"
	case elapsed >= change:
		prio, reason = PriorityHigh, fmt.Sprintf("%s since %s ≥ %s band change", days(elapsed), source, in.Band)
	case elapsed >= change/2:
		prio, reason = PriorityMedium, fmt.Sprintf("%s since %s, within 2× of %s band change", days(elapsed), source, in.Band)
	default:
		prio, reason = PriorityLow, fmt.Sprintf("%s since %s < half of %s band change", days(elapsed), source, in.Band)
	}
	if boosted[in.Category] && prio != PriorityHigh {
		prio = map[string]string{PriorityLow: PriorityMedium, PriorityMedium: PriorityHigh}[prio]
		reason += "; raised one level for " + in.Category
	}
	return prio, reason
}

func days(d time.Duration) string {
	if d < 48*time.Hour {
		return fmt.Sprintf("%.0fh", d.Hours())
	}
	return fmt.Sprintf("%.0fd", d.Hours()/24)
}

// rankForChecking orders claims to check: highest priority first, then
// boosted categories, then answer order. SKIP claims are left out.
func rankForChecking(claims []*ReportClaim, boosted map[string]bool) []*ReportClaim {
	var out []*ReportClaim
	for _, c := range claims {
		if c.Priority != PrioritySkip && c.Status == ClaimPending {
			out = append(out, c)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if ri, rj := priorityRank[out[i].Priority], priorityRank[out[j].Priority]; ri != rj {
			return ri > rj
		}
		return boosted[out[i].Category] && !boosted[out[j].Category]
	})
	return out
}
