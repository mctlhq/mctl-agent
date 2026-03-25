package webhook

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

var ErrAlreadyClaimed = errors.New("event already claimed")
var ErrLeaseExpired = errors.New("lease expired")
var ErrClaimNotFound = errors.New("claim not found")

type Store struct {
	db      *sql.DB
	dialect string
}

func NewStore(db *sql.DB, dialect string) (*Store, error) {
	s := &Store{db: db, dialect: dialect}
	if err := s.migrate(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) rebind(query string) string {
	if s.dialect != "postgres" {
		return query
	}
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
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS webhook_endpoints (
			id TEXT PRIMARY KEY,
			agent_id TEXT NOT NULL UNIQUE,
			url TEXT NOT NULL,
			secret TEXT NOT NULL,
			event_types TEXT NOT NULL DEFAULT '[]',
			active BOOLEAN NOT NULL DEFAULT true,
			created_at DATETIME NOT NULL,
			updated_at DATETIME NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS external_events (
			id TEXT PRIMARY KEY,
			event_type TEXT NOT NULL,
			ticket_id TEXT NOT NULL,
			payload TEXT NOT NULL,
			created_at DATETIME NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS external_deliveries (
			id TEXT PRIMARY KEY,
			event_id TEXT NOT NULL,
			webhook_id TEXT NOT NULL,
			attempt INTEGER NOT NULL DEFAULT 0,
			status TEXT NOT NULL DEFAULT 'pending',
			response_code INTEGER NOT NULL DEFAULT 0,
			response_body_truncated TEXT NOT NULL DEFAULT '',
			last_error TEXT NOT NULL DEFAULT '',
			next_attempt_at DATETIME NOT NULL,
			created_at DATETIME NOT NULL,
			updated_at DATETIME NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS external_claims (
			id TEXT PRIMARY KEY,
			event_id TEXT NOT NULL,
			ticket_id TEXT NOT NULL,
			agent_id TEXT NOT NULL,
			status TEXT NOT NULL DEFAULT 'active',
			lease_expires_at DATETIME NOT NULL,
			result_status TEXT NOT NULL DEFAULT '',
			result_payload TEXT NOT NULL DEFAULT '',
			idempotency_key TEXT NOT NULL DEFAULT '',
			created_at DATETIME NOT NULL,
			completed_at DATETIME
		)`,
		`CREATE INDEX IF NOT EXISTS idx_external_events_ticket ON external_events(ticket_id)`,
		`CREATE INDEX IF NOT EXISTS idx_external_deliveries_status_due ON external_deliveries(status, next_attempt_at)`,
		`CREATE INDEX IF NOT EXISTS idx_external_claims_event ON external_claims(event_id)`,
		`CREATE INDEX IF NOT EXISTS idx_external_claims_ticket ON external_claims(ticket_id)`,
	}
	for _, stmt := range stmts {
		if _, err := s.db.Exec(stmt); err != nil {
			return err
		}
	}
	return nil
}

func normalizeEventTypes(eventTypes []string) ([]string, error) {
	if len(eventTypes) == 0 {
		return nil, fmt.Errorf("event_types must not be empty")
	}
	seen := make(map[string]struct{}, len(eventTypes))
	out := make([]string, 0, len(eventTypes))
	for _, raw := range eventTypes {
		raw = strings.TrimSpace(raw)
		if err := ValidateEventType(raw); err != nil {
			return nil, err
		}
		if _, ok := seen[raw]; ok {
			continue
		}
		seen[raw] = struct{}{}
		out = append(out, raw)
	}
	return out, nil
}

func (s *Store) CreateEndpoint(ep *WebhookEndpoint) error {
	now := time.Now().UTC()
	if ep.ID == "" {
		ep.ID = uuid.New().String()
	}
	eventTypes, err := normalizeEventTypes(ep.EventTypes)
	if err != nil {
		return err
	}
	ep.EventTypes = eventTypes
	ep.CreatedAt = now
	ep.UpdatedAt = now
	if !ep.Active {
		ep.Active = true
	}
	eventJSON, _ := json.Marshal(ep.EventTypes)
	_, err = s.db.Exec(s.rebind(`INSERT INTO webhook_endpoints (id, agent_id, url, secret, event_types, active, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`),
		ep.ID, ep.AgentID, ep.URL, ep.Secret, string(eventJSON), ep.Active, ep.CreatedAt, ep.UpdatedAt,
	)
	return err
}

func (s *Store) ListEndpoints() ([]WebhookEndpoint, error) {
	rows, err := s.db.Query(s.rebind(`SELECT id, agent_id, url, secret, event_types, active, created_at, updated_at FROM webhook_endpoints ORDER BY created_at`))
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck
	var out []WebhookEndpoint
	for rows.Next() {
		var ep WebhookEndpoint
		var eventJSON string
		if err := rows.Scan(&ep.ID, &ep.AgentID, &ep.URL, &ep.Secret, &eventJSON, &ep.Active, &ep.CreatedAt, &ep.UpdatedAt); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(eventJSON), &ep.EventTypes)
		out = append(out, ep)
	}
	return out, rows.Err()
}

func (s *Store) DeleteEndpoint(id string) error {
	_, err := s.db.Exec(s.rebind(`DELETE FROM webhook_endpoints WHERE id=?`), id)
	return err
}

func (s *Store) GetEndpointByAgentID(agentID string) (*WebhookEndpoint, error) {
	var ep WebhookEndpoint
	var eventJSON string
	err := s.db.QueryRow(s.rebind(`SELECT id, agent_id, url, secret, event_types, active, created_at, updated_at FROM webhook_endpoints WHERE agent_id=? AND active=true`), agentID).
		Scan(&ep.ID, &ep.AgentID, &ep.URL, &ep.Secret, &eventJSON, &ep.Active, &ep.CreatedAt, &ep.UpdatedAt)
	if err != nil {
		return nil, err
	}
	_ = json.Unmarshal([]byte(eventJSON), &ep.EventTypes)
	return &ep, nil
}

func (s *Store) ListEndpointsForEvent(eventType EventType) ([]WebhookEndpoint, error) {
	endpoints, err := s.ListEndpoints()
	if err != nil {
		return nil, err
	}
	var out []WebhookEndpoint
	for _, ep := range endpoints {
		if !ep.Active {
			continue
		}
		for _, ev := range ep.EventTypes {
			if ev == string(eventType) {
				out = append(out, ep)
				break
			}
		}
	}
	return out, nil
}

func (s *Store) SaveEvent(ev *Event, ticketID string) error {
	payload, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(s.rebind(`INSERT INTO external_events (id, event_type, ticket_id, payload, created_at) VALUES (?, ?, ?, ?, ?)`),
		ev.ID, string(ev.Type), ticketID, string(payload), ev.OccurredAt,
	)
	return err
}

func (s *Store) GetEvent(eventID string) (*Event, error) {
	var payload string
	err := s.db.QueryRow(s.rebind(`SELECT payload FROM external_events WHERE id=?`), eventID).Scan(&payload)
	if err != nil {
		return nil, err
	}
	var ev Event
	if err := json.Unmarshal([]byte(payload), &ev); err != nil {
		return nil, err
	}
	return &ev, nil
}

func (s *Store) CreateDeliveries(eventID string, endpoints []WebhookEndpoint, now time.Time) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	for _, ep := range endpoints {
		_, err := tx.Exec(s.rebind(`INSERT INTO external_deliveries (id, event_id, webhook_id, attempt, status, response_code, response_body_truncated, last_error, next_attempt_at, created_at, updated_at)
			VALUES (?, ?, ?, 0, ?, 0, '', '', ?, ?, ?)`),
			uuid.New().String(), eventID, ep.ID, DeliveryPending, now, now, now,
		)
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) listPendingDeliveries(now time.Time, limit int) ([]ExternalDelivery, error) {
	rows, err := s.db.Query(s.rebind(`SELECT id, event_id, webhook_id, attempt, status, response_code, response_body_truncated, last_error, next_attempt_at, created_at, updated_at
		FROM external_deliveries
		WHERE status IN (?, ?) AND next_attempt_at <= ?
		ORDER BY next_attempt_at ASC LIMIT ?`), DeliveryPending, DeliveryFailed, now, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck
	var out []ExternalDelivery
	for rows.Next() {
		var d ExternalDelivery
		if err := rows.Scan(&d.ID, &d.EventID, &d.WebhookID, &d.Attempt, &d.Status, &d.ResponseCode, &d.ResponseBodyTruncated, &d.LastError, &d.NextAttemptAt, &d.CreatedAt, &d.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func (s *Store) getEndpointByID(id string) (*WebhookEndpoint, error) {
	var ep WebhookEndpoint
	var eventJSON string
	err := s.db.QueryRow(s.rebind(`SELECT id, agent_id, url, secret, event_types, active, created_at, updated_at FROM webhook_endpoints WHERE id=?`), id).
		Scan(&ep.ID, &ep.AgentID, &ep.URL, &ep.Secret, &eventJSON, &ep.Active, &ep.CreatedAt, &ep.UpdatedAt)
	if err != nil {
		return nil, err
	}
	_ = json.Unmarshal([]byte(eventJSON), &ep.EventTypes)
	return &ep, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func (s *Store) MarkDeliveryDelivered(id string, attempt int, code int, body string, now time.Time) error {
	_, err := s.db.Exec(s.rebind(`UPDATE external_deliveries
		SET attempt=?, status=?, response_code=?, response_body_truncated=?, last_error='', updated_at=?
		WHERE id=?`), attempt, DeliveryDelivered, code, truncate(body, 1024), now, id)
	return err
}

func (s *Store) MarkDeliveryFailed(id string, attempt int, code int, body, lastError string, nextAttempt time.Time, dead bool) error {
	status := DeliveryFailed
	if dead {
		status = DeliveryDead
	}
	_, err := s.db.Exec(s.rebind(`UPDATE external_deliveries
		SET attempt=?, status=?, response_code=?, response_body_truncated=?, last_error=?, next_attempt_at=?, updated_at=?
		WHERE id=?`), attempt, status, code, truncate(body, 1024), truncate(lastError, 1024), nextAttempt, time.Now().UTC(), id)
	return err
}

func (s *Store) CreateClaim(ticketID string, req ClaimRequest, leaseTTL time.Duration) (*ClaimResponse, error) {
	now := time.Now().UTC()
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() //nolint:errcheck

	var existing string
	var expires time.Time
	err = tx.QueryRow(s.rebind(`SELECT id, lease_expires_at FROM external_claims
		WHERE event_id=? AND status=? AND lease_expires_at > ?
		ORDER BY created_at DESC LIMIT 1`), req.EventID, ClaimActive, now).Scan(&existing, &expires)
	if err != nil && err != sql.ErrNoRows {
		return nil, err
	}
	if err == nil {
		return nil, ErrAlreadyClaimed
	}

	leaseID := uuid.New().String()
	expiresAt := now.Add(leaseTTL)
	_, err = tx.Exec(s.rebind(`INSERT INTO external_claims (id, event_id, ticket_id, agent_id, status, lease_expires_at, result_status, result_payload, idempotency_key, created_at)
		VALUES (?, ?, ?, ?, ?, ?, '', '', '', ?)`), leaseID, req.EventID, ticketID, req.AgentID, ClaimActive, expiresAt, now)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &ClaimResponse{LeaseID: leaseID, TicketID: ticketID, EventID: req.EventID, ExpiresAt: expiresAt}, nil
}

func (s *Store) GetActiveClaim(leaseID, eventID, agentID string) (*ExternalClaim, error) {
	c, err := s.GetClaim(leaseID, eventID, agentID)
	if err != nil {
		return nil, err
	}
	if c.Status == ClaimCompleted && c.IdempotencyKey != "" {
		return c, nil
	}
	if c.Status != ClaimActive {
		return nil, ErrClaimNotFound
	}
	if time.Now().UTC().After(c.LeaseExpiresAt) {
		return nil, ErrLeaseExpired
	}
	return c, nil
}

func (s *Store) GetClaim(leaseID, eventID, agentID string) (*ExternalClaim, error) {
	var c ExternalClaim
	var completed sql.NullTime
	err := s.db.QueryRow(s.rebind(`SELECT id, event_id, ticket_id, agent_id, status, lease_expires_at, result_status, result_payload, idempotency_key, created_at, completed_at
		FROM external_claims WHERE id=? AND event_id=? AND agent_id=?`), leaseID, eventID, agentID).
		Scan(&c.ID, &c.EventID, &c.TicketID, &c.AgentID, &c.Status, &c.LeaseExpiresAt, &c.ResultStatus, &c.ResultPayload, &c.IdempotencyKey, &c.CreatedAt, &completed)
	if err != nil {
		return nil, err
	}
	if completed.Valid {
		c.CompletedAt = &completed.Time
	}
	return &c, nil
}

func (s *Store) CompleteClaim(leaseID string, result ExternalResult) error {
	payload, err := json.Marshal(result)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	_, err = s.db.Exec(s.rebind(`UPDATE external_claims
		SET status=?, result_status=?, result_payload=?, idempotency_key=?, completed_at=?
		WHERE id=?`), ClaimCompleted, result.Status, string(payload), result.IdempotencyKey, now, leaseID)
	return err
}

func (s *Store) ExpireStaleLeases(now time.Time) error {
	_, err := s.db.Exec(s.rebind(`UPDATE external_claims SET status=? WHERE status=? AND lease_expires_at <= ?`), ClaimExpired, ClaimActive, now)
	return err
}

func (s *Store) DispatchBatch(now time.Time, limit int) ([]ExternalDelivery, error) {
	return s.listPendingDeliveries(now, limit)
}
