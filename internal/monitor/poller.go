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
	services, err := p.client.ListServices()
	if err != nil {
		slog.Error("poller: failed to list services", "error", err)
		return
	}

	slog.Debug("poller: checking services", "count", len(services))

	for _, svc := range services {
		team := svc.Team
		app := svc.App
		if team == "" || app == "" {
			continue
		}

		status, err := p.client.GetServiceStatus(team, app)
		if err != nil {
			slog.Debug("poller: failed to get status", "team", team, "app", app, "error", err)
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
}
