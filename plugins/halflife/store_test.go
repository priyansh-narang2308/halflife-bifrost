package halflife

import (
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func openTestStore(t *testing.T, path string, maxRows int, keepAnswer bool) *sqliteReports {
	t.Helper()
	s, err := openSQLiteReports(path, maxRows, 24*time.Hour, keepAnswer, func(id string, err error) { t.Errorf("write %s: %v", id, err) })
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// judgedReport is a completed report with one judged claim and two sources.
func judgedReport(id string, created time.Time) *Report {
	r := newReport(&Capture{ReportID: id, RequestID: "req-" + id, VirtualKeyID: "vk-acme", Provider: "openai", Model: "ok",
		Question: "How much is Acme Cloud Pro?", Answer: "Acme Cloud Pro costs $49 per month.", CapturedAt: created})
	score := 5
	r.Claims = []ReportClaim{{
		Text: "Acme Cloud Pro costs $49 per month.", Span: "Acme Cloud Pro costs $49 per month", Category: CategoryPrice,
		TemporalScope: ScopeCurrent, Priority: PriorityHigh, Status: ClaimChecked,
		Verdict: VerdictOutdated, Strength: StrengthStrong, FreshnessScore: &score,
		Quote: &ReportQuote{Text: "Pro plan: $59 per month", Source: 0, URL: "https://acme.example/pricing", Verified: true},
		Judge: &LLMResult{Model: "nemotron", GatewayRequestIDs: []string{"gw-judge-1"}},
		Evidence: &ReportSources{VerifierKeyID: "vk-verifier-acme", Credits: 1, GatewayRequestID: "gw-relay-1", TavilyRequestID: "tv-1",
			Sources: []ReportSource{
				{Title: "Pricing", URL: "https://docs.acme.example/pricing", Snippet: "Pro plan: $59 per month", Extracted: "Pro: $59"},
				{Title: "News", URL: "https://news.example/acme", PublishedDate: "2026-10-01", Snippet: "from $49 to $59"},
			}},
	}}
	return r
}

func complete(t *testing.T, s reports, r *Report) {
	t.Helper()
	s.put(r)
	done := r.CreatedAt.Add(time.Second)
	if !s.update(r.ID, func(r *Report) {
		r.Status, r.Outcome, r.Verdict, r.CompletedAt = ReportComplete, OutcomeEvidenceFound, VerdictOutdated, &done
	}) {
		t.Fatal("update lost the report")
	}
}

func TestSQLiteReportsSurviveReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reports.db")
	s := openTestStore(t, path, 100, false)
	r := judgedReport("hlr_1", time.Now())

	s.put(r)
	if got, ok := s.get("hlr_1"); !ok || got.Status != ReportQueued || got.Answer.Text == "" {
		t.Fatalf("in-flight report: %+v %v", got.Status, ok) // served from memory, unredacted
	}
	if n, _ := s.count(); n != 0 {
		t.Errorf("in-flight report written: %d rows", n)
	}
	complete(t, s, r)
	if _, inMemory := s.live.get("hlr_1"); inMemory {
		t.Error("completed report still in memory")
	}
	if err := s.close(); err != nil {
		t.Fatal(err)
	}

	s = openTestStore(t, path, 100, false)
	defer s.close()
	got, ok := s.get("hlr_1")
	if !ok || got.Status != ReportComplete || got.Verdict != VerdictOutdated || len(got.Claims) != 1 || *got.Claims[0].FreshnessScore != 5 {
		t.Fatalf("after reopen: %v %+v", ok, got)
	}
	// The answer text is not persisted by default; claims and spans are.
	if got.Answer.Text != "" || got.Answer.Question != "" || !got.Answer.TextRedacted || got.Claims[0].Span == "" {
		t.Errorf("answer %+v", got.Answer)
	}
	if _, ok := s.get("hlr_missing"); ok {
		t.Error("unknown report found")
	}
	if v, err := s.schemaVersion(); err != nil || v != len(migrations) {
		t.Errorf("schema version %d %v", v, err)
	}
}

func TestSQLiteReportsKeepAnswerWhenAsked(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "r.db"), 100, true)
	defer s.close()
	complete(t, s, judgedReport("hlr_1", time.Now()))
	if got, _ := s.get("hlr_1"); got.Answer.Text != "Acme Cloud Pro costs $49 per month." || got.Answer.TextRedacted {
		t.Errorf("answer %+v", got.Answer)
	}
}

func TestSQLiteReportsProvenanceRows(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "r.db"), 100, false)
	defer s.close()
	complete(t, s, judgedReport("hlr_1", time.Now()))

	var verdict, strength, key, relay, judgeModel, judgeIDs, quoteURL string
	var score sql.NullInt64
	var credits float64
	var verified bool
	err := s.db.QueryRow(`SELECT verdict, strength, freshness_score, verifier_virtual_key_id, credits, relay_request_id, judge_model,
		judge_request_ids, quote_url, quote_verified FROM claims WHERE report_id = 'hlr_1' AND position = 0`).
		Scan(&verdict, &strength, &score, &key, &credits, &relay, &judgeModel, &judgeIDs, &quoteURL, &verified)
	if err != nil {
		t.Fatal(err)
	}
	if verdict != VerdictOutdated || strength != StrengthStrong || score.Int64 != 5 || key != "vk-verifier-acme" || credits != 1 ||
		relay != "gw-relay-1" || judgeModel != "nemotron" || judgeIDs != `["gw-judge-1"]` || quoteURL != "https://acme.example/pricing" || !verified {
		t.Errorf("claim row: %s %s %v %s %v %s %s %s %s %v", verdict, strength, score, key, credits, relay, judgeModel, judgeIDs, quoteURL, verified)
	}
	rows, err := s.db.Query(`SELECT domain, published_date, extracted FROM claim_sources WHERE report_id = 'hlr_1' ORDER BY position`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var domain, published string
		var extracted bool
		rows.Scan(&domain, &published, &extracted)
		got = append(got, domain+"|"+published+"|"+map[bool]string{true: "x", false: "-"}[extracted])
	}
	if strings.Join(got, ",") != "acme.example||x,news.example|2026-10-01|-" {
		t.Errorf("source rows %v", got)
	}
}

