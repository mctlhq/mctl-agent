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

package pipeline

import (
	"context"
	"fmt"
	"strings"
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

// collectHistoricalEvidence adds resolved similar incidents as evidence,
// giving the LLM context on how this type of issue has been handled before.
func (p *Pipeline) collectHistoricalEvidence(t *ticket.Ticket) {
	similar, err := p.store.FindSimilar(t.Type, t.ID, 3)
	if err != nil || len(similar) == 0 {
		return
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "Similar past incidents (last 90 days, same type):\n\n")
	for _, past := range similar {
		fmt.Fprintf(&sb, "- %s | %s/%s\n", past.CreatedAt.Format("2006-01-02"), past.Tenant, past.Service)
		if past.Analysis != "" {
			fmt.Fprintf(&sb, "  Diagnosis: %s\n", past.Analysis)
		}
		if past.ProposedFix != "" {
			fmt.Fprintf(&sb, "  Fix applied: %s\n", past.ProposedFix)
		}
		if past.Confidence != "" {
			fmt.Fprintf(&sb, "  Confidence: %s\n", past.Confidence)
		}
	}

	_ = p.store.AddEvidence(t.ID, ticket.Evidence{
		Type:        "history",
		Content:     sb.String(),
		CollectedAt: time.Now().UTC(),
	})
}
