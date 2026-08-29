// Copyright 2025 MCTL Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package ticket

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	_ "github.com/lib/pq"
	_ "modernc.org/sqlite"
)

// Store persists tickets and evidence.
type Store struct {
	db      *sql.DB
	dialect string
}

// NewStore opens (or creates) the database.
// Supports "sqlite" (path to file) or "postgres" (postgres://... URL).
//
// ctx bounds the blocking I/O below (WAL pragma, version probe, migration).
// connect_timeout on the DSN only bounds the initial dial; a hang after the
// connection is established (e.g. during migration) is otherwise invisible
// to callers watching ctx for cancellation, such as initTicketStore's
// SIGTERM handling in cmd/agent.
func NewStore(ctx context.Context, connStr string) (*Store, error) {
	driver := "sqlite"
	if strings.HasPrefix(connStr, "postgres://") || strings.HasPrefix(connStr, "postgresql://") {
		driver = "postgres"
	}

	db, err := sql.Open(driver, connStr)
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", driver, err)
	}

	if driver == "sqlite" {
		// WAL mode for better read concurrency — best-effort, non-fatal.
		_, _ = db.ExecContext(ctx, "PRAGMA journal_mode=WAL")
		// Log embedded SQLite version for audit/observability.
		var sqliteVersion string
		if err := db.QueryRowContext(ctx, "SELECT sqlite_version()").Scan(&sqliteVersion); err == nil {
			slog.Info("sqlite driver initialised", "sqlite_version", sqliteVersion)
		}
	}

	s := &Store{db: db, dialect: driver}
	if err := s.migrate(ctx); err != nil {
		// sql.Open starts a background connectionOpener goroutine that only
		// exits on Close. Callers retry NewStore, so leaving it open here
		// leaks one goroutine (and one *sql.DB) per failed attempt.
		_ = db.Close()
		return nil, fmt.Errorf("migrating: %w", err)
	}
	return s, nil
}

func (s *Store) rebind(query string) string {
	if s.dialect != "postgres" {
		return query
	}
	// Replace ? with $1, $2, etc.
	out := make([]byte, 0, len(query))
	argIdx := 1
	for i := 0; i < len(query); i++ {
		if query[i] == '?' {
			out = append(out, '$')
			out = append(out, fmt.Sprintf("%d", argIdx)...)
			argIdx++
		} else {
			out = append(out, query[i])
		}
	}
	return string(out)
}