func TestSQLiteReportsRetention(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "r.db"), 2, false)
	defer s.close()
	now := time.Now()
	s.now = func() time.Time { return now }
	complete(t, s, judgedReport("hlr_old", now.Add(-25*time.Hour))) // past the 24 h retention
	for i, id := range []string{"hlr_a", "hlr_b", "hlr_c"} {
		complete(t, s, judgedReport(id, now.Add(time.Duration(i)*time.Minute)))
	}
	n, _ := s.count()
	_, old := s.get("hlr_old")
	_, a := s.get("hlr_a")
	_, c := s.get("hlr_c")
	if n != 2 || old || a || !c {
		t.Errorf("rows=%d old=%v a=%v c=%v; want the newest 2", n, old, a, c)
	}
	var orphans int
	s.db.QueryRow(`SELECT (SELECT COUNT(*) FROM claims WHERE report_id NOT IN (SELECT id FROM reports)) +
		(SELECT COUNT(*) FROM claim_sources WHERE report_id NOT IN (SELECT id FROM reports))`).Scan(&orphans)
	if orphans != 0 {
		t.Errorf("%d claim or source rows outlived their report", orphans)
	}
}

func TestSQLiteReportsRemoveAndRewrite(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "r.db"), 10, false)
	defer s.close()
	r := judgedReport("hlr_1", time.Now())
	complete(t, s, r)
	// A second completion (not expected, but possible) replaces the rows.
	s.put(r)
	complete(t, s, r)
	var claims int
	s.db.QueryRow(`SELECT COUNT(*) FROM claims`).Scan(&claims)
	if n, _ := s.count(); n != 1 || claims != 1 {
		t.Errorf("rows %d claims %d", n, claims)
	}
	s.remove("hlr_1")
	if _, ok := s.get("hlr_1"); ok {
		t.Error("removed report still served")
	}
	if s.update("hlr_unknown", func(*Report) {}) {
		t.Error("update of an unknown report succeeded")
	}
}

func TestSQLiteReportsWriteFailureKeepsReportInMemory(t *testing.T) {
	var failed []string
	s, err := openSQLiteReports(filepath.Join(t.TempDir(), "r.db"), 10, time.Hour, false, func(id string, err error) { failed = append(failed, id) })
	if err != nil {
		t.Fatal(err)
	}
	s.db.Close() // every write now fails
	r := judgedReport("hlr_1", time.Now())
	complete(t, s, r)
	if got, ok := s.get("hlr_1"); !ok || got.Status != ReportComplete || len(failed) != 1 {
		t.Errorf("served=%v failed=%v", ok, failed)
	}
}

func TestMigrations(t *testing.T) {
	path := filepath.Join(t.TempDir(), "r.db")
	s := openTestStore(t, path, 10, false)
	s.close()
	// Reopening applies nothing twice.
	s = openTestStore(t, path, 10, false)
	var applied int
	s.db.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&applied)
	if applied != len(migrations) {
		t.Errorf("%d migrations recorded, want %d", applied, len(migrations))
	}
	// A database from a newer build is refused, not modified.
	s.db.Exec(`INSERT INTO schema_migrations (version, applied_at) VALUES (?, 0)`, len(migrations)+1)
	s.close()
	if _, err := openSQLiteReports(path, 10, time.Hour, false, nil); err == nil || !strings.Contains(err.Error(), "this build knows up to") {
		t.Errorf("newer schema: %v", err)
	}
	if _, err := openSQLiteReports(filepath.Join(t.TempDir(), "missing", "dir", "r.db"), 10, time.Hour, false, nil); err == nil {
		t.Error("opened a database in a missing directory")
	}
}

func TestPluginWithPersistentStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reports.db")
	p, err := Init(t.Context(), &Config{ReportStorePath: path}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := p.reports.(*sqliteReports); !ok {
		t.Fatalf("store is %T", p.reports)
	}
	complete(t, p.reports, judgedReport("hlr_1", time.Now()))
	if err := p.Cleanup(); err != nil {
		t.Fatal(err)
	}
	p, err = Init(t.Context(), &Config{ReportStorePath: path}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Cleanup()
	got, ok := p.Report("hlr_1")
	if !ok {
		t.Fatal("report lost across restart")
	}
	b, _ := json.Marshal(got)
	if !strings.Contains(string(b), `"text_redacted":true`) || strings.Contains(string(b), "How much is Acme") {
		t.Errorf("served %s", b)
	}
	if _, err := Init(t.Context(), &Config{ReportStorePath: filepath.Join(t.TempDir(), "no", "such", "dir.db")}, nil); err == nil {
		t.Error("Init succeeded with an unusable store path")
	}
}
