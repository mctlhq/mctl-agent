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
	"path/filepath"
	"testing"
)

const someTrace = "164ac97d8d7264326c406240d263dcc1"

// The trace id survives every read path, and a full-row Update from a copy
// that never saw it does not blank it.
//
// Mutation check: drop trace_id from any SELECT/Scan pair, or add it to
// Update's column list, and this fails.
func TestStoreTraceIDRoundTrip(t *testing.T) {
	store := newTestStore(t)
	tk := &Ticket{Source: SourceAlertManager, Type: TypePodCrashloop, Tenant: "billing", Service: "api", AlertName: "KubePodCrashLooping", Status: StatusOpen}
	if err := store.Create(ctx, tk); err != nil {
		t.Fatal(err)
	}
	stale := *tk // a copy taken before the trace existed
	if err := store.SetTraceID(ctx, tk.ID, someTrace); err != nil {
		t.Fatal(err)
	}
	stale.Summary = "updated from a stale copy"
	if err := store.Update(ctx, &stale); err != nil {
		t.Fatal(err)
	}

	got, err := store.Get(ctx, tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.TraceID != someTrace {
		t.Errorf("Get: trace_id = %q", got.TraceID)
	}
	open, err := store.ListOpen(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 1 || open[0].TraceID != someTrace {
		t.Errorf("ListOpen: %+v", open)
	}
	dup, err := store.FindDuplicate(ctx, "billing", "api", TypePodCrashloop, "KubePodCrashLooping")
	if err != nil || dup == nil || dup.TraceID != someTrace {
		t.Errorf("FindDuplicate: %+v, %v", dup, err)
	}
}

// A database created before #97 gains the column on the next start, and its
// existing rows read back with no trace rather than failing the scan.
func TestStoreTraceIDColumnIsAddedToAnExistingDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`
		CREATE TABLE tickets (
			id TEXT PRIMARY KEY, source TEXT NOT NULL, type TEXT NOT NULL,
			tenant TEXT NOT NULL DEFAULT '', service TEXT NOT NULL DEFAULT '',
			summary TEXT NOT NULL DEFAULT '', severity TEXT NOT NULL DEFAULT 'info',
			status TEXT NOT NULL DEFAULT 'open', analysis TEXT NOT NULL DEFAULT '',
			proposed_fix TEXT NOT NULL DEFAULT '', pr_url TEXT NOT NULL DEFAULT '',
			pr_number INTEGER NOT NULL DEFAULT 0, confidence TEXT NOT NULL DEFAULT '',
			created_at DATETIME NOT NULL, updated_at DATETIME NOT NULL, resolved_at DATETIME);
		INSERT INTO tickets (id, source, type, created_at, updated_at)
			VALUES ('old', 'alertmanager', 'pod_crashloop', '2026-01-01 00:00:00', '2026-01-01 00:00:00');`); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()

	store, err := NewStore(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	got, err := store.Get(ctx, "old")
	if err != nil {
		t.Fatal(err)
	}
	if got.TraceID != "" {
		t.Errorf("legacy row trace_id = %q", got.TraceID)
	}
}
