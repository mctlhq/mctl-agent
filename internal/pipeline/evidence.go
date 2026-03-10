package pipeline

import (
	"context"
	"time"

	"github.com/mctlhq/mctl-agent/internal/ticket"
)

// collectEvidence gathers platform data for a ticket via mctl-api.
func (p *Pipeline) collectEvidence(ctx context.Context, t *ticket.Ticket) {
	_ = ctx // available for future cancellation support
	now := time.Now().UTC()

	// Status from ArgoCD.
	if status, err := p.apiClient.GetServiceStatus(t.Tenant, t.Service); err == nil {
		_ = p.store.AddEvidence(t.ID, ticket.Evidence{
			Type:        "argocd_status",
			Content:     ticket.EvidenceJSON(status),
			CollectedAt: now,
		})
	}

	// Service config.
	if config, err := p.apiClient.GetServiceConfig(t.Tenant, t.Service); err == nil {
		_ = p.store.AddEvidence(t.ID, ticket.Evidence{
			Type:        "config",
			Content:     ticket.EvidenceJSON(config),
			CollectedAt: now,
		})
	}

	// Logs (100 lines, 1 hour).
	if logs, err := p.apiClient.GetServiceLogs(t.Tenant, t.Service, 100, time.Hour); err == nil {
		_ = p.store.AddEvidence(t.ID, ticket.Evidence{
			Type:        "logs",
			Content:     ticket.EvidenceJSON(logs),
			CollectedAt: now,
		})
	}

	// Resource usage.
	if resources, err := p.apiClient.GetResourceUsage(t.Tenant); err == nil {
		_ = p.store.AddEvidence(t.ID, ticket.Evidence{
			Type:        "resources",
			Content:     ticket.EvidenceJSON(resources),
			CollectedAt: now,
		})
	}

	// Audit log.
	if audit, err := p.apiClient.ListAudit(); err == nil {
		_ = p.store.AddEvidence(t.ID, ticket.Evidence{
			Type:        "audit",
			Content:     ticket.EvidenceJSON(audit),
			CollectedAt: now,
		})
	}
}
