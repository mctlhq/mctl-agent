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
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func setupTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestMetricsRecordAndSnapshot(t *testing.T) {
	db := setupTestDB(t)

	m, err := NewMetrics(db, 0.3, 5)
	if err != nil {
		t.Fatal(err)
	}

	// Record some events.
	m.RecordMatch("oomkilled", "ticket-1")
	m.RecordDiagnosis("oomkilled", "ticket-1", true, 150*time.Millisecond, "OOM detected")
	m.RecordFix("oomkilled", "ticket-1", true, 200*time.Millisecond, "bumped memory")
	m.RecordResolution("oomkilled", "ticket-1", true)

	m.RecordMatch("oomkilled", "ticket-2")
	m.RecordDiagnosis("oomkilled", "ticket-2", true, 100*time.Millisecond, "")
	m.RecordFix("oomkilled", "ticket-2", false, 50*time.Millisecond, "patch failed")
	m.RecordResolution("oomkilled", "ticket-2", false)

	snap := m.GetSnapshot("oomkilled")

	if snap.TotalMatches != 2 {
		t.Errorf("expected 2 matches, got %d", snap.TotalMatches)
	}
	if snap.TotalDiagnoses != 2 {
		t.Errorf("expected 2 diagnoses, got %d", snap.TotalDiagnoses)
	}
	if snap.TotalFixes != 2 {
		t.Errorf("expected 2 fixes, got %d", snap.TotalFixes)
	}
	if snap.Successes != 1 {
		t.Errorf("expected 1 success, got %d", snap.Successes)
	}
	if snap.Failures != 1 {
		t.Errorf("expected 1 failure, got %d", snap.Failures)
	}
	if snap.SuccessRate != 0.5 {
		t.Errorf("expected 50%% success rate, got %f", snap.SuccessRate)
	}
	if snap.AvgDurationMs == 0 {
		t.Error("expected non-zero avg duration")
	}
}

func TestMetricsCircuitBreaker(t *testing.T) {
	db := setupTestDB(t)

	// Threshold: 30% success rate over last 5 resolutions.
	m, err := NewMetrics(db, 0.3, 5)
	if err != nil {
		t.Fatal(err)
	}

	// Record 5 failures — should trip circuit.
	for i := 0; i < 5; i++ {
		m.RecordResolution("bad_skill", "ticket-"+string(rune('a'+i)), false)
	}

	if !m.IsCircuitOpen("bad_skill") {
		t.Error("circuit should be open after 5 failures")
	}

	// Good skill with all successes — circuit should be closed.
	for i := 0; i < 5; i++ {
		m.RecordResolution("good_skill", "ticket-"+string(rune('a'+i)), true)
	}

	if m.IsCircuitOpen("good_skill") {
		t.Error("circuit should be closed for good skill")
	}

	// Not enough data — circuit should be closed.
	m.RecordResolution("new_skill", "ticket-x", false)
	if m.IsCircuitOpen("new_skill") {
		t.Error("circuit should be closed with insufficient data")
	}
}

func TestMetricsCheckAndDisable(t *testing.T) {
	db := setupTestDB(t)

	m, err := NewMetrics(db, 0.3, 3)
	if err != nil {
		t.Fatal(err)
	}

	reg := NewRegistry()
	reg.Register(&testSkill{name: "failing", matchResult: MatchResult{Matched: true}})
	reg.Register(&testSkill{name: "healthy", matchResult: MatchResult{Matched: true}})

	// Record 3 failures for "failing".
	for i := 0; i < 3; i++ {
		m.RecordResolution("failing", "t-"+string(rune('a'+i)), false)
	}
	// Record 3 successes for "healthy".
	for i := 0; i < 3; i++ {
		m.RecordResolution("healthy", "t-"+string(rune('a'+i)), true)
	}

	disabled := m.CheckAndDisable(reg)
	if len(disabled) != 1 || disabled[0] != "failing" {
		t.Errorf("expected only 'failing' disabled, got %v", disabled)
	}
	if reg.IsEnabled("failing") {
		t.Error("failing skill should be disabled")
	}
	if !reg.IsEnabled("healthy") {
		t.Error("healthy skill should still be enabled")
	}
}

func TestMetricsGetAllSnapshots(t *testing.T) {
	db := setupTestDB(t)

	m, err := NewMetrics(db, 0.3, 5)
	if err != nil {
		t.Fatal(err)
	}

	m.RecordMatch("skill-a", "t1")
	m.RecordMatch("skill-b", "t2")

	snaps := m.GetAllSnapshots()
	if len(snaps) != 2 {
		t.Errorf("expected 2 snapshots, got %d", len(snaps))
	}
}
