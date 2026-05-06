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
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
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

	p.resolveStale(refreshState{})

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

			resolved, err := store.ResolveByID(tk.ID)
			if err != nil {
				t.Fatal(err)
			}
			if resolved {
				t.Errorf("ResolveByID returned resolved=true for status=%s; should report no-op", status)
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

	p.resolveStale(refreshState{})

	got, _ := store.Get(stale.ID)
	if got.Status == ticket.StatusResolved {
		t.Error("StaleAfter=0 must disable the GC; ticket was still resolved")
	}
}

func TestResolveStaleSkipsManualSource(t *testing.T) {
	// Pipeline.TriggerAnalysis creates SourceManual tickets (e.g. from
	// MCP for a user-initiated investigation). They have no recurring
	// signal, so UpdatedAt can only advance through pipeline state
	// transitions. A paused pipeline + a still-open manual ticket must
	// not be silently closed by the stale GC.
	store := newTestStore(t)
	p := NewPoller(nil, store, nil)
	p.StaleAfter = 24 * time.Hour

	manual := &ticket.Ticket{
		Source:   ticket.SourceManual,
		Type:     ticket.TypeArgoCDDegraded, // whitelisted type
		Tenant:   "labs",
		Service:  "user-poked",
		Summary:  "triggered via MCP",
		Severity: ticket.SeverityWarning,
	}
	if err := store.Create(manual); err != nil {
		t.Fatal(err)
	}
	backdate(t, store, manual.ID, time.Now().UTC().Add(-30*time.Hour))

	p.resolveStale(refreshState{})

	got, _ := store.Get(manual.ID)
	if got.Status == ticket.StatusResolved {
		t.Errorf("SourceManual ticket must be spared by stale GC; got status=%q", got.Status)
	}
}

func TestResolveStaleArgoGatedOnRefresh(t *testing.T) {
	// ArgoCDDegraded tickets must only be GC'd when the current cycle
	// actually reached the underlying service. AlertManager-based
	// tickets (e.g. pod_crashloop) are unaffected and must still GC.
	store := newTestStore(t)
	p := NewPoller(nil, store, nil)
	p.StaleAfter = 24 * time.Hour

	mkTicket := func(typ, service string) string {
		tk := &ticket.Ticket{
			Source:   ticket.SourcePolling,
			Type:     typ,
			Tenant:   "labs",
			Service:  service,
			Summary:  "x",
			Severity: ticket.SeverityWarning,
		}
		if err := store.Create(tk); err != nil {
			t.Fatal(err)
		}
		backdate(t, store, tk.ID, time.Now().UTC().Add(-30*time.Hour))
		return tk.ID
	}

	argoRefreshed := mkTicket(ticket.TypeArgoCDDegraded, "svc-ok")
	argoFailed := mkTicket(ticket.TypeArgoCDDegraded, "svc-broken")
	podCrash := mkTicket(ticket.TypePodCrashloop, "some-pod")

	// Run with partial failure: svc-broken's status fetch failed.
	state := refreshState{failedServices: map[string]bool{"labs/svc-broken": true}}
	p.resolveStale(state)

	if got, _ := store.Get(argoRefreshed); got.Status != ticket.StatusResolved {
		t.Errorf("argo ticket for refreshed service: status=%q, want resolved", got.Status)
	}
	if got, _ := store.Get(argoFailed); got.Status == ticket.StatusResolved {
		t.Errorf("argo ticket for failed service must NOT be resolved; telemetry gap")
	}
	if got, _ := store.Get(podCrash); got.Status != ticket.StatusResolved {
		t.Errorf("pod_crashloop ticket is AlertManager-based; must GC regardless; got %q", got.Status)
	}
}

