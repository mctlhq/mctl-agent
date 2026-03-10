package ticket

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	_ "modernc.org/sqlite"
)

// Store persists tickets and evidence in SQLite.
type Store struct {
	db *sql.DB
}

// NewStore opens (or creates) the SQLite database at path.
func NewStore(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("opening sqlite: %w", err)
	}
	// WAL mode for concurrent reads.
	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		return nil, fmt.Errorf("setting WAL mode: %w", err)
	}
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		return nil, fmt.Errorf("migrating: %w", err)
	}
	return s, nil
}

func (s *Store) migrate() error {
	_, err := s.db.Exec(`
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

		CREATE INDEX IF NOT EXISTS idx_tickets_status ON tickets(status);
		CREATE INDEX IF NOT EXISTS idx_tickets_tenant_service_type ON tickets(tenant, service, type);
		CREATE INDEX IF NOT EXISTS idx_evidence_ticket ON evidence(ticket_id);
	`)
	return err
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

	_, err := s.db.Exec(`
		INSERT INTO tickets (id, source, type, tenant, service, summary, severity, status,
			analysis, proposed_fix, pr_url, pr_number, confidence, created_at, updated_at, resolved_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		t.ID, t.Source, t.Type, t.Tenant, t.Service, t.Summary, t.Severity, t.Status,
		t.Analysis, t.ProposedFix, t.PRURL, t.PRNumber, t.Confidence,
		t.CreatedAt, t.UpdatedAt, t.ResolvedAt,
	)
	return err
}

// Update saves changes to an existing ticket.
func (s *Store) Update(t *Ticket) error {
	t.UpdatedAt = time.Now().UTC()
	_, err := s.db.Exec(`
		UPDATE tickets SET source=?, type=?, tenant=?, service=?, summary=?, severity=?, status=?,
			analysis=?, proposed_fix=?, pr_url=?, pr_number=?, confidence=?,
			updated_at=?, resolved_at=?
		WHERE id=?`,
		t.Source, t.Type, t.Tenant, t.Service, t.Summary, t.Severity, t.Status,
		t.Analysis, t.ProposedFix, t.PRURL, t.PRNumber, t.Confidence,
		t.UpdatedAt, t.ResolvedAt, t.ID,
	)
	return err
}

// Get retrieves a ticket by ID, including evidence.
func (s *Store) Get(id string) (*Ticket, error) {
	t := &Ticket{}
	var resolvedAt sql.NullTime
	err := s.db.QueryRow(`
		SELECT id, source, type, tenant, service, summary, severity, status,
			analysis, proposed_fix, pr_url, pr_number, confidence, created_at, updated_at, resolved_at
		FROM tickets WHERE id=?`, id,
	).Scan(&t.ID, &t.Source, &t.Type, &t.Tenant, &t.Service, &t.Summary, &t.Severity, &t.Status,
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
	rows, err := s.db.Query(`
		SELECT id, source, type, tenant, service, summary, severity, status,
			analysis, proposed_fix, pr_url, pr_number, confidence, created_at, updated_at, resolved_at
		FROM tickets ORDER BY created_at DESC LIMIT 100`)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck
	return s.scanTickets(rows)
}

func (s *Store) listByStatus(statuses ...string) ([]*Ticket, error) {
	query := `
		SELECT id, source, type, tenant, service, summary, severity, status,
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

	rows, err := s.db.Query(query, args...)
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
		if err := rows.Scan(&t.ID, &t.Source, &t.Type, &t.Tenant, &t.Service, &t.Summary,
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
	err := s.db.QueryRow(`
		SELECT id, source, type, tenant, service, summary, severity, status,
			analysis, proposed_fix, pr_url, pr_number, confidence, created_at, updated_at, resolved_at
		FROM tickets
		WHERE tenant=? AND service=? AND type=? AND status NOT IN (?, ?)
		ORDER BY created_at DESC LIMIT 1`,
		tenant, service, ticketType, StatusResolved, StatusSuppressed,
	).Scan(&t.ID, &t.Source, &t.Type, &t.Tenant, &t.Service, &t.Summary, &t.Severity, &t.Status,
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
	_, err := s.db.Exec(`
		INSERT INTO evidence (ticket_id, type, content, collected_at)
		VALUES (?, ?, ?, ?)`,
		ticketID, ev.Type, ev.Content, ev.CollectedAt,
	)
	return err
}

func (s *Store) loadEvidence(ticketID string) ([]Evidence, error) {
	rows, err := s.db.Query(`
		SELECT type, content, collected_at FROM evidence
		WHERE ticket_id=? ORDER BY collected_at`, ticketID)
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
	err := s.db.QueryRow(`
		SELECT COUNT(*) FROM tickets
		WHERE pr_url != '' AND created_at > ?`, since,
	).Scan(&count)
	return count, err
}

// ResolveByTenantService resolves open tickets matching tenant+service.
func (s *Store) ResolveByTenantService(tenant, service, ticketType string) error {
	now := time.Now().UTC()
	_, err := s.db.Exec(`
		UPDATE tickets SET status=?, resolved_at=?, updated_at=?
		WHERE tenant=? AND service=? AND type=? AND status NOT IN (?, ?)`,
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

// EvidenceJSON marshals v to JSON for storing as evidence content.
func EvidenceJSON(v interface{}) string {
	b, _ := json.Marshal(v)
	return string(b)
}
