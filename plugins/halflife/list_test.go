package halflife

import (
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"
)

// listed returns the IDs of a list, in order.
func listed(s []ReportSummary) []string {
	ids := make([]string, len(s))
	for i, r := range s {
		ids[i] = r.ID
	}
	return ids
}

func equalIDs(a []string, b ...string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// listFixture puts five reports in s, IDs in creation order: two tenants,
// two models, verdicts and outcomes varied. hlr_5 is still running.
func listFixture(t *testing.T, s reports) {
	t.Helper()
	base := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	for i, tc := range []struct{ tenant, provider, model, verdict, outcome string }{
		{"vk-a", "openai", "gpt-4o", VerdictCurrent, OutcomeEvidenceFound},
		{"vk-b", "openai", "gpt-4o", VerdictContradicted, OutcomeEvidenceFound},
		{"vk-a", "anthropic", "claude", VerdictOutdated, OutcomeEvidenceFound},
		{"vk-a", "openai", "gpt-4o", VerdictUnverifiable, OutcomeNoClaim},
	} {
		r := judgedReport("hlr_"+string(rune('1'+i)), base.Add(time.Duration(i)*time.Second))
		r.Tenant.VirtualKeyID, r.Answer.Provider, r.Answer.Model = tc.tenant, tc.provider, tc.model
		s.put(r)
		done := r.CreatedAt.Add(time.Second)
		s.update(r.ID, func(r *Report) {
			r.Status, r.Outcome, r.Verdict, r.CompletedAt = ReportComplete, tc.outcome, tc.verdict, &done
		})
	}
	live := judgedReport("hlr_5", base.Add(5*time.Second))
	live.Tenant.VirtualKeyID = "vk-a"
	s.put(live) // queued: stays in memory with a SQLite store
}

func checkList(t *testing.T, s reports) {
	t.Helper()
	for name, tc := range map[string]struct {
		f    ReportFilter
		want []string
	}{
		"all, newest first":  {ReportFilter{}, []string{"hlr_5", "hlr_4", "hlr_3", "hlr_2", "hlr_1"}},
		"tenant":             {ReportFilter{Tenant: "vk-b"}, []string{"hlr_2"}},
		"provider, any case": {ReportFilter{Provider: "OpenAI"}, []string{"hlr_5", "hlr_4", "hlr_2", "hlr_1"}},
		"model":              {ReportFilter{Model: "claude"}, []string{"hlr_3"}},
		"verdict, any case":  {ReportFilter{Verdict: "contradicted"}, []string{"hlr_2"}},
		"outcome":            {ReportFilter{Outcome: OutcomeNoClaim}, []string{"hlr_4"}},
		"request":            {ReportFilter{RequestID: "req-hlr_3"}, []string{"hlr_3"}},
		"page size":          {ReportFilter{Limit: 2}, []string{"hlr_5", "hlr_4"}},
		"next page":          {ReportFilter{Before: "hlr_4", Limit: 2}, []string{"hlr_3", "hlr_2"}},
		"filters combine":    {ReportFilter{Tenant: "vk-a", Provider: "openai", Before: "hlr_5"}, []string{"hlr_4", "hlr_1"}},
		"nothing matches":    {ReportFilter{Tenant: "vk-z"}, nil},
	} {
		if got := listed(s.list(tc.f)); !equalIDs(got, tc.want...) {
			t.Errorf("%s: %v, want %v", name, got, tc.want)
		}
	}
	got := s.list(ReportFilter{RequestID: "req-hlr_3"})
	if len(got) != 1 {
		t.Fatal(got)
	}
	r := got[0]
	if r.Status != ReportComplete || r.Verdict != VerdictOutdated || r.Tenant != "vk-a" || r.Provider != "anthropic" || r.Claims != 1 ||
		r.ClaimsChecked != 1 || r.Credits != 1 || r.CompletedAt == nil {
		t.Errorf("summary %+v", r)
	}
}

func TestReportListMemory(t *testing.T) {
	s := newReportStore(100)
	listFixture(t, s)
	checkList(t, s)
}

func TestReportListSQLite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "r.db")
	s := openTestStore(t, path, 100, false)
	listFixture(t, s)
	if n, _ := s.count(); n != 4 {
		t.Fatalf("%d stored, want 4 (hlr_5 is in flight)", n)
	}
	checkList(t, s)
	s.close()

	// After a restart only stored reports remain, and the list still works.
	s = openTestStore(t, path, 100, false)
	defer s.close()
	if got := listed(s.list(ReportFilter{Tenant: "vk-a"})); !equalIDs(got, "hlr_4", "hlr_3", "hlr_1") {
		t.Errorf("after reopen: %v", got)
	}
}

func TestReportListLimit(t *testing.T) {
	for in, want := range map[int]int{0: DefaultReportListLimit, -3: DefaultReportListLimit, 7: 7, MaxReportListLimit + 1: MaxReportListLimit} {
		if got := (ReportFilter{Limit: in}).limit(); got != want {
			t.Errorf("limit(%d) = %d, want %d", in, got, want)
		}
	}
}

// Migration 2 backfills provider and model from reports stored before it.
func TestMigrationBackfillsProviderAndModel(t *testing.T) {
	path := filepath.Join(t.TempDir(), "r.db")
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, applied_at INTEGER NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(migrations[0]); err != nil {
		t.Fatal(err)
	}
	db.Exec(`INSERT INTO schema_migrations (version, applied_at) VALUES (1, 0)`)
	old := judgedReport("hlr_old", time.Now())
	doc, _ := json.Marshal(old)
	if _, err := db.Exec(`INSERT INTO reports (id, request_id, tenant_virtual_key_id, outcome, verdict, created_at, completed_at, document)
		VALUES ('hlr_old', 'req-old', 'vk-acme', 'evidence_found', 'OUTDATED', ?, ?, ?)`, old.CreatedAt.UnixMilli(), old.CreatedAt.UnixMilli(), string(doc)); err != nil {
		t.Fatalf("version-1 insert: %v", err)
	}
	db.Close()

	s := openTestStore(t, path, 100, false)
	defer s.close()
	if v, _ := s.schemaVersion(); v != len(migrations) {
		t.Errorf("schema version %d, want %d", v, len(migrations))
	}
	var provider, model string
	if err := s.db.QueryRow(`SELECT provider, model FROM reports WHERE id = 'hlr_old'`).Scan(&provider, &model); err != nil || provider != "openai" || model != "ok" {
		t.Errorf("backfill: %q %q %v", provider, model, err)
	}
	if got := listed(s.list(ReportFilter{Provider: "openai", Model: "ok"})); !equalIDs(got, "hlr_old") {
		t.Errorf("list after backfill: %v", got)
	}
}