func TestResolveStaleOnlyHeartbeatTypes(t *testing.T) {
	// Only ticket types whose sources emit a Touch on duplicate events
	// are safe to GC by UpdatedAt age. Other types (currently none —
	// this test documents the contract) must be skipped.
	store := newTestStore(t)
	p := NewPoller(nil, store, nil)
	p.StaleAfter = 24 * time.Hour

	mkStale := func(typ string) string {
		tk := &ticket.Ticket{
			Source:   ticket.SourceAlertManager,
			Type:     typ,
			Tenant:   "labs",
			Service:  "svc-" + typ,
			Summary:  "x",
			Severity: ticket.SeverityWarning,
		}
		if err := store.Create(tk); err != nil {
			t.Fatal(err)
		}
		backdate(t, store, tk.ID, time.Now().UTC().Add(-30*time.Hour))
		return tk.ID
	}

	// Heartbeat-enabled types must be resolved.
	resolvable := []string{
		ticket.TypePodCrashloop,
		ticket.TypeResourceLimit,
		ticket.TypeWorkflowFailed,
		ticket.TypeGeneric,
		ticket.TypeGitHubActionsFailed,
	}
	var resolvableIDs []string
	for _, typ := range resolvable {
		resolvableIDs = append(resolvableIDs, mkStale(typ))
	}
	// Unknown type must be preserved by the whitelist gate.
	unknownID := mkStale(ticket.TypeDeploymentFailed)

	p.resolveStale(refreshState{})

	for i, id := range resolvableIDs {
		if got, _ := store.Get(id); got.Status != ticket.StatusResolved {
			t.Errorf("heartbeat type %q: status=%q, want resolved", resolvable[i], got.Status)
		}
	}
	if got, _ := store.Get(unknownID); got.Status == ticket.StatusResolved {
		t.Errorf("non-heartbeat type %q must be spared; got %q",
			ticket.TypeDeploymentFailed, got.Status)
	}
}

func TestResolveStaleArgoSkippedWhenAllUnknown(t *testing.T) {
	// ListServices failed entirely — every ArgoCDDegraded ticket must
	// be skipped, but AlertManager-based tickets still GC.
	store := newTestStore(t)
	p := NewPoller(nil, store, nil)
	p.StaleAfter = 24 * time.Hour

	argoTicket := &ticket.Ticket{
		Source:   ticket.SourcePolling,
		Type:     ticket.TypeArgoCDDegraded,
		Tenant:   "labs",
		Service:  "any-svc",
		Summary:  "x",
		Severity: ticket.SeverityWarning,
	}
	if err := store.Create(argoTicket); err != nil {
		t.Fatal(err)
	}
	backdate(t, store, argoTicket.ID, time.Now().UTC().Add(-30*time.Hour))

	amTicket := &ticket.Ticket{
		Source:   ticket.SourceAlertManager,
		Type:     ticket.TypeResourceLimit,
		Tenant:   "labs",
		Service:  "throttler",
		Summary:  "x",
		Severity: ticket.SeverityWarning,
	}
	if err := store.Create(amTicket); err != nil {
		t.Fatal(err)
	}
	backdate(t, store, amTicket.ID, time.Now().UTC().Add(-30*time.Hour))

	p.resolveStale(refreshState{allUnknown: true})

	if got, _ := store.Get(argoTicket.ID); got.Status == ticket.StatusResolved {
		t.Error("ArgoCDDegraded ticket must not be resolved when allUnknown=true")
	}
	if got, _ := store.Get(amTicket.ID); got.Status != ticket.StatusResolved {
		t.Errorf("AlertManager ticket must still GC; got %q", got.Status)
	}
}

