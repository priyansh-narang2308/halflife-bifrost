package halflife

import (
	"net/url"
	"regexp"
	"strings"
)

// Retrieval policies: the Tavily Search parameters for a claim, chosen by its
// category and volatility band (audit §3.4, points 1-3). The query names the
// subject and attribute, never the asserted value, so the search looks for
// the current value instead of confirming the answer's.

const (
	searchChunksPerSource  = 3
	extractChunksPerSource = 3
	maxExtractedBytes      = 1500
)

// liveFetchHours is max_age_hours per volatility band: how old a cached page
// copy Tavily may serve. Fast-changing facts need a live fetch (0); slow ones
// keep Tavily's default.
var liveFetchHours = map[string]int{
	BandHoursDays:   0,
	BandWeeksMonths: 24,
	BandYear:        168,
}

// retrievalPolicy builds the search for one claim. domains are the operator's
// preferred domains for its category, boosted with include_domains_mode
// "prefer" so a missing official page never empties the results.
func retrievalPolicy(c *ReportClaim, maxResults int, domains []string) SearchRequest {
	req := SearchRequest{
		Query: claimQuery(c), SearchDepth: "basic", MaxResults: maxResults,
		ChunksPerSource: searchChunksPerSource, IncludePublishedDate: true,
	}
	switch c.Category {
	case CategoryVersion:
		// Pin the product: results must name it.
		req.Query = `"` + strings.ReplaceAll(c.Subject, `"`, "") + `" ` + c.Attribute
		req.ExactMatch = true
	case CategoryLeadership:
		// Appointments are news; published dates come with topic news.
		req.Topic, req.TimeRange = "news", "year"
	case CategoryEvent:
		req.Topic = "news"
	}
	if len(domains) > 0 {
		req.IncludeDomains, req.IncludeDomainsMode = append([]string(nil), domains...), "prefer"
	}
	if h, ok := liveFetchHours[c.VolatilityBand]; ok {
		req.MaxAgeHours = &h
	}
	return req
}

// claimQuery is the subject and attribute: "Acme Cloud Pro monthly price".
func claimQuery(c *ReportClaim) string {
	q := strings.TrimSpace(c.Subject + " " + c.Attribute)
	if q == "" {
		q = c.Text
	}
	return buildQuery(q, "")
}

var (
	versionShape = regexp.MustCompile(`\b\d+\.\d+`)
	priceShape   = regexp.MustCompile(`(?i)[$€£¥]\s?\d|\d\s?(usd|eur|gbp|dollars?|euros?)\b|\bfree\b`)
	numberShape  = regexp.MustCompile(`\d`)
	wordRe       = regexp.MustCompile(`[\pL\pN]{3,}`)
)

// thinEvidence reports whether no source speaks to the claim: none mentions
// the subject, or, for a valued claim, none that does holds a value of the
// right shape (a version number, a price, a number). Then Tavily Extract on
// the best page may find what the snippets missed.
func thinEvidence(c *ReportClaim, sources []ReportSource) bool {
	var shape *regexp.Regexp
	switch c.Category {
	case CategoryVersion:
		shape = versionShape
	case CategoryPrice:
		shape = priceShape
	case CategoryStatistic, CategoryEvent:
		shape = numberShape
	default:
		if numberShape.MatchString(c.AssertedValue) { // "100 GB of storage"
			shape = numberShape
		}
	}
	words := wordRe.FindAllString(strings.ToLower(c.Subject), -1)
	for _, s := range sources {
		text := strings.ToLower(s.Title + " " + s.Snippet)
		if !mentionsAny(text, words) {
			continue
		}
		if shape == nil || shape.MatchString(s.Snippet) {
			return false
		}
	}
	return true
}

func mentionsAny(text string, words []string) bool {
	if len(words) == 0 {
		return true
	}
	for _, w := range words {
		if strings.Contains(text, w) {
			return true
		}
	}
	return false
}

// extractTarget picks the page to extract: the first source on a preferred
// domain, else the top source. -1 when there are no sources.
func extractTarget(sources []ReportSource, domains []string) int {
	for i, s := range sources {
		if onDomain(s.URL, domains) {
			return i
		}
	}
	if len(sources) == 0 {
		return -1
	}
	return 0
}

// onDomain reports whether rawURL's host is one of domains or below one.
func onDomain(rawURL string, domains []string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	host := strings.ToLower(u.Hostname())
	for _, d := range domains {
		d = strings.ToLower(strings.TrimPrefix(d, "www."))
		if host == d || strings.HasSuffix(host, "."+d) || host == "www."+d {
			return true
		}
	}
	return false
}
