package halflife

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	bifrost "github.com/maximhq/bifrost/core"
)

// fixedNow is the clock every claim test runs at.
var fixedNow = time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)

// claimFixture is one testdata/claims file: an answer, what the extractor
// returns for it, and what each extracted claim should end up as.
type claimFixture struct {
	Question   string          `json:"question"`
	Answer     string          `json:"answer"`
	Extraction json.RawMessage `json:"extraction"`
	Want       []struct {
		Status      string `json:"status"`
		Scope       string `json:"scope"`
		Priority    string `json:"priority"`
		Reason      string `json:"reason"`
		ScopeReason string `json:"scope_reason"`
	} `json:"want"`
}

// extractingVerifier is a verifier whose LLM is a fake gateway answering
// with replies, and whose searches go to a fake searcher.
func extractingVerifier(t *testing.T, mod func(*Config), replies ...gwReply) (*verifier, *fakeSearcher, *fakeGateway) {
	t.Helper()
	g := newFakeGateway(t, replies...)
	cfg := Config{Verifiers: []VerifierKey{acmeKey}, VerifierModels: []string{"openrouter/nvidia/nemotron:free"}}
	if mod != nil {
		mod(&cfg)
	}
	cfg = cfg.withDefaults()
	if err := cfg.validate(); err != nil {
		t.Fatal(err)
	}
	v, err := newVerifier(cfg, newReportStore(10), bifrost.NewNoOpLogger())
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeSearcher{}
	v.newClient = f.client
	v.sleep = func(context.Context, time.Duration) error { return nil }
	v.now = func() time.Time { return fixedNow }
	v.newLLM = func(key verifierKey) *LLM {
		l := NewLLM(g.srv.URL+"/", key.value, LLMOptions{Models: cfg.VerifierModels, MaxTokens: 512}, nil)
		l.sleep = func(context.Context, time.Duration) error { return nil }
		return l
	}
	return v, f, g
}

func judgeReply(j judgement) gwReply {
	b, _ := json.Marshal(j)
	return gwReply{content: string(b)}
}

func extractionReply(t *testing.T, claims ...extractedClaim) gwReply {
	t.Helper()
	if claims == nil {
		claims = []extractedClaim{}
	}
	b, err := json.Marshal(extraction{Claims: claims})
	if err != nil {
		t.Fatal(err)
	}
	return gwReply{content: string(b)}
}

func TestClaimFixtures(t *testing.T) {
	files, err := filepath.Glob("testdata/claims/*.json")
	if err != nil || len(files) == 0 {
		t.Fatalf("no claim fixtures: %v", err)
	}
	for _, file := range files {
		t.Run(filepath.Base(file), func(t *testing.T) {
			raw, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			var fx claimFixture
			if err := json.Unmarshal(raw, &fx); err != nil {
				t.Fatal(err)
			}
			v, _, _ := extractingVerifier(t, nil, gwReply{content: string(fx.Extraction)})
			c := acmeCapture("fx", fx.Answer)
			c.Question = fx.Question
			r, err := runVerify(t, v, c)
			if err != nil {
				t.Fatal(err)
			}
			if len(r.Claims) != len(fx.Want) {
				t.Fatalf("got %d claims, want %d: %+v", len(r.Claims), len(fx.Want), r.Claims)
			}
			for i, w := range fx.Want {
				got := r.Claims[i]
				if got.Status != w.Status ||
					(w.Scope != "" && got.TemporalScope != w.Scope) ||
					(w.Priority != "" && got.Priority != w.Priority) ||
					!strings.Contains(got.StatusReason, w.Reason) ||
					(w.ScopeReason != "" && got.ScopeReason != w.ScopeReason) {
					t.Errorf("claim %d %q: got status=%s scope=%s (%s) priority=%s (%s) reason=%q; want %+v",
						i, got.Text, got.Status, got.TemporalScope, got.ScopeReason, got.Priority, got.PriorityReason, got.StatusReason, w)
				}
			}
			if r.Extraction == nil || r.Extraction.Model == "" {
				t.Errorf("extraction call not recorded: %+v", r.Extraction)
			}
		})
	}
}

func TestCheckableSentences(t *testing.T) {
	answer := "Here is what I found:\nAcme Cloud Pro costs $49 per month.\nYou should compare plans first. Is it worth it?\nI recommend the annual plan."
	got := checkableSentences(answer)
	if len(got) != 1 || got[0] != "Acme Cloud Pro costs $49 per month." {
		t.Errorf("got %q", got)
	}
}

func TestExtractClaimsSkipsModelWhenNothingCheckable(t *testing.T) {
	g := newFakeGateway(t, gwReply{content: `{"claims":[]}`})
	l, _ := testLLM(g)
	claims, res, err := extractClaims(context.Background(), l, "q", "You should try it. Is it good?")
	if err != nil || claims != nil || res != nil || g.calls() != 0 {
		t.Errorf("claims=%v res=%v err=%v calls=%d", claims, res, err, g.calls())
	}
}

