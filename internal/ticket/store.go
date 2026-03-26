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
	"database/sql"
	"encoding/json"
	"fmt"
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
func NewStore(connStr string) (*Store, error) {
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
		_, _ = db.Exec("PRAGMA journal_mode=WAL")
	}

	s := &Store{db: db, dialect: driver}
	if err := s.migrate(); err != nil {
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

func (s *Store) migrate() error {
	var err error
	if s.dialect == "postgres" {
		_, err = s.db.Exec(`
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
		_, err = s.db.Exec(`
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

	if err := s.ensureColumn("tickets", "alert_name", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}

	_, err = s.db.Exec(`
		CREATE INDEX IF NOT EXISTS idx_tickets_status ON tickets(status);
		CREATE INDEX IF NOT EXISTS idx_tickets_tenant_service_type ON tickets(tenant, service, type);
		CREATE INDEX IF NOT EXISTS idx_evidence_ticket ON evidence(ticket_id);
	`)
	return err
}

func (s *Store) ensureColumn(table, column, definition string) error {
	query := fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", table, column, definition)
	if s.dialect == "postgres" {
		query = fmt.Sprintf("ALTER TABLE %s ADD COLUMN IF NOT EXISTS %s %s", table, column, definition)
	}
	if _, err := s.db.Exec(query); err != nil {
		if s.dialect != "postgres" && strings.Contains(strings.ToLower(err.Error()), "duplicate column") {
			return nil
		}
		return err
	}
	return nil
}

// Create inserts a new ticket, generating a UUID.
func (s *Store) Create(t *Ticket) error {
	t.ID = uuid.New().String()
	now := time.Now().UTC()
	t.CreatedAt = now
	t.UpdatedAt = now
	if t.Status == "" {
		t.Status = StatusOpen
	}

	query := `
		INSERT INTO tickets (id, source, alert_name, type, tenant, service, summary, severity, status,
			analysis, proposed_fix, pr_url, pr_number, confidence, created_at, updated_at, resolved_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

	_, err := s.db.Exec(s.rebind(query),
		t.ID, t.Source, t.AlertName, t.Type, t.Tenant, t.Service, t.Summary, t.Severity, t.Status,
		t.Analysis, t.ProposedFix, t.PRURL, t.PRNumber, t.Confidence,
		t.CreatedAt, t.UpdatedAt, t.ResolvedAt,
	)
	return err
}

// Update saves changes to an existing ticket.
func (s *Store) Update(t *Ticket) error {
	t.UpdatedAt = time.Now().UTC()
	query := `
		UPDATE tickets SET source=?, alert_name=?, type=?, tenant=?, service=?, summary=?, severity=?, status=?,
			analysis=?, proposed_fix=?, pr_url=?, pr_number=?, confidence=?,
			updated_at=?, resolved_at=?
		WHERE id=?`

	_, err := s.db.Exec(s.rebind(query),
		t.Source, t.AlertName, t.Type, t.Tenant, t.Service, t.Summary, t.Severity, t.Status,
		t.Analysis, t.ProposedFix, t.PRURL, t.PRNumber, t.Confidence,
		t.UpdatedAt, t.ResolvedAt, t.ID,
	)
	return err
}

// Get retrieves a ticket by ID, including evidence.
func (s *Store) Get(id string) (*Ticket, error) {
	t := &Ticket{}
	var resolvedAt sql.NullTime
	query := `
		SELECT id, source, alert_name, type, tenant, service, summary, severity, status,
			analysis, proposed_fix, pr_url, pr_number, confidence, created_at, updated_at, resolved_at
		FROM tickets WHERE id=?`

	err := s.db.QueryRow(s.rebind(query), id).Scan(&t.ID, &t.Source, &t.AlertName, &t.Type, &t.Tenant, &t.Service, &t.Summary, &t.Severity, &t.Status,
		&t.Analysis, &t.ProposedFix, &t.PRURL, &t.PRNumber, &t.Confidence,
		&t.CreatedAt, &t.UpdatedAt, &resolvedAt,
	)
	if err != nil {
		return nil, err
	}
	if resolvedAt.Valid {
		t.ResolvedAt = &resolvedAt.Time
	}

	t.Evidence, err = s.loadEvidence(id)
	if err != nil {
		return nil, err
	}
	return t, nil
}

// ListOpen returns all non-resolved, non-suppressed tickets.
func (s *Store) ListOpen() ([]*Ticket, error) {
	return s.listByStatus(StatusOpen, StatusAnalyzing, StatusFixProposed, StatusFixApplied)
}

// ListAll returns all tickets (latest first, limit 100).
func (s *Store) ListAll() ([]*Ticket, error) {
	query := `
		SELECT id, source, alert_name, type, tenant, service, summary, severity, status,
			analysis, proposed_fix, pr_url, pr_number, confidence, created_at, updated_at, resolved_at
		FROM tickets ORDER BY created_at DESC LIMIT 100`

	rows, err := s.db.Query(s.rebind(query))
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck
	return s.scanTickets(rows)
}

func (s *Store) listByStatus(statuses ...string) ([]*Ticket, error) {
	query := `
		SELECT id, source, alert_name, type, tenant, service, summary, severity, status,
			analysis, proposed_fix, pr_url, pr_number, confidence, created_at, updated_at, resolved_at
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

	rows, err := s.db.Query(s.rebind(query), args...)
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
			&t.Severity, &t.Status, &t.Analysis, &t.ProposedFix, &t.PRURL, &t.PRNumber,
			&t.Confidence, &t.CreatedAt, &t.UpdatedAt, &resolvedAt); err != nil {
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
func (s *Store) FindDuplicate(tenant, service, ticketType string) (*Ticket, error) {
	t := &Ticket{}
	var resolvedAt sql.NullTime
	query := `
		SELECT id, source, alert_name, type, tenant, service, summary, severity, status,
			analysis, proposed_fix, pr_url, pr_number, confidence, created_at, updated_at, resolved_at
		FROM tickets
		WHERE tenant=? AND service=? AND type=? AND status NOT IN (?, ?)
		ORDER BY created_at DESC LIMIT 1`

	err := s.db.QueryRow(s.rebind(query),
		tenant, service, ticketType, StatusResolved, StatusSuppressed,
	).Scan(&t.ID, &t.Source, &t.AlertName, &t.Type, &t.Tenant, &t.Service, &t.Summary, &t.Severity, &t.Status,
		&t.Analysis, &t.ProposedFix, &t.PRURL, &t.PRNumber, &t.Confidence,
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
func (s *Store) AddEvidence(ticketID string, ev Evidence) error {
	query := `
		INSERT INTO evidence (ticket_id, type, content, collected_at)
		VALUES (?, ?, ?, ?)`

	_, err := s.db.Exec(s.rebind(query),
		ticketID, ev.Type, ev.Content, ev.CollectedAt,
	)
	return err
}

func (s *Store) loadEvidence(ticketID string) ([]Evidence, error) {
	query := `
		SELECT type, content, collected_at FROM evidence
		WHERE ticket_id=? ORDER BY collected_at`

	rows, err := s.db.Query(s.rebind(query), ticketID)
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
func (s *Store) CountPRsInWindow(hours int) (int, error) {
	since := time.Now().UTC().Add(-time.Duration(hours) * time.Hour)
	var count int
	query := `
		SELECT COUNT(*) FROM tickets
		WHERE pr_url != '' AND created_at > ?`

	err := s.db.QueryRow(s.rebind(query), since).Scan(&count)
	return count, err
}

// CountResolvedInWindow counts tickets resolved in the last N hours.
func (s *Store) CountResolvedInWindow(hours int) (int, error) {
	since := time.Now().UTC().Add(-time.Duration(hours) * time.Hour)
	var count int
	query := `
		SELECT COUNT(*) FROM tickets
		WHERE status=? AND resolved_at > ?`

	err := s.db.QueryRow(s.rebind(query), StatusResolved, since).Scan(&count)
	return count, err
}

// FindSimilar returns resolved tickets of the same type, most recent first.
// Used to inject historical context into LLM diagnosis.
func (s *Store) FindSimilar(ticketType, excludeID string, limit int) ([]*Ticket, error) {
	since := time.Now().UTC().Add(-90 * 24 * time.Hour)
	query := `
		SELECT id, source, alert_name, type, tenant, service, summary, severity, status,
			analysis, proposed_fix, pr_url, pr_number, confidence, created_at, updated_at, resolved_at
		FROM tickets
		WHERE type=? AND status=? AND id != ? AND created_at > ?
		ORDER BY created_at DESC LIMIT ?`

	rows, err := s.db.Query(s.rebind(query),
		ticketType, StatusResolved, excludeID, since, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck
	return s.scanTickets(rows)
}

// ResolveByTenantService resolves open tickets matching tenant+service.
func (s *Store) ResolveByTenantService(tenant, service, ticketType string) error {
	now := time.Now().UTC()
	query := `
		UPDATE tickets SET status=?, resolved_at=?, updated_at=?
		WHERE tenant=? AND service=? AND type=? AND status NOT IN (?, ?)`

	_, err := s.db.Exec(s.rebind(query),
		StatusResolved, now, now,
		tenant, service, ticketType, StatusResolved, StatusSuppressed,
	)
	return err
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
