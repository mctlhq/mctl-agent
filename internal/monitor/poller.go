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
	"fmt"
	"log/slog"
	"time"

	"github.com/mctlhq/mctl-agent/internal/mctlclient"
	"github.com/mctlhq/mctl-agent/internal/ticket"
)

// Poller periodically checks service health via mctl-api.
type Poller struct {
	client   *mctlclient.Client
	store    *ticket.Store
	onTicket func(*ticket.Ticket)
	// StaleAfter enables auto-resolution of open tickets whose UpdatedAt
	// has not advanced within this window. Zero disables the GC pass.
	StaleAfter time.Duration
	// AnalyzingAfter enables auto-resolution of tickets stuck in StatusAnalyzing
	// beyond this window. Zero disables this GC pass.
	AnalyzingAfter time.Duration
	// FixProposedAfter enables auto-resolution of tickets stuck in StatusFixProposed
	// beyond this window. Zero disables this GC pass.
	FixProposedAfter time.Duration
}

// NewPoller creates a new service health poller.
func NewPoller(client *mctlclient.Client, store *ticket.Store, onTicket func(*ticket.Ticket)) *Poller {
	return &Poller{client: client, store: store, onTicket: onTicket}
}

// Run starts the polling loop. Blocks until ctx is cancelled.
func (p *Poller) Run(ctx context.Context, interval time.Duration) {
	slog.Info("poller starting", "interval", interval)

	// Run immediately on start, then on interval.
	p.poll()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			slog.Info("poller stopped")
			return
		case <-ticker.C:
			p.poll()
		}
	}
}

// refreshState captures which ArgoCD service health refreshes succeeded
// this poll cycle. resolveStale uses it to avoid closing
// TypeArgoCDDegraded tickets whose underlying service status could not
// be checked — a stalled UpdatedAt in that case is telemetry gap, not
// recovery. AlertManager-based tickets are independent of mctl-api
// health and are always eligible for GC.
type refreshState struct {
	// allUnknown is true when the poller could not enumerate services
	// (ListServices failed). Every ArgoCDDegraded ticket must be skipped
	// in that cycle.
	allUnknown bool
	// failedServices holds "tenant/service" keys for services whose
	// GetServiceStatus call failed. Only consulted when allUnknown=false.
	failedServices map[string]bool
}

func (rs refreshState) argoRefreshed(tenant, service string) bool {
	if rs.allUnknown {
		return false
	}
	return !rs.failedServices[tenant+"/"+service]
}

func (p *Poller) poll() {
	state := p.pollDegraded()
	p.resolveStale(state)
}

// pollDegraded scans all services for ArgoCD Degraded/Missing health,
// creating or touching tickets, and returns the refresh outcome for
// downstream GC gating.
func (p *Poller) pollDegraded() refreshState {
	services, err := p.client.ListServices()
	if err != nil {
		slog.Error("poller: failed to list services", "error", err)
		return refreshState{allUnknown: true}
	}

	slog.Debug("poller: checking services", "count", len(services))

	failed := map[string]bool{}
	for _, svc := range services {
		team := svc.Team
		app := svc.App
		if team == "" || app == "" {
			continue
		}

		status, err := p.client.GetServiceStatus(team, app)
		if err != nil {
			slog.Warn("poller: failed to get status; will skip stale GC for this service",
				"team", team, "app", app, "error", err)
			failed[team+"/"+app] = true
			continue
		}

		if status.ArgoCD == nil {
			continue
		}

		health := status.ArgoCD.Health
		if health != "Degraded" && health != "Missing" {
			continue
		}

		// Dedup: check for existing open ticket.
		existing, err := p.store.FindDuplicate(team, app, ticket.TypeArgoCDDegraded)
		if err != nil {
			slog.Error("poller: dedup check failed", "error", err)
			continue
		}
		if existing != nil {
			// Bump UpdatedAt so stale-ticket GC sees the condition is
			// still active.
			if err := p.store.Touch(existing.ID); err != nil {
				slog.Error("poller: failed to touch ticket", "error", err, "id", existing.ID)
			}
			continue
		}

		t := &ticket.Ticket{
			Source:   ticket.SourcePolling,
			Type:     ticket.TypeArgoCDDegraded,
			Tenant:   team,
			Service:  app,
			Summary:  "ArgoCD app " + team + "-" + app + " health: " + health,
			Severity: ticket.SeverityWarning,
		}

		if err := p.store.Create(t); err != nil {
			slog.Error("poller: failed to create ticket", "error", err, "team", team, "app", app)
			continue
		}

		// Store status as evidence.
		_ = p.store.AddEvidence(t.ID, ticket.Evidence{
			Type:        "argocd_status",
			Content:     ticket.EvidenceJSON(status.ArgoCD),
			CollectedAt: time.Now().UTC(),
		})

		slog.Info("poller: ticket created",
			"id", t.ID, "team", team, "app", app, "health", health)

		if p.onTicket != nil {
			p.onTicket(t)
		}
	}
	return refreshState{failedServices: failed}
}