func TestExtractClaimsCapsAndTrims(t *testing.T) {
	var many []extractedClaim
	for range maxExtractedClaims + 3 {
		many = append(many, extractedClaim{Text: "  Acme Cloud Pro costs $49.  ", Span: "Acme Cloud Pro costs $49", Subject: "Acme Cloud Pro",
			Category: CategoryPrice, TemporalScope: ScopeCurrent, VolatilityBand: BandWeeksMonths})
	}
	g := newFakeGateway(t, extractionReply(t, many...))
	l, _ := testLLM(g)
	claims, _, err := extractClaims(context.Background(), l, "q", "Acme Cloud Pro costs $49 per month.")
	if err != nil {
		t.Fatal(err)
	}
	if len(claims) != maxExtractedClaims || claims[0].Text != "Acme Cloud Pro costs $49." {
		t.Errorf("got %d claims, first %q", len(claims), claims[0].Text)
	}
	// The prompt carries the filtered answer inside <answer> tags, and the
	// strict schema.
	body := g.bodies[0]
	msgs, _ := body["messages"].([]any)
	user, _ := msgs[len(msgs)-1].(map[string]any)["content"].(string)
	if !strings.Contains(user, "<answer>\nAcme Cloud Pro costs $49 per month.\n</answer>") {
		t.Errorf("user prompt %q", user)
	}
	rf, _ := body["response_format"].(map[string]any)
	if js, _ := rf["json_schema"].(map[string]any); js["name"] != extractionSchemaName || js["strict"] != true {
		t.Errorf("response_format %v", rf)
	}
}

