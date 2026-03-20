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

package skill

import (
	"database/sql"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// Metrics tracks skill execution statistics and provides circuit breaker functionality.
type Metrics struct {
	db               *sql.DB
	dialect          string
	mu               sync.RWMutex
	circuitThreshold float64 // Minimum success rate before auto-disable.
	circuitWindow    int     // Number of recent executions to evaluate.
}

// MetricsSnapshot contains the current stats for a skill.
type MetricsSnapshot struct {
	SkillName      string  `json:"skill_name"`
	TotalMatches   int     `json:"total_matches"`
	TotalDiagnoses int     `json:"total_diagnoses"`
	TotalFixes     int     `json:"total_fixes"`
	Successes      int     `json:"successes"`
	Failures       int     `json:"failures"`
	SuccessRate    float64 `json:"success_rate"`
	AvgDurationMs  int64   `json:"avg_duration_ms"`
	LastExecutedAt string  `json:"last_executed_at,omitempty"`
	CircuitOpen    bool    `json:"circuit_open"`
}

// NewMetrics creates a Metrics tracker with the given database.
func NewMetrics(db *sql.DB, circuitThreshold float64, circuitWindow int) (*Metrics, error) {
	// Detect dialect from DB connection (best effort)
	dialect := "sqlite"
	// Check if db is a postgres connection (lib/pq)
	if fmt.Sprintf("%T", db.Driver()) == "*pq.Driver" {
		dialect = "postgres"
	}

	m := &Metrics{
		db:               db,
		dialect:          dialect,
		circuitThreshold: circuitThreshold,
		circuitWindow:    circuitWindow,
	}
	if err := m.migrate(); err != nil {
		return nil, fmt.Errorf("migrating skill_metrics: %w", err)
	}
	return m, nil
}

func (m *Metrics) rebind(query string) string {
	if m.dialect != "postgres" {
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

func (m *Metrics) migrate() error {
	var err error
	if m.dialect == "postgres" {
		_, err = m.db.Exec(`
			CREATE TABLE IF NOT EXISTS skill_executions (
				id           SERIAL PRIMARY KEY,
				skill_name   TEXT NOT NULL,
				ticket_id    TEXT NOT NULL,
				event        TEXT NOT NULL,
				success      BOOLEAN NOT NULL DEFAULT FALSE,
				duration_ms  INTEGER NOT NULL DEFAULT 0,
				detail       TEXT NOT NULL DEFAULT '',
				created_at   TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
			);
		`)
	} else {
		_, err = m.db.Exec(`
			CREATE TABLE IF NOT EXISTS skill_executions (
				id           INTEGER PRIMARY KEY AUTOINCREMENT,
				skill_name   TEXT NOT NULL,
				ticket_id    TEXT NOT NULL,
				event        TEXT NOT NULL,
				success      BOOLEAN NOT NULL DEFAULT 0,
				duration_ms  INTEGER NOT NULL DEFAULT 0,
				detail       TEXT NOT NULL DEFAULT '',
				created_at   DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
			);
		`)
	}
	if err != nil {
		return err
	}

	_, err = m.db.Exec(`
		CREATE INDEX IF NOT EXISTS idx_skill_exec_name ON skill_executions(skill_name);
		CREATE INDEX IF NOT EXISTS idx_skill_exec_created ON skill_executions(created_at);
	`)
	return err
}

// RecordMatch records that a skill matched a ticket.
func (m *Metrics) RecordMatch(skillName, ticketID string) {
	m.record(skillName, ticketID, "match", true, 0, "")
}

// RecordDiagnosis records a diagnosis attempt.
func (m *Metrics) RecordDiagnosis(skillName, ticketID string, success bool, duration time.Duration, detail string) {
	m.record(skillName, ticketID, "diagnose", success, duration.Milliseconds(), detail)
}

// RecordFix records a fix attempt.
func (m *Metrics) RecordFix(skillName, ticketID string, success bool, duration time.Duration, detail string) {
	m.record(skillName, ticketID, "fix", success, duration.Milliseconds(), detail)
}

// RecordResolution records whether the ticket was resolved after the fix.
func (m *Metrics) RecordResolution(skillName, ticketID string, resolved bool) {
	detail := "resolved"
	if !resolved {
		detail = "unresolved"
	}
	m.record(skillName, ticketID, "resolution", resolved, 0, detail)
}

func (m *Metrics) record(skillName, ticketID, event string, success bool, durationMs int64, detail string) {
	query := `
		INSERT INTO skill_executions (skill_name, ticket_id, event, success, duration_ms, detail)
		VALUES (?, ?, ?, ?, ?, ?)`

	_, err := m.db.Exec(m.rebind(query),
		skillName, ticketID, event, success, durationMs, detail,
	)
	if err != nil {
		slog.Error("failed to record skill metric", "skill", skillName, "event", event, "error", err)
	}
}

// GetSnapshot returns aggregated metrics for a skill.
func (m *Metrics) GetSnapshot(skillName string) MetricsSnapshot {
	snap := MetricsSnapshot{SkillName: skillName}

	_ = m.db.QueryRow(m.rebind(`SELECT COUNT(*) FROM skill_executions WHERE skill_name=? AND event='match'`),
		skillName).Scan(&snap.TotalMatches)

	_ = m.db.QueryRow(m.rebind(`SELECT COUNT(*) FROM skill_executions WHERE skill_name=? AND event='diagnose'`),
		skillName).Scan(&snap.TotalDiagnoses)

	_ = m.db.QueryRow(m.rebind(`SELECT COUNT(*) FROM skill_executions WHERE skill_name=? AND event='fix'`),
		skillName).Scan(&snap.TotalFixes)

	// In Postgres success=1 might not work if it is a real BOOLEAN, better use boolean literals or adapt per dialect
	successCond := "success=1"
	failureCond := "success=0"
	if m.dialect == "postgres" {
		successCond = "success=TRUE"
		failureCond = "success=FALSE"
	}

	_ = m.db.QueryRow(m.rebind(fmt.Sprintf(`SELECT COUNT(*) FROM skill_executions WHERE skill_name=? AND event='resolution' AND %s`, successCond)),
		skillName).Scan(&snap.Successes)

	_ = m.db.QueryRow(m.rebind(fmt.Sprintf(`SELECT COUNT(*) FROM skill_executions WHERE skill_name=? AND event='resolution' AND %s`, failureCond)),
		skillName).Scan(&snap.Failures)

	total := snap.Successes + snap.Failures
	if total > 0 {
		snap.SuccessRate = float64(snap.Successes) / float64(total)
	}

	_ = m.db.QueryRow(m.rebind(`SELECT COALESCE(AVG(duration_ms), 0) FROM skill_executions WHERE skill_name=? AND event='diagnose' AND duration_ms > 0`),
		skillName).Scan(&snap.AvgDurationMs)

	var lastAt sql.NullString
	_ = m.db.QueryRow(m.rebind(`SELECT MAX(created_at) FROM skill_executions WHERE skill_name=?`), skillName).Scan(&lastAt)
	if lastAt.Valid {
		snap.LastExecutedAt = lastAt.String
	}

	snap.CircuitOpen = m.IsCircuitOpen(skillName)

	return snap
}

// GetAllSnapshots returns metrics for all skills that have execution records.
func (m *Metrics) GetAllSnapshots() []MetricsSnapshot {
	rows, err := m.db.Query(`SELECT DISTINCT skill_name FROM skill_executions ORDER BY skill_name`)
	if err != nil {
		return nil
	}
	defer rows.Close() //nolint:errcheck

	var snapshots []MetricsSnapshot
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			continue
		}
		snapshots = append(snapshots, m.GetSnapshot(name))
	}
	return snapshots
}

// IsCircuitOpen checks if a skill's circuit breaker is tripped
// (success rate below threshold in the last N resolutions).
func (m *Metrics) IsCircuitOpen(skillName string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()

	successCond := "success"
	if m.dialect != "postgres" {
		successCond = "success=1"
	}

	var successes, total int
	query := fmt.Sprintf(`
		SELECT 
			COALESCE(SUM(CASE WHEN %s THEN 1 ELSE 0 END), 0),
			COUNT(*)
		FROM (
			SELECT success FROM skill_executions
			WHERE skill_name=? AND event='resolution'
			ORDER BY created_at DESC LIMIT ?
		) sub`, successCond)

	_ = m.db.QueryRow(m.rebind(query),
		skillName, m.circuitWindow,
	).Scan(&successes, &total)

	if total < m.circuitWindow {
		return false // Not enough data to trip circuit.
	}

	rate := float64(successes) / float64(total)
	return rate < m.circuitThreshold
}

// CheckAndDisable checks circuit breakers for all skills and disables tripped ones.
// Returns the list of skills that were auto-disabled.
func (m *Metrics) CheckAndDisable(registry *Registry) []string {
	var disabled []string
	for _, info := range registry.List() {
		if !info.Enabled {
			continue
		}
		if m.IsCircuitOpen(info.Name) {
			registry.Disable(info.Name)
			disabled = append(disabled, info.Name)
			slog.Warn("circuit breaker tripped — skill auto-disabled",
				"skill", info.Name,
				"threshold", m.circuitThreshold,
				"window", m.circuitWindow)
		}
	}
	return disabled
}
