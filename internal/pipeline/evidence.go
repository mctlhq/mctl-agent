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

	"github.com/mctlhq/mctl-agent/internal/fixer"
	"github.com/mctlhq/mctl-agent/internal/ticket"
)

// gitopsBumpLookback is how far back the history of a service's values file
// is read for image bumps. It is wider than any skill's own "recent" window
// on purpose: the evidence records what was seen, the skill decides what
// counts as recent.
const gitopsBumpLookback = time.Hour

// gitopsBumpReadTimeout bounds the GitHub read. The GitHub client has no
// timeout of its own, and evidence collection must not hold a pipeline slot
// for the whole diagnosis deadline over one optional source.
const gitopsBumpReadTimeout = 20 * time.Second

// collectEvidence gathers platform data for a ticket via mctl-api.
func (p *Pipeline) collectEvidence(ctx context.Context, t *ticket.Ticket) {
	_ = ctx // available for future cancellation support
	now := time.Now().UTC()

	// Workflow specific evidence.
	if t.Type == ticket.TypeWorkflowFailed && t.Service != "" {
		if wf, err := p.apiClient.GetWorkflow(t.Service); err == nil {
			_ = p.store.AddEvidence(ctx, t.ID, ticket.Evidence{
				Type:        "workflow_live_status",
				Content:     ticket.EvidenceJSON(wf),
				CollectedAt: now,
			})
		}
	}

	// Status from ArgoCD.
	if t.Service != "" && t.Type != ticket.TypeWorkflowFailed {
		if status, err := p.apiClient.GetServiceStatus(t.Tenant, t.Service); err == nil {
			_ = p.store.AddEvidence(ctx, t.ID, ticket.Evidence{
				Type:        "argocd_status",
				Content:     ticket.EvidenceJSON(status),
				CollectedAt: now,
			})
		}
	}

	// Service config.
	if config, err := p.apiClient.GetServiceConfig(t.Tenant, t.Service); err == nil && t.Type != ticket.TypeWorkflowFailed {
		_ = p.store.AddEvidence(ctx, t.ID, ticket.Evidence{
			Type:        "config",
			Content:     ticket.EvidenceJSON(config),
			CollectedAt: now,
		})
	}

	// Logs (100 lines, 1 hour).
	if logs, err := p.apiClient.GetServiceLogs(t.Tenant, t.Service, 100, time.Hour); err == nil {
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
		if pods, err := p.apiClient.GetServicePods(t.Tenant, t.Service); err == nil {
			_ = p.store.AddEvidence(ctx, t.ID, ticket.Evidence{
				Type:        "pods",
				Content:     string(pods),
				CollectedAt: now,
			})
		} else {
			slog.DebugContext(ctx, "pod state not collected", "ticket_id", t.ID, "error", err)
		}
		if events, err := p.apiClient.ListServiceEvents(t.Tenant, t.Service); err == nil {
			_ = p.store.AddEvidence(ctx, t.ID, ticket.Evidence{
				Type:        "events",
				Content:     string(events),
				CollectedAt: now,
			})
		} else {
			slog.DebugContext(ctx, "events not collected", "ticket_id", t.ID, "error", err)
		}
	}

	// Image bumps made through GitOps. Preview and release rollouts are a
	// commit to the service's values file, which mctl-api's audit never sees.
	if t.Type == ticket.TypePodCrashloop {
		p.collectGitOpsBumps(ctx, t, now)
	}

	// Resource usage.
	if resources, err := p.apiClient.GetResourceUsage(t.Tenant); err == nil {
		_ = p.store.AddEvidence(ctx, t.ID, ticket.Evidence{
			Type:        "resources",
			Content:     ticket.EvidenceJSON(resources),
			CollectedAt: now,
		})
	}

	// Audit log.
	if audit, err := p.apiClient.ListAudit(); err == nil {
		_ = p.store.AddEvidence(ctx, t.ID, ticket.Evidence{
			Type:        "audit",
			Content:     ticket.EvidenceJSON(audit),
			CollectedAt: now,
		})
	}
}

// collectGitOpsBumps records the recent image bumps of the ticket's own
// values file as "gitops_bumps" evidence.
//
// The evidence is stored only after a complete read, and then also when it
// found nothing: an empty list is an observation. A read that failed, or
// that could not be made at all (no GitHub client, a platform service whose
// manifest is outside the path allowlist), stores nothing, so a skill can
// tell "no bump" from "not looked at" by the evidence being there or not.
func (p *Pipeline) collectGitOpsBumps(ctx context.Context, t *ticket.Ticket, now time.Time) {
	if p.github == nil {
		return
	}
	path := fixer.DetectFilePath(t.Tenant, t.Service)
	if path == "" {
		slog.DebugContext(ctx, "gitops bumps not collected: no values file for this service",
			"ticket_id", t.ID, "tenant", t.Tenant, "service", t.Service)
		return
	}
	readCtx, cancel := context.WithTimeout(ctx, gitopsBumpReadTimeout)
	defer cancel()
	history, err := p.github.RecentImageBumps(readCtx, path, now.Add(-gitopsBumpLookback))
	if err != nil {
		slog.WarnContext(ctx, "gitops bumps not collected", "ticket_id", t.ID, "path", path, "error", err)
		return
	}
	_ = p.store.AddEvidence(ctx, t.ID, ticket.Evidence{
		Type:        "gitops_bumps",
		Content:     ticket.EvidenceJSON(history),
		CollectedAt: now,
	})
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
			fmt.Fprintf(&sb, "  Fix applied: %s\n", past.ProposedFix)
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