func TestCheckSpan(t *testing.T) {
	answer := "**Acme Cloud Pro** costs $1,049 per year — and it's not free. Updated “weekly”."
	ok := extractedClaim{Text: "Acme Cloud Pro costs $1,049 per year.", Span: "Acme Cloud Pro costs $1,049 per year", Subject: "Acme Cloud Pro", AssertedValue: "$1,049"}
	cases := []struct {
		name string
		edit func(*extractedClaim)
		want string
	}{
		{"faithful", func(*extractedClaim) {}, ""},
		{"markdown and quotes ignored", func(c *extractedClaim) {
			c.Span, c.Text, c.AssertedValue = `Updated "weekly"`, "Acme Cloud Pro is updated weekly.", "weekly"
		}, ""},
		{"thousands separator ignored", func(c *extractedClaim) { c.AssertedValue = "1049 dollars" }, ""},
		{"paraphrased span", func(c *extractedClaim) { c.Span = "Acme Cloud Pro is $1,049 a year" }, RejectSpanNotInAnswer},
		{"empty span", func(c *extractedClaim) { c.Span = "" }, RejectSpanNotInAnswer},
		{"no subject", func(c *extractedClaim) { c.Subject = "" }, RejectNoSubject},
		{"too short", func(c *extractedClaim) { c.Text = "Costs $1,049." }, RejectTooShort},
		{"bad as_of", func(c *extractedClaim) { c.AsOf = "last year" }, RejectBadAsOf},
		{"bad as_of month", func(c *extractedClaim) { c.AsOf = "2026-13" }, RejectBadAsOf},
		{"value number drifted", func(c *extractedClaim) { c.AssertedValue = "$999" }, RejectValueNotInSpan},
		{"value word drifted", func(c *extractedClaim) { c.AssertedValue = "monthly" }, RejectValueNotInSpan},
		{"text adds a number", func(c *extractedClaim) { c.Text = "Acme Cloud Pro costs $1,049 per year in 2026." }, RejectNewNumber},
		{"text drops negation", func(c *extractedClaim) {
			c.Span, c.Text, c.AssertedValue = "it's not free", "Acme Cloud Pro is free.", ""
		}, RejectNegationFlipped},
		{"text adds negation", func(c *extractedClaim) { c.Text = "Acme Cloud Pro no longer costs $1,049 per year." }, RejectNegationFlipped},
	}
	for _, tc := range cases {
		c := ok
		tc.edit(&c)
		if got := checkSpan(c, answer); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestClassifyScope(t *testing.T) {
	cases := []struct {
		span, label, asOf string
		want, reason      string
	}{
		{"Acme Cloud Pro costs $49 per month", ScopeCurrent, "", ScopeCurrent, ScopeReasonModel},
		{"In 2019 the plan cost $10", ScopeCurrent, "2019", ScopeHistorical, ScopeReasonDated},
		{"As of 2023, Jordan Lee was CEO", ScopeCurrent, "2023-05", ScopeHistorical, ScopeReasonDated},
		{"As of 2026, the plan costs $10", ScopeCurrent, "2026", ScopeCurrent, ScopeReasonModel},
		{"Since 2019, Dana Cruz has led Initech", ScopeCurrent, "2019", ScopeCurrent, ScopeReasonModel},
		{"As of 2023 Dana Cruz is still CEO", ScopeCurrent, "2023", ScopeCurrent, ScopeReasonModel},
		{"Jordan Lee was formerly CTO", ScopeCurrent, "", ScopeHistorical, ScopeReasonPastCue},
		{"Go 1.21 was released in 2023", ScopeUnclear, "", ScopeHistorical, ScopeReasonPastCue},
		{"Jordan Lee was named CEO", ScopeCurrent, "", ScopeCurrent, ScopeReasonModel}, // past tense alone is weak
		{"The current CEO is Dana Cruz", ScopeHistorical, "", ScopeUnclear, ScopeReasonHistoricalNoAs},
		{"The latest Go release is 1.25", ScopeTimeless, "", ScopeCurrent, ScopeReasonPresentCue},
		{"Initech will launch version 4", ScopeUnclear, "", ScopeFuture, ScopeReasonFutureCue},
		{"A terabyte is 1,000 gigabytes", ScopeTimeless, "", ScopeTimeless, ScopeReasonModel},
		{"Acme sells storage", "SOMETIME", "", ScopeUnclear, ScopeReasonUnknownLabel},
	}
	for _, tc := range cases {
		got, reason := classifyScope(extractedClaim{Span: tc.span, TemporalScope: tc.label, AsOf: tc.asOf}, fixedNow)
		if got != tc.want || reason != tc.reason {
			t.Errorf("%q (%s, as_of %q): got %s (%s), want %s (%s)", tc.span, tc.label, tc.asOf, got, reason, tc.want, tc.reason)
		}
	}
}

func TestAsOfParsing(t *testing.T) {
	for in, want := range map[string]int{"2024": 2024, "2024-05": 2024, "2024-05-01": 2024, "": 0, "soon": 0} {
		if got := asOfYear(in); got != want {
			t.Errorf("asOfYear(%q) = %d, want %d", in, got, want)
		}
	}
	if at, ok := asOfTime("2024-05"); !ok || !at.Equal(time.Date(2024, 5, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("asOfTime(2024-05) = %v %v", at, ok)
	}
	if _, ok := asOfTime("May 2024"); ok {
		t.Error("asOfTime accepted a free-form date")
	}
}

func TestPrioritise(t *testing.T) {
	cutoffs, err := newCutoffTable(map[string]string{"openai/gpt-x*": "2026-09", "old-model": "2020"})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name    string
		in      priorityInput
		boosted map[string]bool
		want    string
		reason  string
	}{
		{"historical", priorityInput{Scope: ScopeHistorical, Band: BandWeeksMonths}, nil, PrioritySkip, "historical"},
		{"timeless", priorityInput{Scope: ScopeTimeless, Band: BandWeeksMonths}, nil, PrioritySkip, "timeless"},
		{"future", priorityInput{Scope: ScopeFuture, Band: BandWeeksMonths}, nil, PrioritySkip, "future"},
		{"never", priorityInput{Scope: ScopeCurrent, Band: BandNever}, nil, PrioritySkip, "never"},
		{"default cutoff age", priorityInput{Scope: ScopeCurrent, Band: BandYears}, nil, PriorityMedium, "default 18-month cutoff age"},
		{"default cutoff, fast band", priorityInput{Scope: ScopeCurrent, Band: BandWeeksMonths}, nil, PriorityHigh, "default 18-month"},
		{"says latest", priorityInput{Scope: ScopeCurrent, Band: BandYears, Span: "the latest version", AsOf: "2026-11"}, nil, PriorityHigh, "latest"},
		{"recent as_of, slow band", priorityInput{Scope: ScopeCurrent, Band: BandYears, AsOf: "2026-10"}, nil, PriorityLow, "as_of 2026-10"},
		{"as_of within 2x", priorityInput{Scope: ScopeCurrent, Band: BandYear, AsOf: "2026-07"}, nil, PriorityMedium, "within 2×"},
		{"cutoff prefix match", priorityInput{Scope: ScopeCurrent, Band: BandYear, Provider: "openai", Model: "gpt-x-mini"}, nil, PriorityLow, "cutoff of openai/gpt-x*"},
		{"cutoff bare model", priorityInput{Scope: ScopeCurrent, Band: BandYear, Provider: "anthropic", Model: "old-model"}, nil, PriorityHigh, "cutoff of old-model"},
		{"as_of beats cutoff", priorityInput{Scope: ScopeCurrent, Band: BandYear, AsOf: "2019", Provider: "openai", Model: "gpt-x"}, nil, PriorityHigh, "as_of 2019"},
		{"boost low", priorityInput{Scope: ScopeCurrent, Band: BandYears, AsOf: "2026-10", Category: CategoryLaw}, map[string]bool{CategoryLaw: true}, PriorityMedium, "raised one level for law"},
		{"boost never past high", priorityInput{Scope: ScopeCurrent, Band: BandWeeksMonths, Category: CategoryLaw}, map[string]bool{CategoryLaw: true}, PriorityHigh, ""},
		{"boost not for skip", priorityInput{Scope: ScopeTimeless, Category: CategoryLaw}, map[string]bool{CategoryLaw: true}, PrioritySkip, ""},
		{"hours band, a day old", priorityInput{Scope: ScopeCurrent, Band: BandHoursDays, AsOf: "2026-11-02"}, nil, PriorityHigh, "36h"},
		{"unknown band is middling", priorityInput{Scope: ScopeUnclear, Band: "WHENEVER", AsOf: "2026-10"}, nil, PriorityLow, ""},
	}
	for _, tc := range cases {
		got, reason := prioritise(tc.in, cutoffs, tc.boosted, fixedNow)
		if got != tc.want || !strings.Contains(reason, tc.reason) {
			t.Errorf("%s: got %s (%s), want %s (…%s…)", tc.name, got, reason, tc.want, tc.reason)
		}
	}
}

func TestCutoffTable(t *testing.T) {
	cutoffs, err := newCutoffTable(map[string]string{"gpt-*": "2024", "gpt-5*": "2025-06", "openai/gpt-5-mini": "2025-01-15"})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ provider, model, key string }{
		{"openai", "gpt-5-mini", "openai/gpt-5-mini"}, // exact provider/model wins
		{"azure", "gpt-5-mini", "gpt-5*"},             // longest prefix
		{"azure", "gpt-4o", "gpt-*"},
		{"anthropic", "claude", ""},
	} {
		_, key, ok := cutoffs.lookup(tc.provider, tc.model)
		if key != tc.key || ok != (tc.key != "") {
			t.Errorf("lookup(%s, %s) = %q %v, want %q", tc.provider, tc.model, key, ok, tc.key)
		}
	}
	for _, bad := range []map[string]string{{"gpt": "June 2024"}, {" ": "2024"}} {
		if _, err := newCutoffTable(bad); err == nil {
			t.Errorf("accepted %v", bad)
		}
	}
}

func TestRankForChecking(t *testing.T) {
	claims := []*ReportClaim{
		{Text: "a", Priority: PriorityLow, Status: ClaimPending},
		{Text: "b", Priority: PriorityHigh, Status: ClaimPending, Category: CategoryPrice},
		{Text: "c", Priority: PrioritySkip, Status: ClaimPending},
		{Text: "d", Priority: PriorityHigh, Status: ClaimPending, Category: CategoryLaw},
		{Text: "e", Priority: PriorityHigh, Status: ClaimRejected},
		{Text: "f", Priority: PriorityMedium, Status: ClaimPending},
	}
	var got []string
	for _, c := range rankForChecking(claims, map[string]bool{CategoryLaw: true}) {
		got = append(got, c.Text)
	}
	if strings.Join(got, "") != "dbfa" {
		t.Errorf("order %v, want d b f a", got)
	}
}

func TestRetrievalPolicy(t *testing.T) {
	base := ReportClaim{Subject: "Acme Cloud Pro", Attribute: "monthly price", AssertedValue: "$49", VolatilityBand: BandWeeksMonths}
	cases := []struct {
		name    string
		edit    func(*ReportClaim)
		domains []string
		check   func(SearchRequest) bool
	}{
		{"query omits the asserted value", func(c *ReportClaim) { c.Category = CategoryPrice }, nil, func(r SearchRequest) bool {
			return r.Query == "Acme Cloud Pro monthly price" && !strings.Contains(r.Query, "49") &&
				r.SearchDepth == "basic" && r.MaxResults == 5 && r.ChunksPerSource == 3 && r.IncludePublishedDate && r.Topic == ""
		}},
		{"version is exact", func(c *ReportClaim) {
			c.Category, c.Subject, c.Attribute = CategoryVersion, `Go "lang"`, "latest version"
		}, nil, func(r SearchRequest) bool {
			return r.Query == `"Go lang" latest version` && r.ExactMatch
		}},
		{"leadership is news this year", func(c *ReportClaim) { c.Category, c.VolatilityBand = CategoryLeadership, BandYears }, nil, func(r SearchRequest) bool {
			return r.Topic == "news" && r.TimeRange == "year" && r.MaxAgeHours == nil
		}},
		{"event is news", func(c *ReportClaim) { c.Category = CategoryEvent }, nil, func(r SearchRequest) bool {
			return r.Topic == "news" && r.TimeRange == ""
		}},
		{"preferred domains", func(c *ReportClaim) { c.Category = CategoryLaw }, []string{"gov.example"}, func(r SearchRequest) bool {
			return len(r.IncludeDomains) == 1 && r.IncludeDomains[0] == "gov.example" && r.IncludeDomainsMode == "prefer"
		}},
		{"no domains, no mode", func(c *ReportClaim) { c.Category = CategoryLaw }, nil, func(r SearchRequest) bool {
			return r.IncludeDomains == nil && r.IncludeDomainsMode == ""
		}},
		{"hours band fetches live", func(c *ReportClaim) { c.VolatilityBand = BandHoursDays }, nil, func(r SearchRequest) bool {
			return r.MaxAgeHours != nil && *r.MaxAgeHours == 0
		}},
		{"weeks band", func(c *ReportClaim) {}, nil, func(r SearchRequest) bool { return *r.MaxAgeHours == 24 }},
		{"year band", func(c *ReportClaim) { c.VolatilityBand = BandYear }, nil, func(r SearchRequest) bool { return *r.MaxAgeHours == 168 }},
		{"no subject falls back to text", func(c *ReportClaim) { c.Subject, c.Attribute, c.Text = "", "", "Acme Pro costs $49." }, nil, func(r SearchRequest) bool {
			return strings.Contains(r.Query, "Acme Pro")
		}},
	}
	for _, tc := range cases {
		c := base
		tc.edit(&c)
		if req := retrievalPolicy(&c, 5, tc.domains); !tc.check(req) {
			b, _ := json.Marshal(req)
			t.Errorf("%s: %s", tc.name, b)
		}
	}
}

func TestThinEvidence(t *testing.T) {
	src := func(title, snippet string) []ReportSource { return []ReportSource{{Title: title, Snippet: snippet}} }
	cases := []struct {
		name     string
		category string
		sources  []ReportSource
		want     bool
	}{
		{"no sources", CategoryPrice, nil, true},
		{"subject missing", CategoryPrice, src("Other vendor", "Pro is $59."), true},
		{"price present", CategoryPrice, src("Acme pricing", "Pro is $59 per month."), false},
		{"price absent", CategoryPrice, src("Acme pricing", "See our plans page."), true},
		{"version present", CategoryVersion, src("Acme release notes", "Acme 4.2 is out."), false},
		{"version absent", CategoryVersion, src("Acme release notes", "A new release is out."), true},
		{"other needs only the subject", CategoryOther, src("Acme", "Acme sells storage."), false},
		{"valued other needs a number", CategoryOther, src("Acme", "Acme sells storage."), true},
	}
	for _, tc := range cases {
		c := ReportClaim{Subject: "Acme Cloud", Category: tc.category}
		if strings.HasPrefix(tc.name, "valued") {
			c.AssertedValue = "100 GB"
		}
		if got := thinEvidence(&c, tc.sources); got != tc.want {
			t.Errorf("%s: got %v", tc.name, got)
		}
	}
}

func TestExtractTargetAndDomains(t *testing.T) {
	sources := []ReportSource{{URL: "https://blog.example/acme"}, {URL: "https://docs.acme.example/pricing"}}
	if i := extractTarget(sources, []string{"acme.example"}); i != 1 {
		t.Errorf("preferred domain: got %d", i)
	}
	if i := extractTarget(sources, nil); i != 0 {
		t.Errorf("no preference: got %d", i)
	}
	if i := extractTarget(nil, nil); i != -1 {
		t.Errorf("no sources: got %d", i)
	}
	for u, want := range map[string]bool{
		"https://www.acme.example/x": true, "https://acme.example": true, "https://notacme.example": false, "::bad": false,
	} {
		if got := onDomain(u, []string{"www.acme.example"}); got != want {
			t.Errorf("onDomain(%s) = %v", u, got)
		}
	}
}

// --- the extracted pipeline ---

var (
	priceClaim = extractedClaim{Text: "Acme Cloud Pro costs $49 per month.", Span: "Acme Cloud Pro costs $49 per month", Subject: "Acme Cloud Pro",
		Attribute: "monthly price", AssertedValue: "$49", Category: CategoryPrice, TemporalScope: ScopeCurrent, VolatilityBand: BandWeeksMonths}
	storageClaim = extractedClaim{Text: "Acme Cloud Pro includes 100 GB of storage.", Span: "includes 100 GB of storage", Subject: "Acme Cloud Pro",
		Attribute: "included storage", AssertedValue: "100 GB", Category: CategoryOther, TemporalScope: ScopeCurrent, VolatilityBand: BandYear}
	pricingAnswer = "Acme Cloud Pro costs $49 per month and includes 100 GB of storage."
)

// vagueReply names Acme but states no figure, so a valued claim's evidence
// is thin.
func vagueReply(int) (*SearchResponse, error) {
	return &SearchResponse{Usage: TavilyUsage{Credits: 1}, Results: []SearchResult{
		{Title: "Acme Cloud Pro plans", URL: "https://acme.example/plans", Content: "See the plans page for details."}}}, nil
}

func TestPipelineSearchesWithPolicyAndAttribution(t *testing.T) {
	v, f, g := extractingVerifier(t, nil, extractionReply(t, priceClaim, storageClaim),
		judgeReply(judgement{Verdict: judgeContradicts, CurrentValue: "$59 per month", Quote: "Pro is $59 per month.", Source: 1}),
		judgeReply(judgement{Verdict: judgeInsufficient}))
	f.reply = func(n int) (*SearchResponse, error) {
		if n == 2 {
			return vagueReply(n)
		}
		return &SearchResponse{RequestID: "tv-1", GatewayRequestID: "gw-1", Usage: TavilyUsage{Credits: 1},
			Results: []SearchResult{{Title: "Acme pricing", URL: "https://acme.example/pricing", Content: "Pro is $59 per month."}}}, nil
	}
	r, err := runVerify(t, v, acmeCapture("p1", pricingAnswer))
	if err != nil {
		t.Fatal(err)
	}
	if r.Outcome != OutcomeEvidenceFound || len(r.Claims) != 2 || len(f.calls) != 2 || g.calls() != 3 {
		t.Fatalf("outcome=%s claims=%d searches=%d llm=%d", r.Outcome, len(r.Claims), len(f.calls), g.calls())
	}
	call := f.calls[0]
	if call.key != acmeKey.VirtualKeyID || call.attr.Tenant != "vk-acme" || call.attr.Report != "p1" {
		t.Errorf("search charged %s for %+v", call.key, call.attr)
	}
	if call.req.Query != "Acme Cloud Pro monthly price" || call.req.MaxAgeHours == nil || *call.req.MaxAgeHours != 24 {
		t.Errorf("search %+v", call.req)
	}
	c := r.Claims[0]
	if c.Search == nil || c.Search.Query != c.Query || c.Evidence == nil || len(c.Evidence.Sources) != 1 {
		t.Errorf("claim 0 %+v", c)
	}
	// The price result names Acme and a price: no Extract. The storage result
	// has no figure, so its page is extracted.
	if len(f.extracts) != 1 || f.extracts[0].Query != "Acme Cloud Pro included storage" ||
		r.Claims[1].Evidence.ExtractedURL != "https://acme.example/plans" || r.Claims[1].Evidence.Sources[0].Extracted == "" {
		t.Errorf("extracts %+v, evidence %+v", f.extracts, r.Claims[1].Evidence)
	}
	if r.Claims[1].Evidence.Credits != 2 {
		t.Errorf("storage credits = %v, want search + extract", r.Claims[1].Evidence.Credits)
	}
	// The price comparator found $49 ≠ $59 with no sign $49 ever held; the
	// storage evidence said nothing. The report takes the worse verdict.
	if c := r.Claims[0]; c.Verdict != VerdictContradicted || c.Comparison == nil || c.Comparison.Kind != KindPrice || !c.Quote.Verified {
		t.Errorf("price verdict %s (%s) comparison %+v quote %+v", c.Verdict, c.VerdictReason, c.Comparison, c.Quote)
	}
	if c := r.Claims[1]; c.Verdict != VerdictUnverifiable || c.FreshnessScore != nil {
		t.Errorf("storage verdict %s (%s) score %v", c.Verdict, c.VerdictReason, c.FreshnessScore)
	}
	if r.Verdict != VerdictContradicted {
		t.Errorf("report verdict %s", r.Verdict)
	}
}

func TestPipelineDisableExtract(t *testing.T) {
	v, f, _ := extractingVerifier(t, func(c *Config) { c.DisableExtract = true }, extractionReply(t, storageClaim))
	if _, err := runVerify(t, v, acmeCapture("p2", pricingAnswer)); err != nil {
		t.Fatal(err)
	}
	if len(f.extracts) != 0 {
		t.Errorf("extracted with disable_extract: %+v", f.extracts)
	}
}

func TestPipelineMaxClaims(t *testing.T) {
	v, f, _ := extractingVerifier(t, func(c *Config) { c.MaxClaimsPerResponse = 1 }, extractionReply(t, storageClaim, priceClaim))
	r, err := runVerify(t, v, acmeCapture("p3", pricingAnswer))
	if err != nil {
		t.Fatal(err)
	}
	// Both are HIGH; answer order breaks the tie, so storage is searched.
	if len(f.calls) != 1 || r.Claims[0].Status != ClaimChecked || r.Claims[1].Status != ClaimNotChecked ||
		!strings.Contains(r.Claims[1].StatusReason, "max_claims_per_response (1)") {
		t.Errorf("searches=%d claims=%+v", len(f.calls), r.Claims)
	}
}

func TestPipelineBoostedCategoryGoesFirst(t *testing.T) {
	v, f, _ := extractingVerifier(t, func(c *Config) {
		c.MaxClaimsPerResponse, c.PriorityBoostCategories = 1, []string{CategoryPrice}
	}, extractionReply(t, storageClaim, priceClaim))
	if _, err := runVerify(t, v, acmeCapture("p4", pricingAnswer)); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 1 || f.calls[0].req.Query != "Acme Cloud Pro monthly price" {
		t.Errorf("searched %+v", f.calls)
	}
}

func TestPipelinePreferredDomains(t *testing.T) {
	v, f, _ := extractingVerifier(t, func(c *Config) {
		c.PreferredDomains = map[string][]string{CategoryOther: {"docs.acme.example"}}
	}, extractionReply(t, storageClaim))
	f.reply = func(int) (*SearchResponse, error) {
		return &SearchResponse{Usage: TavilyUsage{Credits: 1}, Results: []SearchResult{
			{Title: "Acme review", URL: "https://blog.example/acme", Content: "Acme is fine."},
			{Title: "Acme docs", URL: "https://docs.acme.example/limits", Content: "Acme limits."},
		}}, nil
	}
	if _, err := runVerify(t, v, acmeCapture("p5", pricingAnswer)); err != nil {
		t.Fatal(err)
	}
	if req := f.calls[0].req; req.IncludeDomainsMode != "prefer" || len(req.IncludeDomains) != 1 {
		t.Errorf("search %+v", req)
	}
	if len(f.extracts) != 1 || f.extracts[0].URLs[0] != "https://docs.acme.example/limits" {
		t.Errorf("extracts %+v", f.extracts)
	}
}

func TestPipelineBudgetSpentMidRun(t *testing.T) {
	v, f, _ := extractingVerifier(t, nil, extractionReply(t, priceClaim, storageClaim))
	f.reply = func(int) (*SearchResponse, error) {
		return nil, &TavilyError{Status: http.StatusPaymentRequired, Message: "budget exceeded"}
	}
	r, err := runVerify(t, v, acmeCapture("p6", pricingAnswer))
	if err != nil {
		t.Fatalf("a spent budget is not an error: %v", err)
	}
	if len(f.calls) != 1 || r.Outcome != OutcomeBudgetExhausted {
		t.Fatalf("searches=%d outcome=%s", len(f.calls), r.Outcome)
	}
	for _, c := range r.Claims {
		if c.Status != ClaimNotChecked || !strings.Contains(c.StatusReason, "402") {
			t.Errorf("claim %q: %s %q", c.Text, c.Status, c.StatusReason)
		}
	}
}

func TestPipelineExtractBudgetStopsLaterSearches(t *testing.T) {
	v, f, _ := extractingVerifier(t, nil, extractionReply(t, storageClaim, priceClaim))
	f.reply = vagueReply
	f.extractReply = func(ExtractRequest) (*ExtractResponse, error) {
		return nil, &TavilyError{Status: http.StatusPaymentRequired, Message: "budget exceeded"}
	}
	r, err := runVerify(t, v, acmeCapture("p7", pricingAnswer))
	if err != nil {
		t.Fatal(err)
	}
	// Storage was searched (evidence stands), its Extract was refused, and
	// the price claim was never searched.
	if len(f.calls) != 1 || r.Claims[0].Status != ClaimChecked || r.Claims[1].Status != ClaimNotChecked || r.Outcome != OutcomeEvidenceFound {
		t.Errorf("searches=%d outcome=%s claims=%+v", len(f.calls), r.Outcome, r.Claims)
	}
}

func TestPipelineExtractFailureKeepsEvidence(t *testing.T) {
	v, _, _ := extractingVerifier(t, nil, extractionReply(t, storageClaim))
	v.newClient = func(key verifierKey) searcher {
		f := &fakeSearcher{reply: vagueReply, extractReply: func(ExtractRequest) (*ExtractResponse, error) {
			return nil, &TavilyError{Status: http.StatusBadGateway, Message: "upstream"}
		}}
		return f.client(key)
	}
	r, err := runVerify(t, v, acmeCapture("p8", pricingAnswer))
	if err != nil {
		t.Fatal(err)
	}
	if c := r.Claims[0]; c.Status != ClaimChecked || !strings.HasPrefix(c.StatusReason, "extract failed:") || r.Outcome != OutcomeEvidenceFound {
		t.Errorf("claim %+v outcome %s", c, r.Outcome)
	}
}

func TestPipelineLiveFetchRefusedRetriesWithoutMaxAge(t *testing.T) {
	hours := extractedClaim{Text: "Acme stock trades at $120 a share.", Span: "Acme stock trades at $120 a share", Subject: "Acme stock",
		Attribute: "share price", AssertedValue: "$120", Category: CategoryPrice, TemporalScope: ScopeCurrent, VolatilityBand: BandHoursDays}
	v, f, _ := extractingVerifier(t, nil, extractionReply(t, hours))
	f.reply = func(n int) (*SearchResponse, error) {
		if n == 1 {
			return nil, &TavilyError{Status: 434, Message: "live fetch unavailable"}
		}
		return &SearchResponse{Usage: TavilyUsage{Credits: 1}, Results: []SearchResult{{Title: "Acme stock", URL: "https://q.example/acme", Content: "Acme at $118."}}}, nil
	}
	r, err := runVerify(t, v, acmeCapture("p9", "Acme stock trades at $120 a share."))
	if err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 2 || f.calls[0].req.MaxAgeHours == nil || *f.calls[0].req.MaxAgeHours != 0 || f.calls[1].req.MaxAgeHours != nil {
		t.Fatalf("searches %+v", f.calls)
	}
	if e := r.Claims[0].Evidence; !e.LiveFetchRefused || len(e.Sources) != 1 {
		t.Errorf("evidence %+v", e)
	}
}

func TestPipelineSearchFailure(t *testing.T) {
	v, f, _ := extractingVerifier(t, nil, extractionReply(t, priceClaim))
	f.reply = func(int) (*SearchResponse, error) {
		return nil, &TavilyError{Status: http.StatusBadRequest, Message: "bad query"}
	}
	r, err := runVerify(t, v, acmeCapture("p10", pricingAnswer))
	if err == nil || r.Outcome != OutcomeSearchFailed || r.Claims[0].Status != ClaimFailed || r.Error == "" {
		t.Errorf("err=%v outcome=%s claim=%+v", err, r.Outcome, r.Claims[0])
	}
}

func TestPipelineNothingToCheck(t *testing.T) {
	old := extractedClaim{Text: "Acme Cloud was founded in 2015.", Span: "Acme Cloud was founded in 2015", Subject: "Acme Cloud",
		AssertedValue: "2015", Category: CategoryEvent, TemporalScope: ScopeHistorical, AsOf: "2015", VolatilityBand: BandNever}
	v, f, _ := extractingVerifier(t, nil, extractionReply(t, old))
	r, err := runVerify(t, v, acmeCapture("p11", "Acme Cloud was founded in 2015."))
	if err != nil || r.Outcome != OutcomeNothingToCheck || len(f.calls) != 0 {
		t.Errorf("err=%v outcome=%s searches=%d", err, r.Outcome, len(f.calls))
	}
}

func TestPipelineAllRejectedIsNothingToCheck(t *testing.T) {
	drifted := priceClaim
	drifted.AssertedValue = "$45"
	v, f, _ := extractingVerifier(t, nil, extractionReply(t, drifted))
	r, err := runVerify(t, v, acmeCapture("p12", pricingAnswer))
	if err != nil || r.Outcome != OutcomeNothingToCheck || len(f.calls) != 0 || r.Claims[0].Status != ClaimRejected {
		t.Errorf("err=%v outcome=%s searches=%d claims=%+v", err, r.Outcome, len(f.calls), r.Claims)
	}
}

func TestPipelineNoClaims(t *testing.T) {
	v, f, g := extractingVerifier(t, nil, extractionReply(t))
	r, err := runVerify(t, v, acmeCapture("p13", pricingAnswer))
	if err != nil || r.Outcome != OutcomeNoClaim || len(f.calls) != 0 || g.calls() != 1 || r.Extraction == nil {
		t.Errorf("err=%v outcome=%s searches=%d llm=%d", err, r.Outcome, len(f.calls), g.calls())
	}
	// An answer with nothing checkable never reaches the model.
	r, _ = runVerify(t, v, acmeCapture("p14", "You should ask support. Is that okay?"))
	if r.Outcome != OutcomeNoClaim || g.calls() != 1 || r.Extraction != nil {
		t.Errorf("outcome=%s llm=%d", r.Outcome, g.calls())
	}
}

func TestPipelineExtractionBudgetSpent(t *testing.T) {
	v, f, _ := extractingVerifier(t, nil, gwReply{status: http.StatusPaymentRequired, raw: `{"error":{"message":"budget exceeded"}}`})
	r, err := runVerify(t, v, acmeCapture("p15", pricingAnswer))
	if err != nil || r.Outcome != OutcomeBudgetExhausted || len(f.calls) != 0 {
		t.Errorf("err=%v outcome=%s searches=%d", err, r.Outcome, len(f.calls))
	}
}

func TestPipelineExtractionMalformed(t *testing.T) {
	v, f, g := extractingVerifier(t, nil, gwReply{content: `{"claims": "none"}`})
	r, err := runVerify(t, v, acmeCapture("p16", pricingAnswer))
	if !errors.Is(err, ErrLLMMalformed) || r.Outcome != OutcomeExtractionFailed || len(f.calls) != 0 || g.calls() != 2 {
		t.Errorf("err=%v outcome=%s searches=%d llm=%d", err, r.Outcome, len(f.calls), g.calls())
	}
}

func TestPipelineNoKeyForTenant(t *testing.T) {
	v, _, g := extractingVerifier(t, nil, extractionReply(t, priceClaim))
	c := acmeCapture("p17", pricingAnswer)
	c.VirtualKeyID = "vk-other"
	r, err := runVerify(t, v, c)
	if err != nil || r.Outcome != OutcomeNoVerifierKey || g.calls() != 0 {
		t.Errorf("err=%v outcome=%s llm=%d", err, r.Outcome, g.calls())
	}
}

// The evaluation's verify-all baseline checks what priority would skip.
func TestVerifyAllChecksSkippedClaims(t *testing.T) {
	past := extractedClaim{Text: "Acme Cloud Pro cost $49 per month in 2019.", Span: "Acme Cloud Pro cost $49 per month in 2019", Subject: "Acme Cloud Pro",
		Attribute: "monthly price", AssertedValue: "$49", Category: CategoryPrice, TemporalScope: ScopeHistorical, AsOf: "2019", VolatilityBand: BandWeeksMonths}
	answer := "Acme Cloud Pro cost $49 per month in 2019."
	for verifyAll, wantSearches := range map[bool]int{false: 0, true: 1} {
		v, f, _ := extractingVerifier(t, func(c *Config) { c.VerifyAll, c.DisableExtract = verifyAll, true }, extractionReply(t, past))
		if _, err := runVerify(t, v, acmeCapture("hlr_1", answer)); err != nil {
			t.Fatal(err)
		}
		if len(f.calls) != wantSearches {
			t.Errorf("verify_all %v: %d searches", verifyAll, len(f.calls))
		}
	}
}

// A model may cut its span one clause short of the value it read. The span
// grows to the sentence holding both, and no further.
func TestSpanGrowsToTheSentenceHoldingTheValue(t *testing.T) {
	answer := `Asked "when did the last patch come out?", the answer is "Feb 13, 2024". Later releases cost $5.`
	claim := extractedClaim{Text: "The last patch came out on Feb 13, 2024.", Span: "when did the last patch come out?",
		Subject: "the last patch", AssertedValue: "Feb 13, 2024"}
	widenSpan(&claim, answer)
	if why := checkSpan(claim, answer); why != "" {
		t.Fatalf("rejected: %s (span %q)", why, claim.Span)
	}
	if strings.Contains(claim.Span, "$5") {
		t.Errorf("span left its sentence: %q", claim.Span)
	}

	other := extractedClaim{Text: "The last patch cost $5 in all.", Span: "when did the last patch come out?",
		Subject: "the last patch", AssertedValue: "$5"}
	widenSpan(&other, answer)
	if why := checkSpan(other, answer); why != RejectValueNotInSpan {
		t.Errorf("a value in another sentence must stay rejected, got %q", why)
	}
}
