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

package monitor

import (
	"testing"
	"time"

	"github.com/mctlhq/mctl-agent/internal/ticket"
	_ "modernc.org/sqlite"
)

// backdate rewrites the ticket's UpdatedAt (and CreatedAt if older) to
// the given moment. Used to simulate a ticket that went stale.
func backdate(t *testing.T, store *ticket.Store, id string, when time.Time) {
	t.Helper()
	full, err := store.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	full.UpdatedAt = when
	if full.CreatedAt.After(when) {
		full.CreatedAt = when
	}
	if err := store.Update(full); err != nil {
		t.Fatal(err)
	}
	// Update bumps UpdatedAt to now; rewrite it directly with a raw query.
	if _, err := store.DB().Exec(
		`UPDATE tickets SET updated_at=? WHERE id=?`, when, id,
	); err != nil {
		t.Fatal(err)
	}
}

func TestPollerResolvesStaleOpenTicket(t *testing.T) {
	store := newTestStore(t)
	p := NewPoller(nil, store, nil)
	p.StaleAfter = 24 * time.Hour

	// Old, untouched, status=open → should resolve.
	oldTicket := &ticket.Ticket{
		Source:   ticket.SourceAlertManager,
		Type:     ticket.TypePodCrashloop,
		Tenant:   "labs",
		Service:  "ghost-pod",
		Summary:  "stale",
		Severity: ticket.SeverityCritical,
	}
	if err := store.Create(oldTicket); err != nil {
		t.Fatal(err)
	}
	backdate(t, store, oldTicket.ID, time.Now().UTC().Add(-30*time.Hour))

	// Fresh ticket (just created) → should NOT resolve.
	freshTicket := &ticket.Ticket{
		Source:   ticket.SourceAlertManager,
		Type:     ticket.TypeResourceLimit,
		Tenant:   "labs",
		Service:  "live-service",
		Summary:  "fresh",
		Severity: ticket.SeverityWarning,
	}
	if err := store.Create(freshTicket); err != nil {
		t.Fatal(err)
	}

	// Old ticket but status=analyzing → must NOT be auto-resolved
	// (pipeline owns it).
	analyzing := &ticket.Ticket{
		Source:   ticket.SourceAlertManager,
		Type:     ticket.TypeGeneric,
		Tenant:   "labs",
		Service:  "in-progress",
		Summary:  "analyzing",
		Severity: ticket.SeverityWarning,
	}
	if err := store.Create(analyzing); err != nil {
		t.Fatal(err)
	}
	analyzing.Status = ticket.StatusAnalyzing
	if err := store.Update(analyzing); err != nil {
		t.Fatal(err)
	}
	backdate(t, store, analyzing.ID, time.Now().UTC().Add(-30*time.Hour))

	p.resolveStale()

	// Verify: oldTicket is resolved, others still open.
	got, err := store.Get(oldTicket.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != ticket.StatusResolved {
		t.Errorf("stale open ticket: status = %q, want resolved", got.Status)
	}

	got, err = store.Get(freshTicket.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status == ticket.StatusResolved {
		t.Error("fresh ticket was resolved; should have been left alone")
	}

	got, err = store.Get(analyzing.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status == ticket.StatusResolved {
		t.Errorf("analyzing ticket was resolved; pipeline state must be preserved")
	}
}

func TestResolveByIDIgnoresNonOpenTickets(t *testing.T) {
	// Guards the race where the pipeline promotes a ticket from open →
	// analyzing between resolveStale's ListOpen read and its ResolveByID
	// write. The UPDATE must refuse to close anything that has since
	// moved out of status=open.
	store := newTestStore(t)

	for _, status := range []string{
		ticket.StatusAnalyzing,
		ticket.StatusFixProposed,
		ticket.StatusFixApplied,
	} {
		t.Run(status, func(t *testing.T) {
			tk := &ticket.Ticket{
				Source:   ticket.SourceAlertManager,
				Type:     ticket.TypeGeneric,
				Tenant:   "labs",
				Service:  "svc-" + status,
				Summary:  "in pipeline",
				Severity: ticket.SeverityWarning,
			}
			if err := store.Create(tk); err != nil {
				t.Fatal(err)
			}
			tk.Status = status
			if err := store.Update(tk); err != nil {
				t.Fatal(err)
			}

			if err := store.ResolveByID(tk.ID); err != nil {
				t.Fatal(err)
			}

			got, err := store.Get(tk.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.Status != status {
				t.Errorf("ResolveByID overwrote status=%s → %s; should be a no-op",
					status, got.Status)
			}
		})
	}
}

func TestPollerResolveStaleDisabledWhenZero(t *testing.T) {
	store := newTestStore(t)
	p := NewPoller(nil, store, nil)
	// StaleAfter left at zero → no-op.

	stale := &ticket.Ticket{
		Source:   ticket.SourceAlertManager,
		Type:     ticket.TypePodCrashloop,
		Tenant:   "labs",
		Service:  "anything",
		Summary:  "s",
		Severity: ticket.SeverityWarning,
	}
	if err := store.Create(stale); err != nil {
		t.Fatal(err)
	}
	backdate(t, store, stale.ID, time.Now().UTC().Add(-30*24*time.Hour))

	p.resolveStale()

	got, _ := store.Get(stale.ID)
	if got.Status == ticket.StatusResolved {
		t.Error("StaleAfter=0 must disable the GC; ticket was still resolved")
	}
}
