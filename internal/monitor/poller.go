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

func (p *Poller) poll() {
	// Stale-ticket GC is gated on a successful health refresh. If mctl-api
	// is unreachable, open degraded tickets do not get their UpdatedAt
	// touched, so running the GC would auto-resolve them based on a
	// telemetry outage rather than real recovery.
	if !p.pollDegraded() {
		slog.Warn("poller: skipping stale-ticket GC, pollDegraded did not complete")
		return
	}
	p.resolveStale()
}

// pollDegraded reports whether the degraded-service scan completed
// successfully for every service. A false return — triggered by
// ListServices failing or ANY per-service GetServiceStatus failing —
// means open Degraded tickets could not all be Touch-refreshed this
// cycle, so downstream stale-ticket GC must be skipped to avoid
// auto-resolving incidents whose UpdatedAt stalled on a transient
// status-endpoint error rather than real recovery.
func (p *Poller) pollDegraded() bool {
	services, err := p.client.ListServices()
	if err != nil {
		slog.Error("poller: failed to list services", "error", err)
		return false
	}

	slog.Debug("poller: checking services", "count", len(services))

	allRefreshed := true
	for _, svc := range services {
		team := svc.Team
		app := svc.App
		if team == "" || app == "" {
			continue
		}

		status, err := p.client.GetServiceStatus(team, app)
		if err != nil {
			slog.Warn("poller: failed to get status; will skip stale GC this cycle",
				"team", team, "app", app, "error", err)
			allRefreshed = false
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
	return allRefreshed
}

// resolveStale closes open tickets whose UpdatedAt has not advanced
// within StaleAfter. UpdatedAt is refreshed on each duplicate-alert hit
// (see alerthandler.go and pollDegraded above), so a frozen UpdatedAt
// means the underlying signal has stopped firing.
//
// Only tickets in StatusOpen are considered — tickets the pipeline is
// actively analyzing or fixing keep their UpdatedAt current through
// their own status transitions, and we never want to close those out
// from under the pipeline.
func (p *Poller) resolveStale() {
	if p.StaleAfter <= 0 {
		return
	}
	open, err := p.store.ListOpen()
	if err != nil {
		slog.Error("poller: failed to list open tickets for stale GC", "error", err)
		return
	}
	cutoff := time.Now().UTC().Add(-p.StaleAfter)
	for _, t := range open {
		if t.Status != ticket.StatusOpen {
			continue
		}
		if !t.UpdatedAt.Before(cutoff) {
			continue
		}
		if err := p.store.ResolveByID(t.ID); err != nil {
			slog.Error("poller: failed to auto-resolve stale ticket",
				"error", err, "id", t.ID)
			continue
		}
		slog.Info("poller: auto-resolved stale ticket",
			"id", t.ID, "tenant", t.Tenant, "service", t.Service,
			"type", t.Type, "last_updated", t.UpdatedAt, "stale_after", p.StaleAfter)
	}
}