func (s *Store) migrate(ctx context.Context) error {
	var err error
	if s.dialect == "postgres" {
		_, err = s.db.ExecContext(ctx, `
			CREATE TABLE IF NOT EXISTS tickets (
				id          TEXT PRIMARY KEY,
				source      TEXT NOT NULL,
				type        TEXT NOT NULL,
				tenant      TEXT NOT NULL DEFAULT '',
				service     TEXT NOT NULL DEFAULT '',
				summary     TEXT NOT NULL DEFAULT '',
				severity    TEXT NOT NULL DEFAULT 'info',
				status      TEXT NOT NULL DEFAULT 'open',
				analysis    TEXT NOT NULL DEFAULT '',
				proposed_fix TEXT NOT NULL DEFAULT '',
				pr_url      TEXT NOT NULL DEFAULT '',
				pr_number   INTEGER NOT NULL DEFAULT 0,
				pr_repo     TEXT NOT NULL DEFAULT '',
				pr_branch   TEXT NOT NULL DEFAULT '',
				pr_commit_sha TEXT NOT NULL DEFAULT '',
				confidence  TEXT NOT NULL DEFAULT '',
				created_at  TIMESTAMPTZ NOT NULL,
				updated_at  TIMESTAMPTZ NOT NULL,
				resolved_at TIMESTAMPTZ
			);

			CREATE TABLE IF NOT EXISTS evidence (
				id           SERIAL PRIMARY KEY,
				ticket_id    TEXT NOT NULL REFERENCES tickets(id),
				type         TEXT NOT NULL,
				content      TEXT NOT NULL,
				collected_at TIMESTAMPTZ NOT NULL
			);
		`)
	} else {
		_, err = s.db.ExecContext(ctx, `
			CREATE TABLE IF NOT EXISTS tickets (
				id          TEXT PRIMARY KEY,
				source      TEXT NOT NULL,
				type        TEXT NOT NULL,
				tenant      TEXT NOT NULL DEFAULT '',
				service     TEXT NOT NULL DEFAULT '',
				summary     TEXT NOT NULL DEFAULT '',
				severity    TEXT NOT NULL DEFAULT 'info',
				status      TEXT NOT NULL DEFAULT 'open',
				analysis    TEXT NOT NULL DEFAULT '',
				proposed_fix TEXT NOT NULL DEFAULT '',
				pr_url      TEXT NOT NULL DEFAULT '',
				pr_number   INTEGER NOT NULL DEFAULT 0,
				pr_repo     TEXT NOT NULL DEFAULT '',
				pr_branch   TEXT NOT NULL DEFAULT '',
				pr_commit_sha TEXT NOT NULL DEFAULT '',
				confidence  TEXT NOT NULL DEFAULT '',
				created_at  DATETIME NOT NULL,
				updated_at  DATETIME NOT NULL,
				resolved_at DATETIME
			);

			CREATE TABLE IF NOT EXISTS evidence (
				id           INTEGER PRIMARY KEY AUTOINCREMENT,
				ticket_id    TEXT NOT NULL REFERENCES tickets(id),
				type         TEXT NOT NULL,
				content      TEXT NOT NULL,
				collected_at DATETIME NOT NULL
			);
		`)
	}
	if err != nil {
		return err
	}

	if err := s.ensureColumn(ctx, "tickets", "alert_name", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err := s.ensureColumn(ctx, "tickets", "pr_repo", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err := s.ensureColumn(ctx, "tickets", "pr_branch", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err := s.ensureColumn(ctx, "tickets", "pr_commit_sha", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err := s.ensureColumn(ctx, "tickets", "alert_fingerprint", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err := s.ensureIndex(ctx, "idx_tickets_alert_fingerprint", "tickets", "alert_fingerprint"); err != nil {
		return err
	}

	_, err = s.db.ExecContext(ctx, `
		CREATE INDEX IF NOT EXISTS idx_tickets_status ON tickets(status);
		CREATE INDEX IF NOT EXISTS idx_tickets_tenant_service_type ON tickets(tenant, service, type);
		CREATE INDEX IF NOT EXISTS idx_evidence_ticket ON evidence(ticket_id);
	`)
	return err
}

func (s *Store) ensureColumn(ctx context.Context, table, column, definition string) error {
	query := fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", table, column, definition)
	if s.dialect == "postgres" {
		query = fmt.Sprintf("ALTER TABLE %s ADD COLUMN IF NOT EXISTS %s %s", table, column, definition)
	}
	if _, err := s.db.ExecContext(ctx, query); err != nil {
		if s.dialect != "postgres" && strings.Contains(strings.ToLower(err.Error()), "duplicate column") {
			return nil
		}
		return err
	}
	return nil
}

func (s *Store) ensureIndex(ctx context.Context, name, table, column string) error {
	query := fmt.Sprintf("CREATE INDEX IF NOT EXISTS %s ON %s(%s)", name, table, column)
	_, err := s.db.ExecContext(ctx, query)
	return err
}

// Create inserts a new ticket, generating a UUID.
func (s *Store) Create(ctx context.Context, t *Ticket) error {
	t.ID = uuid.New().String()
	now := time.Now().UTC()
	t.CreatedAt = now
	t.UpdatedAt = now
	if t.Status == "" {
		t.Status = StatusOpen
	}

	query := `
		INSERT INTO tickets (id, source, alert_name, type, tenant, service, summary, severity, status,
			analysis, proposed_fix, pr_url, pr_number, pr_repo, pr_branch, pr_commit_sha, confidence, alert_fingerprint, created_at, updated_at, resolved_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

	_, err := s.db.ExecContext(ctx, s.rebind(query),
		t.ID, t.Source, t.AlertName, t.Type, t.Tenant, t.Service, t.Summary, t.Severity, t.Status,
		t.Analysis, t.ProposedFix, t.PRURL, t.PRNumber, t.PRRepo, t.PRBranch, t.PRCommitSHA, t.Confidence, t.AlertFingerprint,
		t.CreatedAt, t.UpdatedAt, t.ResolvedAt,
	)
	return err
}

// Update saves changes to an existing ticket.
func (s *Store) Update(ctx context.Context, t *Ticket) error {
	t.UpdatedAt = time.Now().UTC()
	query := `
		UPDATE tickets SET source=?, alert_name=?, type=?, tenant=?, service=?, summary=?, severity=?, status=?,
			analysis=?, proposed_fix=?, pr_url=?, pr_number=?, pr_repo=?, pr_branch=?, pr_commit_sha=?, confidence=?,
			alert_fingerprint=?, updated_at=?, resolved_at=?
		WHERE id=?`

	_, err := s.db.ExecContext(ctx, s.rebind(query),
		t.Source, t.AlertName, t.Type, t.Tenant, t.Service, t.Summary, t.Severity, t.Status,
		t.Analysis, t.ProposedFix, t.PRURL, t.PRNumber, t.PRRepo, t.PRBranch, t.PRCommitSHA, t.Confidence,
		t.AlertFingerprint, t.UpdatedAt, t.ResolvedAt, t.ID,
	)
	return err
}

// EscalateFromStatus records an escalation, and only the fields an escalation
// owns: status, analysis and confidence. It applies while the stored row is
// still at fromStatus, and reports whether it did.
//
// Deliberately NOT a guarded Update. Update rewrites every column from an
// in-memory ticket, which is safe only while nothing else can have touched the
// row — and the whole point of a detached write is that it lands after the
// caller's context is gone, with plenty of room for something else to have
// touched it. A same-status guard is not enough either: a duplicate firing
// appends to alert_fingerprint via TouchWithFingerprint without changing
// status, so a full-row write would pass the guard and revert the fingerprint
// set. Reconciliation resolves a ticket only when ALL of its fingerprints are
// absent from the active set, so losing one lets a still-firing incident be
// closed. Narrow the columns instead of widening the guard.
func (s *Store) EscalateFromStatus(ctx context.Context, id, fromStatus, status, analysis, confidence string) (bool, error) {
	res, err := s.db.ExecContext(ctx, s.rebind(`
		UPDATE tickets SET status=?, analysis=?, confidence=?, updated_at=?
		WHERE id=? AND status=?`),
		status, analysis, confidence, time.Now().UTC(), id, fromStatus,
	)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// RecordPRLinkage stores the PR a fix produced and reports whether the status
// advanced.
//
// Two statements, and the split is the point. The first is guarded on
// fromStatus and carries everything this caller owns — the PR coordinates, the
// diagnosis it just produced, and the new status — so RowsAffected answers
// "did the transition apply" atomically, from the write itself. An
// UPDATE-then-SELECT readback cannot: a concurrent write in the gap makes the
// answer describe someone else's row, and a read that merely fails leaves the
// caller believing a write that landed did not.
//
// When the guard misses, the row moved on and its newer state must stand — but
// the PR still exists on GitHub, so the second statement records the
// coordinates alone. Not the analysis: a resolution that won the race appended
// its own reason there, and overwriting it would erase why the incident
// actually closed.
func (s *Store) RecordPRLinkage(ctx context.Context, t *Ticket, fromStatus string) (bool, error) {
	t.UpdatedAt = time.Now().UTC()
	res, err := s.db.ExecContext(ctx, s.rebind(`
		UPDATE tickets SET
			pr_url=?, pr_number=?, pr_repo=?, pr_branch=?, pr_commit_sha=?,
			analysis=?, proposed_fix=?, confidence=?, status=?, updated_at=?
		WHERE id=? AND status=?`),
		t.PRURL, t.PRNumber, t.PRRepo, t.PRBranch, t.PRCommitSHA,
		t.Analysis, t.ProposedFix, t.Confidence, t.Status, t.UpdatedAt,
		t.ID, fromStatus,
	)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	if n > 0 {
		return true, nil
	}

	_, err = s.db.ExecContext(ctx, s.rebind(`
		UPDATE tickets SET
			pr_url=?, pr_number=?, pr_repo=?, pr_branch=?, pr_commit_sha=?, updated_at=?
		WHERE id=?`),
		t.PRURL, t.PRNumber, t.PRRepo, t.PRBranch, t.PRCommitSHA, t.UpdatedAt, t.ID,
	)
	return false, err
}

// Get retrieves a ticket by ID, including evidence.
func (s *Store) Get(ctx context.Context, id string) (*Ticket, error) {
	t := &Ticket{}
	var resolvedAt sql.NullTime
	query := `
		SELECT id, source, alert_name, type, tenant, service, summary, severity, status,
			analysis, proposed_fix, pr_url, pr_number, pr_repo, pr_branch, pr_commit_sha, confidence, alert_fingerprint, created_at, updated_at, resolved_at
		FROM tickets WHERE id=?`

	err := s.db.QueryRowContext(ctx, s.rebind(query), id).Scan(&t.ID, &t.Source, &t.AlertName, &t.Type, &t.Tenant, &t.Service, &t.Summary, &t.Severity, &t.Status,
		&t.Analysis, &t.ProposedFix, &t.PRURL, &t.PRNumber, &t.PRRepo, &t.PRBranch, &t.PRCommitSHA, &t.Confidence, &t.AlertFingerprint,
		&t.CreatedAt, &t.UpdatedAt, &resolvedAt,
	)
	if err != nil {
		return nil, err
	}
	if resolvedAt.Valid {
		t.ResolvedAt = &resolvedAt.Time
	}

	t.Evidence, err = s.loadEvidence(ctx, id)
	if err != nil {
		return nil, err
	}
	return t, nil
}

// ListOpen returns all non-resolved, non-suppressed tickets.
func (s *Store) ListOpen(ctx context.Context) ([]*Ticket, error) {
	// StatusEscalated is included deliberately: it is terminal for the pipeline
	// but the problem is still open, so the watchdog, the AlertManager reconcile
	// and orphan pruning must all keep seeing it. Leaving it out would recreate
	// the stuck-forever bug this status was introduced to fix, minus the watchdog.
	return s.listByStatus(ctx, StatusOpen, StatusAnalyzing, StatusEscalated, StatusFixProposed, StatusFixApplied)
}

// ListAll returns all tickets (latest first, limit 100).
func (s *Store) ListAll(ctx context.Context) ([]*Ticket, error) {
	return s.ListByFilters(ctx, "", "", "", 100)
}

// ListByFilters returns tickets matching the given filters, latest first.
// Empty filter values are ignored. limit <= 0 means no LIMIT clause.
// Filters are applied in SQL before the limit so narrow queries return
// correct results even when the underlying table is much larger.
func (s *Store) ListByFilters(ctx context.Context, status, tenant, service string, limit int) ([]*Ticket, error) {
	query := `
		SELECT id, source, alert_name, type, tenant, service, summary, severity, status,
			analysis, proposed_fix, pr_url, pr_number, pr_repo, pr_branch, pr_commit_sha, confidence, alert_fingerprint, created_at, updated_at, resolved_at
		FROM tickets`
	var clauses []string
	var args []interface{}
	if status != "" {
		clauses = append(clauses, "status=?")
		args = append(args, status)
	}
	if tenant != "" {
		clauses = append(clauses, "tenant=?")
		args = append(args, tenant)
	}
	if service != "" {
		clauses = append(clauses, "service=?")
		args = append(args, service)
	}
	if len(clauses) > 0 {
		query += " WHERE " + strings.Join(clauses, " AND ")
	}
	query += " ORDER BY created_at DESC"
	if limit > 0 {
		query += " LIMIT ?"
		args = append(args, limit)
	}

	rows, err := s.db.QueryContext(ctx, s.rebind(query), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck
	return s.scanTickets(rows)
}

func (s *Store) listByStatus(ctx context.Context, statuses ...string) ([]*Ticket, error) {
	query := `
		SELECT id, source, alert_name, type, tenant, service, summary, severity, status,
			analysis, proposed_fix, pr_url, pr_number, pr_repo, pr_branch, pr_commit_sha, confidence, alert_fingerprint, created_at, updated_at, resolved_at
		FROM tickets WHERE status IN (`
	args := make([]interface{}, len(statuses))
	for i, st := range statuses {
		if i > 0 {
			query += ","
		}
		query += "?"
		args[i] = st
	}
	query += ") ORDER BY created_at DESC"

	rows, err := s.db.QueryContext(ctx, s.rebind(query), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck
	return s.scanTickets(rows)
}

func (s *Store) scanTickets(rows *sql.Rows) ([]*Ticket, error) {
	var tickets []*Ticket
	for rows.Next() {
		t := &Ticket{}
		var resolvedAt sql.NullTime
		if err := rows.Scan(&t.ID, &t.Source, &t.AlertName, &t.Type, &t.Tenant, &t.Service, &t.Summary,
			&t.Severity, &t.Status, &t.Analysis, &t.ProposedFix, &t.PRURL, &t.PRNumber, &t.PRRepo, &t.PRBranch, &t.PRCommitSHA,
			&t.Confidence, &t.AlertFingerprint, &t.CreatedAt, &t.UpdatedAt, &resolvedAt); err != nil {
			return nil, err
		}
		if resolvedAt.Valid {
			t.ResolvedAt = &resolvedAt.Time
		}
		tickets = append(tickets, t)
	}
	return tickets, rows.Err()
}

// FindDuplicate checks for an existing open ticket with the same tenant, service, and type.
func (s *Store) FindDuplicate(ctx context.Context, tenant, service, ticketType string) (*Ticket, error) {
	t := &Ticket{}
	var resolvedAt sql.NullTime
	query := `
		SELECT id, source, alert_name, type, tenant, service, summary, severity, status,
			analysis, proposed_fix, pr_url, pr_number, pr_repo, pr_branch, pr_commit_sha, confidence, alert_fingerprint, created_at, updated_at, resolved_at
		FROM tickets
		WHERE tenant=? AND service=? AND type=? AND status NOT IN (?, ?)
		ORDER BY created_at DESC LIMIT 1`

	err := s.db.QueryRowContext(ctx, s.rebind(query),
		tenant, service, ticketType, StatusResolved, StatusSuppressed,
	).Scan(&t.ID, &t.Source, &t.AlertName, &t.Type, &t.Tenant, &t.Service, &t.Summary, &t.Severity, &t.Status,
		&t.Analysis, &t.ProposedFix, &t.PRURL, &t.PRNumber, &t.PRRepo, &t.PRBranch, &t.PRCommitSHA, &t.Confidence, &t.AlertFingerprint,
		&t.CreatedAt, &t.UpdatedAt, &resolvedAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if resolvedAt.Valid {
		t.ResolvedAt = &resolvedAt.Time
	}
	return t, nil
}

// AddEvidence adds evidence to a ticket.
func (s *Store) AddEvidence(ctx context.Context, ticketID string, ev Evidence) error {
	query := `
		INSERT INTO evidence (ticket_id, type, content, collected_at)
		VALUES (?, ?, ?, ?)`

	_, err := s.db.ExecContext(ctx, s.rebind(query),
		ticketID, ev.Type, ev.Content, ev.CollectedAt,
	)
	return err
}

func (s *Store) loadEvidence(ctx context.Context, ticketID string) ([]Evidence, error) {
	query := `
		SELECT type, content, collected_at FROM evidence
		WHERE ticket_id=? ORDER BY collected_at`

	rows, err := s.db.QueryContext(ctx, s.rebind(query), ticketID)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck

	var evs []Evidence
	for rows.Next() {
		var ev Evidence
		if err := rows.Scan(&ev.Type, &ev.Content, &ev.CollectedAt); err != nil {
			return nil, err
		}
		evs = append(evs, ev)
	}
	return evs, rows.Err()
}

// CountPRsInWindow counts tickets with non-empty PR URLs created in the last N hours.
func (s *Store) CountPRsInWindow(ctx context.Context, hours int) (int, error) {
	since := time.Now().UTC().Add(-time.Duration(hours) * time.Hour)
	var count int
	query := `
		SELECT COUNT(*) FROM tickets
		WHERE pr_url != '' AND created_at > ?`

	err := s.db.QueryRowContext(ctx, s.rebind(query), since).Scan(&count)
	return count, err
}

// CountResolvedInWindow counts tickets resolved in the last N hours.
func (s *Store) CountResolvedInWindow(ctx context.Context, hours int) (int, error) {
	since := time.Now().UTC().Add(-time.Duration(hours) * time.Hour)
	var count int
	query := `
		SELECT COUNT(*) FROM tickets
		WHERE status=? AND resolved_at > ?`

	err := s.db.QueryRowContext(ctx, s.rebind(query), StatusResolved, since).Scan(&count)
	return count, err
}

// FindSimilar returns resolved tickets of the same type, most recent first.
// Used to inject historical context into LLM diagnosis.
func (s *Store) FindSimilar(ctx context.Context, ticketType, excludeID string, limit int) ([]*Ticket, error) {
	since := time.Now().UTC().Add(-90 * 24 * time.Hour)
	query := `
		SELECT id, source, alert_name, type, tenant, service, summary, severity, status,
			analysis, proposed_fix, pr_url, pr_number, pr_repo, pr_branch, pr_commit_sha, confidence, alert_fingerprint, created_at, updated_at, resolved_at
		FROM tickets
		WHERE type=? AND status=? AND id != ? AND created_at > ?
		ORDER BY created_at DESC LIMIT ?`

	rows, err := s.db.QueryContext(ctx, s.rebind(query),
		ticketType, StatusResolved, excludeID, since, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck
	return s.scanTickets(rows)
}

// FindRecentlyResolved returns the most recent ticket with the given
// (tenant, service, type, alertName) that was resolved within the window.
// Used to suppress re-firing "flap" alerts that would otherwise create a
// fresh ticket every time Prometheus toggles above/below threshold.
//
// alertName is part of the key because classifyAlert collapses multiple
// distinct Prometheus alert names into shared ticket types (e.g.
// TenantCPUQuotaHigh, TenantMemoryQuotaHigh, and CPUThrottlingHigh all
// map to TypeResourceLimit). Keying only on type would let a recently
// resolved incident suppress an unrelated one firing on the same
// tenant/service. An empty alertName matches tickets whose alert_name
// is blank — e.g. tickets created by the poller or other non-
// AlertManager sources.
func (s *Store) FindRecentlyResolved(ctx context.Context, tenant, service, ticketType, alertName string, window time.Duration) (*Ticket, error) {
	if window <= 0 {
		return nil, nil
	}
	since := time.Now().UTC().Add(-window)
	t := &Ticket{}
	var resolvedAt sql.NullTime
	query := `
		SELECT id, source, alert_name, type, tenant, service, summary, severity, status,
			analysis, proposed_fix, pr_url, pr_number, pr_repo, pr_branch, pr_commit_sha, confidence, alert_fingerprint, created_at, updated_at, resolved_at
		FROM tickets
		WHERE tenant=? AND service=? AND type=? AND alert_name=? AND status=? AND resolved_at > ?
		ORDER BY resolved_at DESC LIMIT 1`

	err := s.db.QueryRowContext(ctx, s.rebind(query),
		tenant, service, ticketType, alertName, StatusResolved, since,
	).Scan(&t.ID, &t.Source, &t.AlertName, &t.Type, &t.Tenant, &t.Service, &t.Summary, &t.Severity, &t.Status,
		&t.Analysis, &t.ProposedFix, &t.PRURL, &t.PRNumber, &t.PRRepo, &t.PRBranch, &t.PRCommitSHA, &t.Confidence, &t.AlertFingerprint,
		&t.CreatedAt, &t.UpdatedAt, &resolvedAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if resolvedAt.Valid {
		t.ResolvedAt = &resolvedAt.Time
	}
	return t, nil
}

// Touch bumps the ticket's UpdatedAt without changing any other field.
// Used on duplicate-alert firings so stale-ticket GC can tell a still-
// firing alert from one that stopped firing.
func (s *Store) Touch(ctx context.Context, id string) error {
	now := time.Now().UTC()
	query := `UPDATE tickets SET updated_at=? WHERE id=?`
	_, err := s.db.ExecContext(ctx, s.rebind(query), now, id)
	return err
}

// TouchWithFingerprint bumps UpdatedAt and merges the supplied alert
// fingerprint into the ticket's fingerprint set. The set is stored as
// a comma-separated string in the alert_fingerprint column.
//
// Tickets are deduplicated by (tenant, service, type) in the
// AlertHandler, so a single ticket can represent multiple concurrent
// AlertManager alerts (e.g. the same alertname firing on two pods of
// the same service). The reconciliation pass only resolves a ticket
// when ALL of its fingerprints are absent from AM's active set, so we
// must accumulate every fingerprint we have seen for the ticket — not
// overwrite with the most recent one.
//
// The merge is performed atomically inside a single UPDATE so that
// concurrent duplicate-alert touches for the same ticket cannot lose
// fingerprints via a read/modify/write race. The CASE expression
// computes the new value using only the existing column value — no
// preceding SELECT is needed.
func (s *Store) TouchWithFingerprint(ctx context.Context, id, fingerprint string) error {
	if fingerprint == "" {
		return s.Touch(ctx, id)
	}
	now := time.Now().UTC()

	// CASE branches:
	//   1. column is empty / NULL                                     → set to fingerprint
	//   2. fingerprint already present (LIKE '%,fp,%' on the padded value) → leave column unchanged
	//   3. otherwise                                                   → append ',fingerprint'
	// LIKE-with-||-padding is supported by both SQLite and PostgreSQL.
	// Only the LIKE operand is escaped: `%`/`_` are wildcards to the pattern
	// but ordinary characters in the value that gets stored and appended.
	query := `
		UPDATE tickets
		SET alert_fingerprint = CASE
			WHEN alert_fingerprint = '' OR alert_fingerprint IS NULL THEN ?
			WHEN ',' || alert_fingerprint || ',' LIKE '%,' || ? || ',%' ESCAPE '\' THEN alert_fingerprint
			ELSE alert_fingerprint || ',' || ?
		END,
		updated_at = ?
		WHERE id = ?`
	_, err := s.db.ExecContext(ctx, s.rebind(query), fingerprint, escapeLike(fingerprint), fingerprint, now, id)
	return err
}

// escapeLike neutralises the LIKE metacharacters in a value that is compared
// as data rather than as a pattern. Fingerprints reach us verbatim from the
// AlertManager webhook body, whose bearer token is optional, so a `%` left
// unescaped would silently widen an exact-membership test into a wildcard.
// The backslash is escaped first, otherwise it would double-escape the
// escapes added after it. Callers must pair this with an `ESCAPE '\'` clause.
func escapeLike(v string) string {
	r := strings.NewReplacer(`\`, `\\`, "%", `\%`, "_", `\_`)
	return r.Replace(v)
}

// mergeFingerprint appends fingerprint to the comma-separated set in
// existing, preserving order and skipping duplicates and empty
// strings. Returns the original set unchanged if fingerprint is empty
// or already present.
//
// Kept around as a Go-side reference implementation that mirrors the
// atomic SQL CASE expression in TouchWithFingerprint and is used by
// unit tests to validate set semantics independent of the database.
func mergeFingerprint(existing, fingerprint string) string {
	if fingerprint == "" {
		return existing
	}
	if existing == "" {
		return fingerprint
	}
	for _, fp := range strings.Split(existing, ",") {
		if strings.TrimSpace(fp) == fingerprint {
			return existing
		}
	}
	return existing + "," + fingerprint
}

// ResolveByID marks a single open ticket as resolved. The UPDATE is
// gated on status=open so concurrent pipeline transitions (a ticket
// moving from open → analyzing / fix_proposed / fix_applied) are not
// silently overwritten by the stale-ticket GC; the resolver reads
// ListOpen and calls ResolveByID, and the pipeline can race between
// those two steps.
//
// Returns true when a row was actually updated, false when the gate
// filtered the write (e.g. the pipeline promoted the ticket first).
// Callers should check the bool before logging a resolution.
func (s *Store) ResolveByID(ctx context.Context, id string) (bool, error) {
	now := time.Now().UTC()
	query := `
		UPDATE tickets SET status=?, resolved_at=?, updated_at=?
		WHERE id=? AND status=?`
	res, err := s.db.ExecContext(ctx, s.rebind(query),
		StatusResolved, now, now,
		id, StatusOpen,
	)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// ResolveByIDFromStatus marks a ticket as resolved when it is in the
// specified fromStatus. Unlike ResolveByID (which gates on StatusOpen),
// this is used by the stale-TTL GC for StatusAnalyzing and
// StatusFixProposed tickets, and it also appends reason to the analysis
// field so operators can distinguish automatic from manual resolutions.
//
// Returns true when a row was actually updated, false when the gate
// filtered the write (ticket was already promoted to another status).
func (s *Store) ResolveByIDFromStatus(ctx context.Context, id, fromStatus, reason string) (bool, error) {
	now := time.Now().UTC()
	query := `
		UPDATE tickets SET
			status=?,
			resolved_at=?,
			updated_at=?,
			analysis = CASE WHEN analysis='' THEN ? ELSE analysis || E'\n' || ? END
		WHERE id=? AND status=?`
	// SQLite does not support E'\n' escape syntax; use literal newline.
	if s.dialect != "postgres" {
		query = `
		UPDATE tickets SET
			status=?,
			resolved_at=?,
			updated_at=?,
			analysis = CASE WHEN analysis='' THEN ? ELSE analysis || char(10) || ? END
		WHERE id=? AND status=?`
	}
	res, err := s.db.ExecContext(ctx, s.rebind(query),
		StatusResolved, now, now,
		reason, reason,
		id, fromStatus,
	)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// ResolveByTenantService resolves open tickets matching tenant+service+type
// and returns the IDs of the rows that were transitioned. The IDs let
// callers fan out the resolution to external systems (e.g. mctl-api's
// `alerts` table) — without them the local SQLite resolve was a dead-end
// and external incident records stayed `open` forever.
//
// The UPDATE uses RETURNING so the set of resolved IDs is gathered
// atomically with the status transition. A SELECT-then-UPDATE pair
// would be racy: a concurrent webhook handling the same
// (tenant, service, type) could insert a new open ticket between the
// two statements; the UPDATE would then close that ticket but its ID
// would never appear in the returned slice, leaving the corresponding
// mctl-api incident `open` forever — the exact drift this propagation
// channel was added to fix. Both modernc.org/sqlite (SQLite 3.45+) and
// pgx support UPDATE…RETURNING with identical syntax.
// fingerprint, when non-empty, additionally requires the ticket to carry
// that AlertManager fingerprint in its accumulated set. (tenant, service,
// type) alone is a coarse key — several alertnames map to one ticket type —
// so a replayed `resolved` webhook could otherwise close a DIFFERENT
// incident that opened under the same key in the meantime. Membership, not
// equality: a ticket that deduped several concurrent alerts holds all of
// their fingerprints, and resolving on any one of them is the pre-existing
// behaviour this must not change. Empty fingerprint keeps the old
// unconditional match, for callers that have none.
//
// notAfter, when non-zero, additionally requires the ticket to predate that
// instant. A fingerprint identifies an alert's LABEL SET, not one occurrence
// of it — the same alert flapping resolved→firing→resolved carries the same
// fingerprint every time. So fingerprint membership alone does not make a
// replayed resolve idempotent: if the alert re-fires and opens a fresh ticket
// before AlertManager redelivers the older batch, that redelivery matches the
// new ticket too. Callers pass the alert's EndsAt: a ticket opened after the
// alert already ended cannot be the occurrence that alert resolves. Clock skew
// between AlertManager and this process biases toward leaving a ticket open,
// which reconcileWithAlertManager then closes on its next pass — the safe
// direction, same argument as the workload-key migration in #105.
func (s *Store) ResolveByTenantService(ctx context.Context, tenant, service, ticketType, fingerprint string, notAfter time.Time) ([]string, error) {
	now := time.Now().UTC()
	query := `
		UPDATE tickets SET status=?, resolved_at=?, updated_at=?
		WHERE tenant=? AND service=? AND type=? AND status NOT IN (?, ?)
		RETURNING id`
	args := []any{
		StatusResolved, now, now,
		tenant, service, ticketType, StatusResolved, StatusSuppressed,
	}
	var conds []string
	if fingerprint != "" {
		// The set is stored comma-separated; wrapping both sides in commas
		// makes this an exact element match rather than a substring one, so
		// "abc" cannot match "abcdef". `||` is the SQL-standard
		// concatenation operator and behaves identically in SQLite and
		// Postgres, the two dialects NewStore supports.
		//
		// ESCAPE, because the fingerprint arrives verbatim in the webhook
		// body and the webhook's bearer token is optional: an unescaped `%`
		// would turn this membership test into a wildcard that matches every
		// ticket under the key, handing an unauthenticated caller the ability
		// to resolve incidents it knows nothing about.
		//
		// The `alert_fingerprint = ''` arm keeps pre-Phase-2 tickets
		// resolvable. Fingerprints have not always been persisted, and an
		// open ticket predating that carries an empty set — a predicate
		// requiring membership would never match it again, and
		// reconcileWithAlertManager deliberately skips exactly those rows
		// (poller.go), so the ticket and its mctl-api incident would stay
		// open until TTL GC. Matching them on (tenant, service, type) alone
		// is the behaviour that shipped before this scope existed; the arm
		// restores the status quo for that finite legacy set without
		// loosening anything for tickets that do carry fingerprints.
		conds = append(conds,
			"(alert_fingerprint = '' OR (',' || alert_fingerprint || ',') LIKE ('%,' || ? || ',%') ESCAPE '\\')")
		args = append(args, escapeLike(fingerprint))
	}
	if !notAfter.IsZero() {
		conds = append(conds, "created_at <= ?")
		args = append(args, notAfter.UTC())
	}
	if len(conds) > 0 {
		query = `
		UPDATE tickets SET status=?, resolved_at=?, updated_at=?
		WHERE tenant=? AND service=? AND type=? AND status NOT IN (?, ?)
		  AND ` + strings.Join(conds, "\n\t\t  AND ") + `
		RETURNING id`
	}

	rows, err := s.db.QueryContext(ctx, s.rebind(query), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return ids, nil
}

// StatusSourcePair is a (status, source) tuple used to key the
// OpenTicketBreakdown result map.
type StatusSourcePair struct {
	Status string
	Source string
}

// OpenTicketBreakdown returns a count of non-terminal tickets grouped by
// (status, source). Terminal statuses (resolved, suppressed) are excluded.
func (s *Store) OpenTicketBreakdown(ctx context.Context) (map[StatusSourcePair]int, error) {
	const q = `
        SELECT status, source, COUNT(*) FROM tickets
        WHERE status NOT IN ('resolved', 'suppressed')
        GROUP BY status, source`
	rows, err := s.db.QueryContext(ctx, s.rebind(q))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[StatusSourcePair]int{}
	for rows.Next() {
		var k StatusSourcePair
		var n int
		if err := rows.Scan(&k.Status, &k.Source, &n); err != nil {
			return nil, err
		}
		out[k] = n
	}
	return out, rows.Err()
}

// Close closes the database.
func (s *Store) Close() error {
	return s.db.Close()
}

// DB returns the underlying database connection for shared use (e.g., skill metrics).
func (s *Store) DB() *sql.DB {
	return s.db
}

// Dialect returns the configured SQL dialect.
func (s *Store) Dialect() string {
	return s.dialect
}

// EvidenceJSON marshals v to JSON for storing as evidence content.
func EvidenceJSON(v interface{}) string {
	b, _ := json.Marshal(v)
	return string(b)
}
