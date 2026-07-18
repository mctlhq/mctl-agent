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

package optimizer

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Recommendation lifecycle statuses. Everything before the terminal set
// (success/regression/rolled_back/closed/superseded) counts as "open" for
// the one-recommendation-per-workload rule.
const (
	RecStatusDryRun     = "dry_run"
	RecStatusPROpen     = "pr_open"
	RecStatusMerged     = "merged"
	RecStatusEvaluating = "evaluating"
	RecStatusSuccess    = "success"
	RecStatusRegression = "regression"
	RecStatusRolledBack = "rolled_back"
	RecStatusClosed     = "closed"
	RecStatusSuperseded = "superseded"
)

// Run statuses (post-merge evaluation lifecycle).
const (
	RunStatusWaitingMerge = "waiting_merge"
	RunStatusWarmup       = "warmup"
	RunStatusEvaluating   = "evaluating"
	RunStatusDone         = "done"
)

// Verdicts.
const (
	VerdictSuccess    = "SUCCESS"
	VerdictRegression = "REGRESSION"
)

// Rollup is one hour of observed usage for a service's main container,
// aggregated across pods (per-pod values, worst pod wins).
type Rollup struct {
	Tenant        string
	Service       string
	HourStart     time.Time
	CPUP95m       float64 // millicores
	CPUP99m       float64
	CPUMaxm       float64
	MemP95        float64 // bytes
	MemP99        float64
	MemMax        float64
	ThrottleRatio float64 // 0..1 for the hour
	Restarts      float64
	OOMSeen       bool
	Replicas      float64
	Samples       int
	ImageTag      string
}