func TestPollerResolvesStaleAnalyzingTicket(t *testing.T) {
	store := newTestStore(t)
	p := NewPoller(nil, store, nil)
	p.StaleAfter = 24 * time.Hour
	p.AnalyzingAfter = 48 * time.Hour

	tk := &ticket.Ticket{
		Source:   ticket.SourceAlertManager,
		Type:     ticket.TypePodCrashloop,
		Tenant:   "labs",
		Service:  "stuck-analyzing",
		Summary:  "stuck in analyzing",
		Severity: ticket.SeverityCritical,
	}
	if err := store.Create(tk); err != nil {
		t.Fatal(err)
	}
	tk.Status = ticket.StatusAnalyzing
	if err := store.Update(tk); err != nil {
		t.Fatal(err)
	}
	backdate(t, store, tk.ID, time.Now().UTC().Add(-72*time.Hour))

	p.resolveStale(refreshState{})

	got, err := store.Get(tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != ticket.StatusResolved {
		t.Errorf("analyzing ticket past 48h: status = %q, want resolved", got.Status)
	}
	wantSubstr := "Auto-resolved by stale TTL GC (status=analyzing"
	if !strings.Contains(got.Analysis, wantSubstr) {
		t.Errorf("analysis field = %q, want to contain %q", got.Analysis, wantSubstr)
	}
}

func TestPollerResolvesStaleFixProposedTicket(t *testing.T) {
	store := newTestStore(t)
	p := NewPoller(nil, store, nil)
	p.StaleAfter = 24 * time.Hour
	p.FixProposedAfter = 168 * time.Hour

	tk := &ticket.Ticket{
		Source:   ticket.SourceAlertManager,
		Type:     ticket.TypeResourceLimit,
		Tenant:   "admins",
		Service:  "old-pr-service",
		Summary:  "fix proposed, PR abandoned",
		Severity: ticket.SeverityWarning,
	}
	if err := store.Create(tk); err != nil {
		t.Fatal(err)
	}
	tk.Status = ticket.StatusFixProposed
	if err := store.Update(tk); err != nil {
		t.Fatal(err)
	}
	backdate(t, store, tk.ID, time.Now().UTC().Add(-200*time.Hour))

	p.resolveStale(refreshState{})

	got, err := store.Get(tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != ticket.StatusResolved {
		t.Errorf("fix_proposed ticket past 168h: status = %q, want resolved", got.Status)
	}
	wantSubstr := "Auto-resolved by stale TTL GC (status=fix_proposed"
	if !strings.Contains(got.Analysis, wantSubstr) {
		t.Errorf("analysis field = %q, want to contain %q", got.Analysis, wantSubstr)
	}
}

func TestPollerKeepsRecentAnalyzingTicket(t *testing.T) {
	store := newTestStore(t)
	p := NewPoller(nil, store, nil)
	p.StaleAfter = 24 * time.Hour
	p.AnalyzingAfter = 48 * time.Hour

	tk := &ticket.Ticket{
		Source:   ticket.SourceAlertManager,
		Type:     ticket.TypeGeneric,
		Tenant:   "ovk",
		Service:  "recent-analyzing",
		Summary:  "only 24h old, should not resolve",
		Severity: ticket.SeverityWarning,
	}
	if err := store.Create(tk); err != nil {
		t.Fatal(err)
	}
	tk.Status = ticket.StatusAnalyzing
	if err := store.Update(tk); err != nil {
		t.Fatal(err)
	}
	backdate(t, store, tk.ID, time.Now().UTC().Add(-24*time.Hour))

	p.resolveStale(refreshState{})

	got, err := store.Get(tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status == ticket.StatusResolved {
		t.Errorf("analyzing ticket only 24h old (threshold=48h): should not be resolved, got status=%q", got.Status)
	}
}

func TestPrunesOrphanTicketAfterGracePeriod(t *testing.T) {
	// A ticket on a service NOT in the inventory, past the grace period,
	// must be resolved with the orphan reason.
	store := newTestStore(t)
	p := NewPoller(nil, store, nil)
	p.OrphanAfter = 1 * time.Hour

	statuses := []string{
		ticket.StatusOpen,
		ticket.StatusAnalyzing,
		ticket.StatusFixProposed,
	}
	for _, status := range statuses {
		t.Run(status, func(t *testing.T) {
			tk := &ticket.Ticket{
				Source:   ticket.SourceAlertManager,
				Type:     ticket.TypePodCrashloop,
				Tenant:   "ovk",
				Service:  "smoke",
				Summary:  "synthetic alert",
				Severity: ticket.SeverityCritical,
			}
			if err := store.Create(tk); err != nil {
				t.Fatal(err)
			}
			if status != ticket.StatusOpen {
				tk.Status = status
				if err := store.Update(tk); err != nil {
					t.Fatal(err)
				}
			}
			backdate(t, store, tk.ID, time.Now().UTC().Add(-2*time.Hour))

			state := refreshState{
				knownServices: map[string]bool{"ovk/real-service": true},
			}
			p.pruneOrphans(state)

			got, err := store.Get(tk.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.Status != ticket.StatusResolved {
				t.Errorf("status=%s: want resolved, got %q", status, got.Status)
			}
			const wantSubstr = "Auto-resolved: service does not exist"
			if !strings.Contains(got.Analysis, wantSubstr) {
				t.Errorf("status=%s: analysis=%q, want to contain %q", status, got.Analysis, wantSubstr)
			}
		})
	}
}

func TestKeepsTicketWhoseServiceExists(t *testing.T) {
	// A ticket whose (tenant, service) IS in the inventory must not be resolved.
	store := newTestStore(t)
	p := NewPoller(nil, store, nil)
	p.OrphanAfter = 1 * time.Hour

	tk := &ticket.Ticket{
		Source:   ticket.SourceAlertManager,
		Type:     ticket.TypePodCrashloop,
		Tenant:   "ovk",
		Service:  "real-service",
		Summary:  "real alert",
		Severity: ticket.SeverityCritical,
	}
	if err := store.Create(tk); err != nil {
		t.Fatal(err)
	}
	backdate(t, store, tk.ID, time.Now().UTC().Add(-2*time.Hour))

	state := refreshState{
		knownServices: map[string]bool{"ovk/real-service": true},
	}
	p.pruneOrphans(state)

	got, err := store.Get(tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status == ticket.StatusResolved {
		t.Errorf("ticket for known service must not be orphan-pruned; got status=%q", got.Status)
	}
}

func TestSkipsOrphanPruneWhenInventoryUnknown(t *testing.T) {
	// When allUnknown=true (ListServices failed), no ticket must be resolved
	// by the orphan prune pass, even if backdated past the grace period.
	store := newTestStore(t)
	p := NewPoller(nil, store, nil)
	p.OrphanAfter = 1 * time.Hour

	tk := &ticket.Ticket{
		Source:   ticket.SourceAlertManager,
		Type:     ticket.TypePodCrashloop,
		Tenant:   "ovk",
		Service:  "does-not-exist",
		Summary:  "orphan",
		Severity: ticket.SeverityCritical,
	}
	if err := store.Create(tk); err != nil {
		t.Fatal(err)
	}
	backdate(t, store, tk.ID, time.Now().UTC().Add(-2*time.Hour))

	state := refreshState{allUnknown: true}
	p.pruneOrphans(state)

	got, err := store.Get(tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status == ticket.StatusResolved {
		t.Errorf("orphan prune must be skipped when inventory is unknown; got status=%q", got.Status)
	}
}

func TestSkipsManualOrphanTicket(t *testing.T) {
	// SourceManual tickets must never be orphan-pruned, even if the service
	// does not exist and the ticket is past the grace period.
	store := newTestStore(t)
	p := NewPoller(nil, store, nil)
	p.OrphanAfter = 1 * time.Hour

	tk := &ticket.Ticket{
		Source:   ticket.SourceManual,
		Type:     ticket.TypeArgoCDDegraded,
		Tenant:   "ovk",
		Service:  "does-not-exist",
		Summary:  "operator investigation",
		Severity: ticket.SeverityWarning,
	}
	if err := store.Create(tk); err != nil {
		t.Fatal(err)
	}
	backdate(t, store, tk.ID, time.Now().UTC().Add(-2*time.Hour))

	// Non-empty inventory so the empty-inventory guard does not fire and
	// we genuinely exercise the source allow-list.
	state := refreshState{
		knownServices: map[string]bool{"ovk/something-else": true},
	}
	p.pruneOrphans(state)

	got, err := store.Get(tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status == ticket.StatusResolved {
		t.Errorf("SourceManual ticket must be spared by orphan pruning; got status=%q", got.Status)
	}
}

func TestSkipsOrphanPruneOnEmptyInventory(t *testing.T) {
	// An empty service list returned by mctl-api (HTTP 200 but no items —
	// e.g. partial outage in the upstream listing) is indistinguishable
	// from "no services exist" and must NOT cause a mass-resolve. Codex
	// P1 review on PR #12.
	store := newTestStore(t)
	p := NewPoller(nil, store, nil)
	p.OrphanAfter = 1 * time.Hour

	tk := &ticket.Ticket{
		Source:   ticket.SourceAlertManager,
		Type:     ticket.TypePodCrashloop,
		Tenant:   "ovk",
		Service:  "real-service",
		Summary:  "real alert",
		Severity: ticket.SeverityCritical,
	}
	if err := store.Create(tk); err != nil {
		t.Fatal(err)
	}
	backdate(t, store, tk.ID, time.Now().UTC().Add(-2*time.Hour))

	// allUnknown=false but the inventory came back empty. Must not resolve.
	state := refreshState{
		knownServices: map[string]bool{},
	}
	p.pruneOrphans(state)

	got, err := store.Get(tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status == ticket.StatusResolved {
		t.Errorf("orphan prune must be skipped when inventory is empty; got status=%q", got.Status)
	}
}

func TestSkipsOrphanPruneForGitHubWebhookSource(t *testing.T) {
	// SourceGitHubWebhook tickets carry repo metadata in (Tenant, Service),
	// which does NOT map to mctl service inventory. Auto-resolving them
	// would drop legitimate CI failures. Only AlertManager and Polling
	// sources are inventory-backed and safe to orphan-prune. Codex P1
	// review on PR #12.
	store := newTestStore(t)
	p := NewPoller(nil, store, nil)
	p.OrphanAfter = 1 * time.Hour

	tk := &ticket.Ticket{
		Source:   ticket.SourceGitHubWebhook,
		Type:     ticket.TypeGitHubActionsFailed,
		Tenant:   "mctlhq",
		Service:  "mctl-agent",
		Summary:  "build failed on main",
		Severity: ticket.SeverityWarning,
	}
	if err := store.Create(tk); err != nil {
		t.Fatal(err)
	}
	backdate(t, store, tk.ID, time.Now().UTC().Add(-2*time.Hour))

	// (mctlhq/mctl-agent) is intentionally absent from the mctl service
	// inventory — the inventory keys on tenant+app, not GitHub org+repo.
	state := refreshState{
		knownServices: map[string]bool{"ovk/something-else": true},
	}
	p.pruneOrphans(state)

	got, err := store.Get(tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status == ticket.StatusResolved {
		t.Errorf("GitHub-webhook ticket must be spared by orphan pruning; got status=%q", got.Status)
	}
}

func makeAMServer(t *testing.T, fingerprints ...string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		alerts := make([]map[string]interface{}, 0, len(fingerprints))
		for _, fp := range fingerprints {
			alerts = append(alerts, map[string]interface{}{
				"fingerprint": fp,
				"status":      map[string]interface{}{"state": "active"},
			})
		}
		b, _ := json.Marshal(alerts)
		_, _ = w.Write(b)
	}))
}

func newAMPoller(t *testing.T, srv *httptest.Server) *Poller {
	t.Helper()
	store := newTestStore(t)
	p := NewPoller(nil, store, nil)
	p.AMReconcileEnabled = true
	p.AMReconcileMinAge = 15 * time.Minute
	p.AMClient = &AlertManagerClient{
		BaseURL: srv.URL,
		Timeout: 5 * time.Second,
		HTTP:    srv.Client(),
	}
	return p
}

func createAMTicket(t *testing.T, store *ticket.Store, fp, status string) *ticket.Ticket {
	t.Helper()
	tk := &ticket.Ticket{
		Source:           ticket.SourceAlertManager,
		Type:             ticket.TypePodCrashloop,
		Tenant:           "labs",
		Service:          "svc",
		Summary:          "test",
		Severity:         ticket.SeverityCritical,
		AlertFingerprint: fp,
		Status:           status,
	}
	if err := store.Create(tk); err != nil {
		t.Fatal(err)
	}
	return tk
}

func TestAMReconcileResolvesNonFiringTicket(t *testing.T) {
	srv := makeAMServer(t, "other-fingerprint")
	defer srv.Close()

	p := newAMPoller(t, srv)
	tk := createAMTicket(t, p.store, "fp-gone", ticket.StatusOpen)
	backdate(t, p.store, tk.ID, time.Now().UTC().Add(-20*time.Minute))

	p.reconcileWithAlertManager(context.Background())

	got, err := p.store.Get(tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != ticket.StatusResolved {
		t.Errorf("want resolved, got %s", got.Status)
	}
	if !strings.Contains(got.Analysis, "Auto-resolved by AM reconcile") {
		t.Errorf("analysis %q missing expected reason", got.Analysis)
	}
}

func TestAMReconcileKeepsActiveTicket(t *testing.T) {
	const fp = "fp-active"
	srv := makeAMServer(t, fp)
	defer srv.Close()

	p := newAMPoller(t, srv)
	tk := createAMTicket(t, p.store, fp, ticket.StatusOpen)
	backdate(t, p.store, tk.ID, time.Now().UTC().Add(-20*time.Minute))

	p.reconcileWithAlertManager(context.Background())

	got, err := p.store.Get(tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != ticket.StatusOpen {
		t.Errorf("want open, got %s", got.Status)
	}
}

func TestAMReconcileSkipsBelowMinAge(t *testing.T) {
	srv := makeAMServer(t, "different-fp")
	defer srv.Close()

	p := newAMPoller(t, srv)
	tk := createAMTicket(t, p.store, "fp-young", ticket.StatusOpen)
	// age = 5 min < 15 min min age
	backdate(t, p.store, tk.ID, time.Now().UTC().Add(-5*time.Minute))

	p.reconcileWithAlertManager(context.Background())

	got, err := p.store.Get(tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != ticket.StatusOpen {
		t.Errorf("want open (too young), got %s", got.Status)
	}
}

func TestAMReconcileSkipsEmptyActiveSet(t *testing.T) {
	// AM returns empty array — must not resolve any tickets.
	srv := makeAMServer(t) // no fingerprints
	defer srv.Close()

	p := newAMPoller(t, srv)
	tk := createAMTicket(t, p.store, "fp-any", ticket.StatusOpen)
	backdate(t, p.store, tk.ID, time.Now().UTC().Add(-20*time.Minute))

	p.reconcileWithAlertManager(context.Background())

	got, err := p.store.Get(tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != ticket.StatusOpen {
		t.Errorf("want open (empty AM set), got %s", got.Status)
	}
}

func TestAMReconcileSkipsOnAMError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	p := newAMPoller(t, srv)
	tk := createAMTicket(t, p.store, "fp-any", ticket.StatusOpen)
	backdate(t, p.store, tk.ID, time.Now().UTC().Add(-20*time.Minute))

	p.reconcileWithAlertManager(context.Background())

	got, err := p.store.Get(tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != ticket.StatusOpen {
		t.Errorf("want open (AM 500 error), got %s", got.Status)
	}
}

func TestAMReconcileSkipsTicketsWithoutFingerprint(t *testing.T) {
	srv := makeAMServer(t, "some-fp")
	defer srv.Close()

	p := newAMPoller(t, srv)
	tk := createAMTicket(t, p.store, "", ticket.StatusOpen) // no fingerprint
	backdate(t, p.store, tk.ID, time.Now().UTC().Add(-20*time.Minute))

	p.reconcileWithAlertManager(context.Background())

	got, err := p.store.Get(tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != ticket.StatusOpen {
		t.Errorf("want open (no fingerprint), got %s", got.Status)
	}
}

func TestAMReconcileSkipsNonAlertManagerSource(t *testing.T) {
	srv := makeAMServer(t, "some-fp")
	defer srv.Close()

	p := newAMPoller(t, srv)
	tk := &ticket.Ticket{
		Source:           ticket.SourcePolling,
		Type:             ticket.TypeArgoCDDegraded,
		Tenant:           "labs",
		Service:          "svc",
		Summary:          "test",
		Severity:         ticket.SeverityWarning,
		AlertFingerprint: "other-fp",
	}
	if err := p.store.Create(tk); err != nil {
		t.Fatal(err)
	}
	backdate(t, p.store, tk.ID, time.Now().UTC().Add(-20*time.Minute))

	p.reconcileWithAlertManager(context.Background())

	got, err := p.store.Get(tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != ticket.StatusOpen {
		t.Errorf("want open (polling source), got %s", got.Status)
	}
}

func TestAMReconcileSkipsWhenDisabled(t *testing.T) {
	hitCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hitCount++
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()

	store := newTestStore(t)
	p := NewPoller(nil, store, nil)
	p.AMReconcileEnabled = false
	p.AMReconcileMinAge = 15 * time.Minute
	p.AMClient = &AlertManagerClient{
		BaseURL: srv.URL,
		Timeout: 5 * time.Second,
		HTTP:    srv.Client(),
	}

	tk := createAMTicket(t, store, "fp-any", ticket.StatusOpen)
	backdate(t, store, tk.ID, time.Now().UTC().Add(-20*time.Minute))

	p.reconcileWithAlertManager(context.Background())

	if hitCount != 0 {
		t.Errorf("AM should not be called when disabled, got %d calls", hitCount)
	}
	got, err := store.Get(tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != ticket.StatusOpen {
		t.Errorf("want open (disabled), got %s", got.Status)
	}
}

// TestAMReconcileKeepsTicketWhenAnyFingerprintActive guards against the
// bug Codex flagged on PR #13 (P1): tickets are deduplicated by
// (tenant, service, type), so a single ticket can carry multiple
// fingerprints when several concurrent alerts collapse onto it (e.g.
// the same alertname firing on two pods of the same service). The
// reconciliation pass must keep the ticket open as long as ANY of its
// fingerprints is still in AM's active set; only when every one is
// absent is the underlying incident really resolved.
func TestAMReconcileKeepsTicketWhenAnyFingerprintActive(t *testing.T) {
	srv := makeAMServer(t, "fp-A") // only A is active; B is gone
	defer srv.Close()

	p := newAMPoller(t, srv)
	tk := createAMTicket(t, p.store, "fp-A,fp-B", ticket.StatusOpen)
	backdate(t, p.store, tk.ID, time.Now().UTC().Add(-20*time.Minute))

	p.reconcileWithAlertManager(context.Background())

	got, err := p.store.Get(tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != ticket.StatusOpen {
		t.Errorf("ticket with one still-firing fingerprint must NOT be resolved; got status=%q", got.Status)
	}
}

// TestAMReconcileResolvesWhenAllFingerprintsAbsent is the positive
// counterpart: a ticket carrying a CSV set of fingerprints, ALL of
// which are absent from AM's active set, is correctly resolved.
func TestAMReconcileResolvesWhenAllFingerprintsAbsent(t *testing.T) {
	srv := makeAMServer(t, "fp-other")
	defer srv.Close()

	p := newAMPoller(t, srv)
	tk := createAMTicket(t, p.store, "fp-A,fp-B,fp-C", ticket.StatusOpen)
	backdate(t, p.store, tk.ID, time.Now().UTC().Add(-20*time.Minute))

	p.reconcileWithAlertManager(context.Background())

	got, err := p.store.Get(tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != ticket.StatusResolved {
		t.Errorf("ticket with all fingerprints absent must be resolved; got status=%q", got.Status)
	}
	if !strings.Contains(got.Analysis, "fp-A,fp-B,fp-C") {
		t.Errorf("resolution reason should reference all fingerprints; got analysis=%q", got.Analysis)
	}
}
