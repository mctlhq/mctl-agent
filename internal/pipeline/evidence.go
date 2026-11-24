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
	"log/slog"
	"strings"
	"time"

	"github.com/mctlhq/mctl-agent/internal/ticket"
)

// collectEvidence gathers platform data for a ticket via mctl-api.
func (p *Pipeline) collectEvidence(ctx context.Context, t *ticket.Ticket) {
	now := time.Now().UTC()

	// Workflow specific evidence.
	if t.Type == ticket.TypeWorkflowFailed && t.Service != "" {
		if wf, err := p.apiClient.GetWorkflow(ctx, t.Service); err == nil {
			_ = p.store.AddEvidence(ctx, t.ID, ticket.Evidence{
				Type:        "workflow_live_status",
				Content:     ticket.EvidenceJSON(wf),
				CollectedAt: now,
			})
		}
	}

	// Status from ArgoCD.
	if t.Service != "" && t.Type != ticket.TypeWorkflowFailed {
		if status, err := p.apiClient.GetServiceStatus(ctx, t.Tenant, t.Service); err == nil {
			_ = p.store.AddEvidence(ctx, t.ID, ticket.Evidence{
				Type:        "argocd_status",
				Content:     ticket.EvidenceJSON(status),
				CollectedAt: now,
			})
		}
	}

	// Service config.
	if config, err := p.apiClient.GetServiceConfig(ctx, t.Tenant, t.Service); err == nil && t.Type != ticket.TypeWorkflowFailed {
		_ = p.store.AddEvidence(ctx, t.ID, ticket.Evidence{
			Type:        "config",
			Content:     ticket.EvidenceJSON(config),
			CollectedAt: now,
		})
	}

	// Logs (100 lines, 1 hour).
	if logs, err := p.apiClient.GetServiceLogs(ctx, t.Tenant, t.Service, 100, time.Hour); err == nil {
		_ = p.store.AddEvidence(ctx, t.ID, ticket.Evidence{
			Type:        "logs",
			Content:     ticket.EvidenceJSON(logs),
			CollectedAt: now,
		})
	}

	// Pod state and Warning events. Logs alone cannot say that a container
	// is restarting, with which exit code, or that the kubelet is backing
	// off: those are pod status and events, never lines the application
	// writes. A failed read adds no evidence at all, so a skill never takes
	// "could not read" for "no restarts".
	if t.Service != "" && t.Type != ticket.TypeWorkflowFailed {
		if pods, err := p.apiClient.GetServicePods(ctx, t.Tenant, t.Service); err == nil {
			_ = p.store.AddEvidence(ctx, t.ID, ticket.Evidence{
				Type:        "pods",
				Content:     string(pods),
				CollectedAt: now,
			})
		} else {
			slog.DebugContext(ctx, "pod state not collected", "ticket_id", t.ID, "error", err)
		}
		if events, err := p.apiClient.ListServiceEvents(ctx, t.Tenant, t.Service); err == nil {
			_ = p.store.AddEvidence(ctx, t.ID, ticket.Evidence{
				Type:        "events",
				Content:     string(events),
				CollectedAt: now,
			})
		} else {
			slog.DebugContext(ctx, "events not collected", "ticket_id", t.ID, "error", err)
		}
	}

	// Resource usage.
	if resources, err := p.apiClient.GetResourceUsage(ctx, t.Tenant); err == nil {
		_ = p.store.AddEvidence(ctx, t.ID, ticket.Evidence{
			Type:        "resources",
			Content:     ticket.EvidenceJSON(resources),
			CollectedAt: now,
		})
	}

	// Audit log.
	if audit, err := p.apiClient.ListAudit(ctx); err == nil {
		_ = p.store.AddEvidence(ctx, t.ID, ticket.Evidence{
			Type:        "audit",
			Content:     ticket.EvidenceJSON(audit),
			CollectedAt: now,
		})
	}
}

// collectHistoricalEvidence adds resolved similar incidents as evidence,
// giving the LLM context on how this type of issue has been handled before.
func (p *Pipeline) collectHistoricalEvidence(ctx context.Context, t *ticket.Ticket) {
	similar, err := p.store.FindSimilar(ctx, t.Type, t.ID, 3)
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
			fmt.Fprintf(&sb, "  %s: %s\n", pastFixLabel(past), past.ProposedFix)
		}
		if past.Confidence != "" {
			fmt.Fprintf(&sb, "  Confidence: %s\n", past.Confidence)
		}
	}

	_ = p.store.AddEvidence(ctx, t.ID, ticket.Evidence{
		Type:        "history",
		Content:     sb.String(),
		CollectedAt: time.Now().UTC(),
	})
}

// pastFixLabel names what a past ticket's ProposedFix is. A resolved ticket
// does not record whether its fix was applied: ProposedFix is also kept for a
// suggestion nobody acted on (MEDIUM confidence, dry-run), for a PR that was
// never merged, and for a ticket the poller closed because the application
// recovered by itself. Calling all of those "Fix applied" told the next
// diagnosis that an untried change had worked.
func pastFixLabel(past *ticket.Ticket) string {
	if past.PRNumber != 0 || past.PRURL != "" {
		return "Fix proposed in a PR (not recorded whether it was merged)"
	}
	return "Fix proposed, no PR opened (not applied by the agent)"
}
