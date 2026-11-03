package halflife

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	_ "github.com/mattn/go-sqlite3" // the gateway's own SQLite driver (its logs.db uses it)
)

// Report storage. In-flight reports (queued, running) always live in memory,
// where the verifier updates them. With report_store_path set, a report is
// written to SQLite when it completes and then served from there, so it
// survives restarts; the database also keeps each claim and source as rows
// (provenance: which key paid, which relay and judge calls, which quote) for
// operator queries and metrics.

// reports is what the plugin and the verifier need from a store.
type reports interface {
	put(r *Report)
	update(id string, fn func(*Report)) bool
	get(id string) (Report, bool)
	remove(id string)
	close() error
}

// migrations are applied in order, each once; the index + 1 is the version.
// Never edit one that has shipped: append a new one.
var migrations = []string{
	`CREATE TABLE reports (
		id                    TEXT PRIMARY KEY,
		request_id            TEXT NOT NULL,
		tenant_virtual_key_id TEXT NOT NULL,
		outcome               TEXT NOT NULL,
		verdict               TEXT NOT NULL,
		created_at            INTEGER NOT NULL, -- Unix milliseconds
		completed_at          INTEGER NOT NULL,
		document              TEXT NOT NULL     -- the report as served, JSON
	);
	CREATE INDEX reports_created_at ON reports (created_at);
	CREATE INDEX reports_tenant ON reports (tenant_virtual_key_id, created_at);

	CREATE TABLE claims (
		report_id               TEXT NOT NULL REFERENCES reports (id) ON DELETE CASCADE,
		position                INTEGER NOT NULL,
		text                    TEXT NOT NULL,
		category                TEXT NOT NULL,
		temporal_scope          TEXT NOT NULL,
		priority                TEXT NOT NULL,
		status                  TEXT NOT NULL,
		verdict                 TEXT NOT NULL,
		strength                TEXT NOT NULL,
		freshness_score         INTEGER,
		verifier_virtual_key_id TEXT NOT NULL,
		credits                 REAL NOT NULL,
		relay_request_id        TEXT NOT NULL, -- the Tavily call in Bifrost's relay log
		tavily_request_id       TEXT NOT NULL,
		judge_model             TEXT NOT NULL,
		judge_request_ids       TEXT NOT NULL, -- JSON array, in Bifrost's request log
		quote                   TEXT NOT NULL,
		quote_url               TEXT NOT NULL,
		quote_verified          INTEGER NOT NULL,
		PRIMARY KEY (report_id, position)
	);
	CREATE INDEX claims_verdict ON claims (verdict);

	CREATE TABLE claim_sources (
		report_id      TEXT NOT NULL,
		claim_position INTEGER NOT NULL,
		position       INTEGER NOT NULL,
		url            TEXT NOT NULL,
		domain         TEXT NOT NULL,
		title          TEXT NOT NULL,
		published_date TEXT NOT NULL,
		extracted      INTEGER NOT NULL,
		PRIMARY KEY (report_id, claim_position, position),
		FOREIGN KEY (report_id, claim_position) REFERENCES claims (report_id, position) ON DELETE CASCADE
	);`,
}

// sqliteReports keeps in-flight reports in memory and completed ones in SQLite.
type sqliteReports struct {
	live          *reportStore
	db            *sql.DB
	maxRows       int
	retention     time.Duration
	keepAnswer    bool
	now           func() time.Time
	mu            sync.Mutex // serialises writes; SQLite allows one writer
	onWriteFailed func(id string, err error)
}