// Recommendation is a proposed (or applied) resource-request change.
type Recommendation struct {
	ID            string
	Tenant        string
	Service       string
	WindowStart   time.Time
	WindowEnd     time.Time
	DaysOfData    float64
	Confidence    string // HIGH|MEDIUM
	Risk          string // LOW|MEDIUM|HIGH
	OldCPURequest string
	OldMemRequest string
	NewCPURequest string
	NewMemRequest string
	EvidenceJSON  string
	Status        string
	Branch        string
	PRNumber      int
	PRURL         string
	MergeSHA      string
	MergedAt      *time.Time
	RollbackPRNum int
	RollbackPRURL string
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// Run tracks the post-merge evaluation of one recommendation.
type Run struct {
	ID               string
	RecommendationID string
	BaselineStart    time.Time
	BaselineEnd      time.Time
	MergedAt         *time.Time
	WarmupUntil      *time.Time
	EvalStart        *time.Time
	EvalEnd          *time.Time
	Status           string
	Verdict          string
	VerdictJSON      string
	CommentPosted    bool
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

// Store persists optimizer rollups, recommendations, and evaluation runs.
// It shares the agent's database (sqlite or postgres) via ticket.Store.DB().
type Store struct {
	db      *sql.DB
	dialect string
}

// NewStore runs migrations on the shared database handle.
func NewStore(db *sql.DB, dialect string) (*Store, error) {
	s := &Store{db: db, dialect: dialect}
	if err := s.migrate(); err != nil {
		return nil, fmt.Errorf("optimizer migrate: %w", err)
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
	serial := "INTEGER PRIMARY KEY AUTOINCREMENT"
	ts := "DATETIME"
	if s.dialect == "postgres" {
		serial = "SERIAL PRIMARY KEY"
		ts = "TIMESTAMPTZ"
	}

	stmts := []string{
		fmt.Sprintf(`
			CREATE TABLE IF NOT EXISTS optimizer_rollups (
				id             %s,
				tenant         TEXT NOT NULL,
				service        TEXT NOT NULL,
				hour_start     %s NOT NULL,
				cpu_p95_m      REAL NOT NULL DEFAULT 0,
				cpu_p99_m      REAL NOT NULL DEFAULT 0,
				cpu_max_m      REAL NOT NULL DEFAULT 0,
				mem_p95        REAL NOT NULL DEFAULT 0,
				mem_p99        REAL NOT NULL DEFAULT 0,
				mem_max        REAL NOT NULL DEFAULT 0,
				throttle_ratio REAL NOT NULL DEFAULT 0,
				restarts       REAL NOT NULL DEFAULT 0,
				oom_seen       INTEGER NOT NULL DEFAULT 0,
				replicas       REAL NOT NULL DEFAULT 0,
				samples        INTEGER NOT NULL DEFAULT 0,
				image_tag      TEXT NOT NULL DEFAULT '',
				created_at     %s NOT NULL,
				UNIQUE (tenant, service, hour_start)
			)`, serial, ts, ts),
		fmt.Sprintf(`
			CREATE TABLE IF NOT EXISTS optimizer_recommendations (
				id               TEXT PRIMARY KEY,
				tenant           TEXT NOT NULL,
				service          TEXT NOT NULL,
				window_start     %s NOT NULL,
				window_end       %s NOT NULL,
				days_of_data     REAL NOT NULL DEFAULT 0,
				confidence       TEXT NOT NULL DEFAULT '',
				risk             TEXT NOT NULL DEFAULT '',
				old_cpu_request  TEXT NOT NULL DEFAULT '',
				old_mem_request  TEXT NOT NULL DEFAULT '',
				new_cpu_request  TEXT NOT NULL DEFAULT '',
				new_mem_request  TEXT NOT NULL DEFAULT '',
				evidence_json    TEXT NOT NULL DEFAULT '',
				status           TEXT NOT NULL DEFAULT '',
				branch           TEXT NOT NULL DEFAULT '',
				pr_number        INTEGER NOT NULL DEFAULT 0,
				pr_url           TEXT NOT NULL DEFAULT '',
				merge_sha        TEXT NOT NULL DEFAULT '',
				merged_at        %s,
				rollback_pr_number INTEGER NOT NULL DEFAULT 0,
				rollback_pr_url    TEXT NOT NULL DEFAULT '',
				created_at       %s NOT NULL,
				updated_at       %s NOT NULL
			)`, ts, ts, ts, ts, ts),
		fmt.Sprintf(`
			CREATE TABLE IF NOT EXISTS optimizer_runs (
				id                TEXT PRIMARY KEY,
				recommendation_id TEXT NOT NULL REFERENCES optimizer_recommendations(id),
				baseline_start    %s NOT NULL,
				baseline_end      %s NOT NULL,
				merged_at         %s,
				warmup_until      %s,
				eval_start        %s,
				eval_end          %s,
				status            TEXT NOT NULL DEFAULT '',
				verdict           TEXT NOT NULL DEFAULT '',
				verdict_json      TEXT NOT NULL DEFAULT '',
				comment_posted    INTEGER NOT NULL DEFAULT 0,
				created_at        %s NOT NULL,
				updated_at        %s NOT NULL
			)`, ts, ts, ts, ts, ts, ts, ts, ts),
		`CREATE INDEX IF NOT EXISTS idx_opt_rollups_svc_hour ON optimizer_rollups(tenant, service, hour_start)`,
		`CREATE INDEX IF NOT EXISTS idx_opt_recs_svc_status ON optimizer_recommendations(tenant, service, status)`,
		`CREATE INDEX IF NOT EXISTS idx_opt_runs_status ON optimizer_runs(status)`,
	}
	for _, stmt := range stmts {
		if _, err := s.db.Exec(stmt); err != nil {
			return err
		}
	}
	return nil
}

// UpsertRollup inserts or replaces the rollup for its (tenant, service, hour).
func (s *Store) UpsertRollup(r *Rollup) error {
	oom := 0
	if r.OOMSeen {
		oom = 1
	}
	query := `
		INSERT INTO optimizer_rollups (tenant, service, hour_start,
			cpu_p95_m, cpu_p99_m, cpu_max_m, mem_p95, mem_p99, mem_max,
			throttle_ratio, restarts, oom_seen, replicas, samples, image_tag, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (tenant, service, hour_start) DO UPDATE SET
			cpu_p95_m=excluded.cpu_p95_m, cpu_p99_m=excluded.cpu_p99_m, cpu_max_m=excluded.cpu_max_m,
			mem_p95=excluded.mem_p95, mem_p99=excluded.mem_p99, mem_max=excluded.mem_max,
			throttle_ratio=excluded.throttle_ratio, restarts=excluded.restarts,
			oom_seen=excluded.oom_seen, replicas=excluded.replicas, samples=excluded.samples,
			image_tag=excluded.image_tag`
	_, err := s.db.Exec(s.rebind(query),
		r.Tenant, r.Service, r.HourStart.UTC(),
		r.CPUP95m, r.CPUP99m, r.CPUMaxm, r.MemP95, r.MemP99, r.MemMax,
		r.ThrottleRatio, r.Restarts, oom, r.Replicas, r.Samples, r.ImageTag, time.Now().UTC())
	return err
}

// RollupsInWindow returns rollups with hour_start in [from, to), oldest first.
func (s *Store) RollupsInWindow(tenant, service string, from, to time.Time) ([]Rollup, error) {
	query := `
		SELECT tenant, service, hour_start, cpu_p95_m, cpu_p99_m, cpu_max_m,
			mem_p95, mem_p99, mem_max, throttle_ratio, restarts, oom_seen,
			replicas, samples, image_tag
		FROM optimizer_rollups
		WHERE tenant=? AND service=? AND hour_start >= ? AND hour_start < ?
		ORDER BY hour_start ASC`
	rows, err := s.db.Query(s.rebind(query), tenant, service, from.UTC(), to.UTC())
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []Rollup
	for rows.Next() {
		var r Rollup
		var oom int
		if err := rows.Scan(&r.Tenant, &r.Service, &r.HourStart,
			&r.CPUP95m, &r.CPUP99m, &r.CPUMaxm, &r.MemP95, &r.MemP99, &r.MemMax,
			&r.ThrottleRatio, &r.Restarts, &oom, &r.Replicas, &r.Samples, &r.ImageTag); err != nil {
			return nil, err
		}
		r.OOMSeen = oom != 0
		r.HourStart = r.HourStart.UTC()
		out = append(out, r)
	}
	return out, rows.Err()
}

// LatestRollupHour returns the newest hour_start recorded for the workload.
func (s *Store) LatestRollupHour(tenant, service string) (time.Time, bool, error) {
	query := `SELECT hour_start FROM optimizer_rollups WHERE tenant=? AND service=? ORDER BY hour_start DESC LIMIT 1`
	var t time.Time
	err := s.db.QueryRow(s.rebind(query), tenant, service).Scan(&t)
	if err == sql.ErrNoRows {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, err
	}
	return t.UTC(), true, nil
}

// PruneRollups deletes rollups older than the cutoff. Returns rows deleted.
func (s *Store) PruneRollups(olderThan time.Time) (int64, error) {
	res, err := s.db.Exec(s.rebind(`DELETE FROM optimizer_rollups WHERE hour_start < ?`), olderThan.UTC())
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// CreateRecommendation inserts a new recommendation, generating its ID.
func (s *Store) CreateRecommendation(r *Recommendation) error {
	r.ID = uuid.New().String()
	now := time.Now().UTC()
	r.CreatedAt = now
	r.UpdatedAt = now
	query := `
		INSERT INTO optimizer_recommendations (id, tenant, service, window_start, window_end,
			days_of_data, confidence, risk, old_cpu_request, old_mem_request,
			new_cpu_request, new_mem_request, evidence_json, status, branch,
			pr_number, pr_url, merge_sha, merged_at, rollback_pr_number, rollback_pr_url,
			created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	_, err := s.db.Exec(s.rebind(query),
		r.ID, r.Tenant, r.Service, r.WindowStart.UTC(), r.WindowEnd.UTC(),
		r.DaysOfData, r.Confidence, r.Risk, r.OldCPURequest, r.OldMemRequest,
		r.NewCPURequest, r.NewMemRequest, r.EvidenceJSON, r.Status, r.Branch,
		r.PRNumber, r.PRURL, r.MergeSHA, nullTime(r.MergedAt), r.RollbackPRNum, r.RollbackPRURL,
		r.CreatedAt, r.UpdatedAt)
	return err
}

// UpdateRecommendation persists mutable fields of an existing recommendation.
func (s *Store) UpdateRecommendation(r *Recommendation) error {
	r.UpdatedAt = time.Now().UTC()
	query := `
		UPDATE optimizer_recommendations SET
			status=?, branch=?, pr_number=?, pr_url=?, merge_sha=?, merged_at=?,
			rollback_pr_number=?, rollback_pr_url=?, evidence_json=?, updated_at=?
		WHERE id=?`
	_, err := s.db.Exec(s.rebind(query),
		r.Status, r.Branch, r.PRNumber, r.PRURL, r.MergeSHA, nullTime(r.MergedAt),
		r.RollbackPRNum, r.RollbackPRURL, r.EvidenceJSON, r.UpdatedAt, r.ID)
	return err
}

const recSelectCols = `id, tenant, service, window_start, window_end, days_of_data,
	confidence, risk, old_cpu_request, old_mem_request, new_cpu_request, new_mem_request,
	evidence_json, status, branch, pr_number, pr_url, merge_sha, merged_at,
	rollback_pr_number, rollback_pr_url, created_at, updated_at`

func scanRecommendation(row interface{ Scan(...any) error }) (*Recommendation, error) {
	var r Recommendation
	var mergedAt sql.NullTime
	err := row.Scan(&r.ID, &r.Tenant, &r.Service, &r.WindowStart, &r.WindowEnd, &r.DaysOfData,
		&r.Confidence, &r.Risk, &r.OldCPURequest, &r.OldMemRequest, &r.NewCPURequest, &r.NewMemRequest,
		&r.EvidenceJSON, &r.Status, &r.Branch, &r.PRNumber, &r.PRURL, &r.MergeSHA, &mergedAt,
		&r.RollbackPRNum, &r.RollbackPRURL, &r.CreatedAt, &r.UpdatedAt)
	if err != nil {
		return nil, err
	}
	if mergedAt.Valid {
		t := mergedAt.Time.UTC()
		r.MergedAt = &t
	}
	return &r, nil
}

// GetRecommendation fetches one recommendation by ID.
func (s *Store) GetRecommendation(id string) (*Recommendation, error) {
	query := `SELECT ` + recSelectCols + ` FROM optimizer_recommendations WHERE id=?`
	rec, err := scanRecommendation(s.db.QueryRow(s.rebind(query), id))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return rec, err
}

// OpenRecommendation returns the non-terminal recommendation for a workload,
// or nil. Enforces the one-open-recommendation-per-workload rule.
func (s *Store) OpenRecommendation(tenant, service string) (*Recommendation, error) {
	query := `SELECT ` + recSelectCols + `
		FROM optimizer_recommendations
		WHERE tenant=? AND service=? AND status IN (?, ?, ?, ?)
		ORDER BY created_at DESC LIMIT 1`
	rec, err := scanRecommendation(s.db.QueryRow(s.rebind(query),
		tenant, service, RecStatusDryRun, RecStatusPROpen, RecStatusMerged, RecStatusEvaluating))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return rec, err
}

// ListRecommendations returns recommendations, newest first, optionally
// filtered by status.
func (s *Store) ListRecommendations(status string, limit int) ([]*Recommendation, error) {
	if limit <= 0 {
		limit = 50
	}
	query := `SELECT ` + recSelectCols + ` FROM optimizer_recommendations`
	args := []any{}
	if status != "" {
		query += ` WHERE status=?`
		args = append(args, status)
	}
	query += ` ORDER BY created_at DESC LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.Query(s.rebind(query), args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []*Recommendation
	for rows.Next() {
		rec, err := scanRecommendation(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

// CountRecommendationPRsInWindow counts recommendations that resulted in a
// real PR (dry runs excluded) created within the past N hours. This is the
// optimizer's own budget, separate from the ticket-backed PR rate limits.
func (s *Store) CountRecommendationPRsInWindow(hours int) (int, error) {
	since := time.Now().UTC().Add(-time.Duration(hours) * time.Hour)
	query := `SELECT COUNT(*) FROM optimizer_recommendations WHERE pr_number > 0 AND created_at >= ?`
	var n int
	err := s.db.QueryRow(s.rebind(query), since).Scan(&n)
	return n, err
}

// CreateRun inserts a new evaluation run, generating its ID.
func (s *Store) CreateRun(r *Run) error {
	r.ID = uuid.New().String()
	now := time.Now().UTC()
	r.CreatedAt = now
	r.UpdatedAt = now
	query := `
		INSERT INTO optimizer_runs (id, recommendation_id, baseline_start, baseline_end,
			merged_at, warmup_until, eval_start, eval_end, status, verdict, verdict_json,
			comment_posted, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	_, err := s.db.Exec(s.rebind(query),
		r.ID, r.RecommendationID, r.BaselineStart.UTC(), r.BaselineEnd.UTC(),
		nullTime(r.MergedAt), nullTime(r.WarmupUntil), nullTime(r.EvalStart), nullTime(r.EvalEnd),
		r.Status, r.Verdict, r.VerdictJSON, boolInt(r.CommentPosted), r.CreatedAt, r.UpdatedAt)
	return err
}

// UpdateRun persists mutable fields of an existing run.
func (s *Store) UpdateRun(r *Run) error {
	r.UpdatedAt = time.Now().UTC()
	query := `
		UPDATE optimizer_runs SET
			merged_at=?, warmup_until=?, eval_start=?, eval_end=?, status=?,
			verdict=?, verdict_json=?, comment_posted=?, updated_at=?
		WHERE id=?`
	_, err := s.db.Exec(s.rebind(query),
		nullTime(r.MergedAt), nullTime(r.WarmupUntil), nullTime(r.EvalStart), nullTime(r.EvalEnd),
		r.Status, r.Verdict, r.VerdictJSON, boolInt(r.CommentPosted), r.UpdatedAt, r.ID)
	return err
}

// RunsByStatus lists runs in the given statuses, oldest first.
func (s *Store) RunsByStatus(statuses ...string) ([]*Run, error) {
	if len(statuses) == 0 {
		return nil, nil
	}
	placeholders := ""
	args := make([]any, 0, len(statuses))
	for i, st := range statuses {
		if i > 0 {
			placeholders += ", "
		}
		placeholders += "?"
		args = append(args, st)
	}
	query := `
		SELECT id, recommendation_id, baseline_start, baseline_end, merged_at,
			warmup_until, eval_start, eval_end, status, verdict, verdict_json,
			comment_posted, created_at, updated_at
		FROM optimizer_runs WHERE status IN (` + placeholders + `) ORDER BY created_at ASC`
	rows, err := s.db.Query(s.rebind(query), args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []*Run
	for rows.Next() {
		var r Run
		var mergedAt, warmupUntil, evalStart, evalEnd sql.NullTime
		var posted int
		if err := rows.Scan(&r.ID, &r.RecommendationID, &r.BaselineStart, &r.BaselineEnd,
			&mergedAt, &warmupUntil, &evalStart, &evalEnd, &r.Status, &r.Verdict,
			&r.VerdictJSON, &posted, &r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, err
		}
		r.MergedAt = timePtr(mergedAt)
		r.WarmupUntil = timePtr(warmupUntil)
		r.EvalStart = timePtr(evalStart)
		r.EvalEnd = timePtr(evalEnd)
		r.CommentPosted = posted != 0
		out = append(out, &r)
	}
	return out, rows.Err()
}

func nullTime(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UTC()
}

func timePtr(nt sql.NullTime) *time.Time {
	if !nt.Valid {
		return nil
	}
	t := nt.Time.UTC()
	return &t
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
