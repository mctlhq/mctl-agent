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
	"testing"
	"time"

	"github.com/mctlhq/mctl-agent/internal/ticket"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	ts, err := ticket.NewStore(":memory:")
	if err != nil {
		t.Fatalf("ticket store: %v", err)
	}
	s, err := NewStore(ts.DB(), ts.Dialect())
	if err != nil {
		t.Fatalf("optimizer store: %v", err)
	}
	return s
}

func TestMigrateIdempotent(t *testing.T) {
	s := newTestStore(t)
	if err := s.migrate(); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
}

func TestUpsertRollupDedup(t *testing.T) {
	s := newTestStore(t)
	hour := time.Date(2026, 7, 18, 10, 0, 0, 0, time.UTC)

	r := &Rollup{Tenant: "labs", Service: "svc", HourStart: hour, CPUP95m: 100, MemP99: 1024, Samples: 60}
	if err := s.UpsertRollup(r); err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	r.CPUP95m = 150
	r.OOMSeen = true
	if err := s.UpsertRollup(r); err != nil {
		t.Fatalf("second upsert: %v", err)
	}

	got, err := s.RollupsInWindow("labs", "svc", hour.Add(-time.Hour), hour.Add(time.Hour))
	if err != nil {
		t.Fatalf("RollupsInWindow: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1 (dedup)", len(got))
	}
	if got[0].CPUP95m != 150 || !got[0].OOMSeen {
		t.Errorf("rollup not updated: %+v", got[0])
	}

	latest, ok, err := s.LatestRollupHour("labs", "svc")
	if err != nil || !ok || !latest.Equal(hour) {
		t.Errorf("LatestRollupHour = (%v, %v, %v), want (%v, true, nil)", latest, ok, err, hour)
	}
	if _, ok, _ := s.LatestRollupHour("labs", "other"); ok {
		t.Error("LatestRollupHour for unknown service: ok = true, want false")
	}
}

func TestPruneRollups(t *testing.T) {
	s := newTestStore(t)
	old := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	recent := time.Date(2026, 7, 18, 0, 0, 0, 0, time.UTC)
	for _, h := range []time.Time{old, recent} {
		if err := s.UpsertRollup(&Rollup{Tenant: "labs", Service: "svc", HourStart: h}); err != nil {
			t.Fatal(err)
		}
	}
	n, err := s.PruneRollups(recent.Add(-24 * time.Hour))
	if err != nil {
		t.Fatalf("PruneRollups: %v", err)
	}
	if n != 1 {
		t.Errorf("pruned %d rows, want 1", n)
	}
}

func TestOpenRecommendationRule(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().UTC()

	rec := &Recommendation{
		Tenant: "labs", Service: "svc",
		WindowStart: now.Add(-14 * 24 * time.Hour), WindowEnd: now,
		DaysOfData: 14, Confidence: "HIGH", Risk: "LOW",
		OldCPURequest: "500m", OldMemRequest: "512Mi",
		NewCPURequest: "250m", NewMemRequest: "384Mi",
		Status: RecStatusPROpen, Branch: "agent/optimize/labs-svc-20260718",
		PRNumber: 42,
	}
	if err := s.CreateRecommendation(rec); err != nil {
		t.Fatalf("CreateRecommendation: %v", err)
	}
	if rec.ID == "" {
		t.Fatal("ID not generated")
	}

	open, err := s.OpenRecommendation("labs", "svc")
	if err != nil {
		t.Fatalf("OpenRecommendation: %v", err)
	}
	if open == nil || open.ID != rec.ID {
		t.Fatalf("open = %+v, want id %s", open, rec.ID)
	}
	if open.NewCPURequest != "250m" || open.PRNumber != 42 {
		t.Errorf("roundtrip mismatch: %+v", open)
	}

	// Terminal status frees the workload.
	rec.Status = RecStatusSuccess
	if err := s.UpdateRecommendation(rec); err != nil {
		t.Fatalf("UpdateRecommendation: %v", err)
	}
	open, err = s.OpenRecommendation("labs", "svc")
	if err != nil {
		t.Fatalf("OpenRecommendation after close: %v", err)
	}
	if open != nil {
		t.Errorf("open = %+v, want nil after terminal status", open)
	}

	n, err := s.CountRecommendationPRsInWindow(24)
	if err != nil || n != 1 {
		t.Errorf("CountRecommendationPRsInWindow = (%d, %v), want (1, nil)", n, err)
	}

	list, err := s.ListRecommendations("", 10)
	if err != nil || len(list) != 1 {
		t.Errorf("ListRecommendations = (%d, %v), want 1 row", len(list), err)
	}
}

func TestRunLifecycle(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().UTC().Truncate(time.Second)

	rec := &Recommendation{Tenant: "labs", Service: "svc", WindowStart: now.Add(-time.Hour), WindowEnd: now, Status: RecStatusPROpen}
	if err := s.CreateRecommendation(rec); err != nil {
		t.Fatal(err)
	}

	run := &Run{
		RecommendationID: rec.ID,
		BaselineStart:    now.Add(-14 * 24 * time.Hour),
		BaselineEnd:      now,
		Status:           RunStatusWaitingMerge,
	}
	if err := s.CreateRun(run); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	waiting, err := s.RunsByStatus(RunStatusWaitingMerge)
	if err != nil || len(waiting) != 1 {
		t.Fatalf("RunsByStatus = (%d, %v), want 1", len(waiting), err)
	}
	if waiting[0].MergedAt != nil {
		t.Error("MergedAt should be nil before merge")
	}

	merged := now
	warmup := now.Add(24 * time.Hour)
	run.MergedAt = &merged
	run.WarmupUntil = &warmup
	run.Status = RunStatusWarmup
	if err := s.UpdateRun(run); err != nil {
		t.Fatalf("UpdateRun: %v", err)
	}

	got, err := s.RunsByStatus(RunStatusWarmup, RunStatusEvaluating)
	if err != nil || len(got) != 1 {
		t.Fatalf("RunsByStatus after update = (%d, %v), want 1", len(got), err)
	}
	if got[0].MergedAt == nil || !got[0].MergedAt.Equal(merged) {
		t.Errorf("MergedAt = %v, want %v", got[0].MergedAt, merged)
	}
	if got[0].WarmupUntil == nil || !got[0].WarmupUntil.Equal(warmup) {
		t.Errorf("WarmupUntil = %v, want %v", got[0].WarmupUntil, warmup)
	}
}