// openSQLiteReports opens (creating if needed) and migrates the database.
func openSQLiteReports(path string, maxRows int, retention time.Duration, keepAnswer bool, onWriteFailed func(string, error)) (*sqliteReports, error) {
	db, err := sql.Open("sqlite3", "file:"+path+"?_foreign_keys=on&_journal_mode=WAL&_busy_timeout=5000")
	if err != nil {
		return nil, fmt.Errorf("halflife: open report store: %w", err)
	}
	db.SetMaxOpenConns(1) // one connection: writes are serialised anyway, and PRAGMAs hold per connection
	if err := migrate(db); err != nil {
		db.Close()
		return nil, err
	}
	s := &sqliteReports{live: newReportStore(maxRows), db: db, maxRows: maxRows, retention: retention, keepAnswer: keepAnswer,
		now: time.Now, onWriteFailed: onWriteFailed}
	if err := s.prune(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// migrate brings the schema to the newest version, each step in its own
// transaction. A database newer than this build is refused, not touched.
func migrate(db *sql.DB) error {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY, applied_at INTEGER NOT NULL)`); err != nil {
		return fmt.Errorf("halflife: report store: %w", err)
	}
	var current int
	if err := db.QueryRow(`SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&current); err != nil {
		return fmt.Errorf("halflife: report store: %w", err)
	}
	if current > len(migrations) {
		return fmt.Errorf("halflife: report store schema is version %d; this build knows up to %d", current, len(migrations))
	}
	for v := current + 1; v <= len(migrations); v++ {
		tx, err := db.Begin()
		if err != nil {
			return fmt.Errorf("halflife: report store: %w", err)
		}
		if _, err := tx.Exec(migrations[v-1]); err != nil {
			tx.Rollback()
			return fmt.Errorf("halflife: report store migration %d: %w", v, err)
		}
		if _, err := tx.Exec(`INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)`, v, time.Now().UnixMilli()); err != nil {
			tx.Rollback()
			return fmt.Errorf("halflife: report store migration %d: %w", v, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("halflife: report store migration %d: %w", v, err)
		}
	}
	return nil
}

func (s *sqliteReports) put(r *Report) { s.live.put(r) }

// update applies fn in memory; once the report is complete it is written to
// the database and leaves memory. A failed write keeps it in memory, where
// it is still served until evicted.
func (s *sqliteReports) update(id string, fn func(*Report)) bool {
	if !s.live.update(id, fn) {
		return false
	}
	r, ok := s.live.get(id)
	if !ok || r.Status != ReportComplete {
		return true
	}
	if err := s.write(r); err != nil {
		if s.onWriteFailed != nil {
			s.onWriteFailed(id, err)
		}
		return true
	}
	s.live.remove(id)
	return true
}

func (s *sqliteReports) get(id string) (Report, bool) {
	if r, ok := s.live.get(id); ok {
		return r, true
	}
	var doc string
	err := s.db.QueryRow(`SELECT document FROM reports WHERE id = ?`, id).Scan(&doc)
	if err != nil {
		return Report{}, false
	}
	var r Report
	if json.Unmarshal([]byte(doc), &r) != nil {
		return Report{}, false
	}
	return r, true
}

func (s *sqliteReports) remove(id string) {
	s.live.remove(id)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.db.Exec(`DELETE FROM reports WHERE id = ?`, id)
}

func (s *sqliteReports) close() error { return s.db.Close() }

// write stores a completed report, its claims and their sources in one
// transaction, then applies retention.
func (s *sqliteReports) write(r Report) error {
	if !s.keepAnswer {
		r.Answer.Question, r.Answer.Text, r.Answer.TextRedacted = "", "", true
	}
	doc, err := json.Marshal(r)
	if err != nil {
		return err
	}
	completed := s.now()
	if r.CompletedAt != nil {
		completed = *r.CompletedAt
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// A report is written once; a rewrite replaces it with its rows.
	if _, err := tx.Exec(`DELETE FROM reports WHERE id = ?`, r.ID); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO reports (id, request_id, tenant_virtual_key_id, outcome, verdict, created_at, completed_at, document)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		r.ID, r.RequestID, r.Tenant.VirtualKeyID, r.Outcome, r.Verdict, r.CreatedAt.UnixMilli(), completed.UnixMilli(), string(doc)); err != nil {
		return err
	}
	for i, c := range r.Claims {
		if err := insertClaim(tx, r.ID, i, c); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return s.pruneLocked()
}

func insertClaim(tx *sql.Tx, reportID string, pos int, c ReportClaim) error {
	ev := c.Evidence
	if ev == nil {
		ev = &ReportSources{}
	}
	var judgeModel string
	judgeIDs := []string{}
	if c.Judge != nil {
		judgeModel, judgeIDs = c.Judge.Model, append(judgeIDs, c.Judge.GatewayRequestIDs...)
	}
	ids, _ := json.Marshal(judgeIDs)
	var quote, quoteURL string
	var verified bool
	if c.Quote != nil {
		quote, quoteURL, verified = c.Quote.Text, c.Quote.URL, c.Quote.Verified
	}
	if _, err := tx.Exec(`INSERT INTO claims (report_id, position, text, category, temporal_scope, priority, status, verdict, strength,
		freshness_score, verifier_virtual_key_id, credits, relay_request_id, tavily_request_id, judge_model, judge_request_ids,
		quote, quote_url, quote_verified) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		reportID, pos, c.Text, c.Category, c.TemporalScope, c.Priority, c.Status, c.Verdict, c.Strength,
		c.FreshnessScore, ev.VerifierKeyID, ev.Credits, ev.GatewayRequestID, ev.TavilyRequestID, judgeModel, string(ids),
		quote, quoteURL, verified); err != nil {
		return err
	}
	for j, src := range ev.Sources {
		if _, err := tx.Exec(`INSERT INTO claim_sources (report_id, claim_position, position, url, domain, title, published_date, extracted)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			reportID, pos, j, src.URL, registrableDomain(src.URL), src.Title, src.PublishedDate, src.Extracted != ""); err != nil {
			return err
		}
	}
	return nil
}

// prune deletes reports past the retention period, then the oldest beyond
// the row limit. It runs when the store opens and after every write. Claims and sources go with them (ON DELETE CASCADE).
func (s *sqliteReports) prune() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pruneLocked()
}

func (s *sqliteReports) pruneLocked() error {
	cutoff := s.now().Add(-s.retention).UnixMilli()
	if _, err := s.db.Exec(`DELETE FROM reports WHERE created_at < ?`, cutoff); err != nil {
		return fmt.Errorf("halflife: report retention: %w", err)
	}
	_, err := s.db.Exec(`DELETE FROM reports WHERE id IN (SELECT id FROM reports ORDER BY created_at DESC, id DESC LIMIT -1 OFFSET ?)`, s.maxRows)
	if err != nil {
		return fmt.Errorf("halflife: report retention: %w", err)
	}
	return nil
}

// count is the number of stored reports (tests and diagnostics).
func (s *sqliteReports) count() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM reports`).Scan(&n)
	return n, err
}

// schemaVersion reports the database's migration version.
func (s *sqliteReports) schemaVersion() (int, error) {
	var v int
	err := s.db.QueryRow(`SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&v)
	return v, err
}