// heartbeatTicketTypes lists ticket types whose creation path has a
// paired Touch-on-duplicate heartbeat in this package. Only these types
// are eligible for auto-resolve by UpdatedAt age — for any other type,
// a stale UpdatedAt does not necessarily mean the underlying signal
// stopped, because the source may not refresh UpdatedAt on recurring
// events.
//
// Adding a new ticket type here requires that its source also calls
// store.Touch on every duplicate/repeat event.
var heartbeatTicketTypes = map[string]bool{
	ticket.TypeArgoCDDegraded:      true, // Touch in pollDegraded
	ticket.TypePodCrashloop:        true, // Touch in alerthandler
	ticket.TypeResourceLimit:       true, // Touch in alerthandler
	ticket.TypeWorkflowFailed:      true, // Touch in alerthandler
	ticket.TypeGeneric:             true, // Touch in alerthandler
	ticket.TypeGitHubActionsFailed: true, // Touch in github_webhook
}

// heartbeatTicketSources lists ticket sources that emit heartbeats.
// SourceManual (used by Pipeline.TriggerAnalysis for user-initiated
// investigations) has no recurring signal and must never be auto-
// resolved by age — otherwise a paused pipeline or a long-running
// investigation would be silently closed.
var heartbeatTicketSources = map[string]bool{
	ticket.SourceAlertManager:  true,
	ticket.SourcePolling:       true,
	ticket.SourceGitHubWebhook: true,
}

// eligibleType reports whether the given ticket type is GC-eligible
// (i.e. its source emits a Touch heartbeat on every recurring event).
func (p *Poller) eligibleType(typ string) bool {
	return heartbeatTicketTypes[typ]
}

// eligibleSource reports whether the given ticket source emits heartbeats
// that keep UpdatedAt current while the underlying signal is active.
func (p *Poller) eligibleSource(src string) bool {
	return heartbeatTicketSources[src]
}

// resolveStale closes open tickets whose UpdatedAt has not advanced
// within StaleAfter. UpdatedAt is refreshed on each duplicate-signal
// hit (see alerthandler.go, github_webhook.go, and pollDegraded
// above), so a frozen UpdatedAt means the underlying signal has
// stopped recurring.
//
// Only tickets in StatusOpen are considered — tickets the pipeline is
// actively analyzing or fixing keep their UpdatedAt current through
// their own status transitions, and we never want to close those out
// from under the pipeline.
//
// Only ticket types listed in heartbeatTicketTypes are GC-eligible.
// Anything else might not refresh UpdatedAt on recurring signals and
// would be false-resolved on age alone.
//
// For TypeArgoCDDegraded tickets, GC is further gated on the current
// cycle having reached the underlying service. If mctl-api could not
// enumerate services or the specific service's health fetch failed,
// the ticket is skipped this cycle — its stalled UpdatedAt is a
// telemetry gap, not real recovery. The other heartbeat-enabled types
// do not depend on mctl-api reachability and are GC'd purely by
// UpdatedAt, so a partial poller outage does not block noise cleanup.
func (p *Poller) resolveStale(state refreshState) {
	if p.StaleAfter <= 0 {
		return
	}
	open, err := p.store.ListOpen()
	if err != nil {
		slog.Error("poller: failed to list open tickets for stale GC", "error", err)
		return
	}

	thresholds := map[string]time.Duration{
		ticket.StatusOpen:        p.StaleAfter,
		ticket.StatusAnalyzing:   p.AnalyzingAfter,
		ticket.StatusFixProposed: p.FixProposedAfter,
	}

	for _, t := range open {
		cutoff, ok := thresholds[t.Status]
		if !ok || cutoff <= 0 {
			continue
		}
		if !p.eligibleSource(t.Source) {
			slog.Debug("poller: skipping stale GC, ticket source has no heartbeat",
				"id", t.ID, "source", t.Source)
			continue
		}
		if !p.eligibleType(t.Type) {
			slog.Debug("poller: skipping stale GC, ticket type has no heartbeat",
				"id", t.ID, "type", t.Type)
			continue
		}
		if t.Type == ticket.TypeArgoCDDegraded && !state.argoRefreshed(t.Tenant, t.Service) {
			slog.Debug("poller: skipping ArgoCDDegraded stale GC, refresh not confirmed",
				"id", t.ID, "tenant", t.Tenant, "service", t.Service)
			continue
		}
		if time.Since(t.UpdatedAt) < cutoff {
			continue
		}

		age := time.Since(t.UpdatedAt).Round(time.Hour)

		if t.Status == ticket.StatusOpen {
			// StatusOpen: existing behavior — no reason appended to analysis.
			resolved, err := p.store.ResolveByID(t.ID)
			if err != nil {
				slog.Error("poller: failed to auto-resolve stale ticket",
					"error", err, "id", t.ID)
				continue
			}
			if !resolved {
				slog.Debug("poller: stale GC no-op, ticket advanced concurrently",
					"id", t.ID)
				continue
			}
			slog.Info("poller: auto-resolved stale ticket",
				"id", t.ID, "tenant", t.Tenant, "service", t.Service,
				"type", t.Type, "last_updated", t.UpdatedAt, "stale_after", p.StaleAfter)
		} else {
			reason := fmt.Sprintf(
				"Auto-resolved by stale TTL GC (status=%s, age=%s, threshold=%s)",
				t.Status, age, cutoff,
			)
			resolved, err := p.store.ResolveByIDFromStatus(t.ID, t.Status, reason)
			if err != nil {
				slog.Warn("poller: stale TTL resolve failed", "ticket", t.ID, "err", err)
				continue
			}
			if !resolved {
				slog.Debug("poller: stale GC no-op, ticket advanced concurrently",
					"id", t.ID)
				continue
			}
			slog.Info("poller: stale TTL resolved",
				"ticket", t.ID, "status", t.Status, "age", age, "threshold", cutoff)
		}
	}
}
