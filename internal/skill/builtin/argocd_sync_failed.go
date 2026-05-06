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

package builtin

import (
	"context"
	"strings"

	"github.com/mctlhq/mctl-agent/internal/skill"
	"github.com/mctlhq/mctl-agent/internal/ticket"
)

// ArgoCDSyncFailedSkill diagnoses ArgoCD apps that are Degraded with a sync
// failure — distinct from ArgoCDDriftSkill, which only handles benign
// OutOfSync+Healthy drift. Specific signatures we recognize from the
// app status `message` field (mctlclient.ArgoStatus.Message):
//
//   - CRD storedVersion conflict left over from a chart major-version
//     revert (e.g. ESO 2.x → 0.10.x). Recovery is documented in
//     ~/.claude/projects/.../memory/reference_eso_storedversion_recovery.md.
//   - "request to convert CR from an invalid group/version" — managedFields
//     poisoning from the same class of incident.
//
// Fix is intentionally nil: recovery requires kubectl operations on the
// cluster (not a gitops PR), so the skill produces a Telegram-ready
// diagnosis with the exact commands and waits for human approval.
type ArgoCDSyncFailedSkill struct{}

func NewArgoCDSyncFailedSkill() *ArgoCDSyncFailedSkill { return &ArgoCDSyncFailedSkill{} }

func (s *ArgoCDSyncFailedSkill) Name() string    { return "argocd_sync_failed" }
func (s *ArgoCDSyncFailedSkill) Version() string { return "1.0.0" }

func (s *ArgoCDSyncFailedSkill) Description() string {
	return "Diagnoses ArgoCD apps stuck Degraded with sync failure (CRD storedVersion conflicts, conversion errors)"
}

func (s *ArgoCDSyncFailedSkill) RequiredCapabilities() []skill.CapabilityID {
	return []skill.CapabilityID{skill.CapReadStatus, skill.CapSendNotify}
}

func (s *ArgoCDSyncFailedSkill) Match(_ context.Context, _ *ticket.Ticket, ev skill.EvidenceSet) skill.MatchResult {
	status := ev.Get("argocd_status")
	if status == "" {
		return skill.MatchResult{}
	}

	// Don't compete with ArgoCDDriftSkill on benign drift.
	if strings.Contains(status, `"health":"Healthy"`) {
		return skill.MatchResult{}
	}

	degraded := strings.Contains(status, `"health":"Degraded"`)
	outOfSync := strings.Contains(status, `"syncStatus":"OutOfSync"`)
	knownPattern := storedVersionConflict(status) || invalidGroupVersion(status)

	switch {
	case knownPattern:
		return skill.MatchResult{
			Matched:    true,
			Confidence: 0.92,
			Priority:   80,
			Reason:     "ArgoCD app sync failed with a known CRD storedVersion / conversion signature",
		}
	case degraded && outOfSync:
		return skill.MatchResult{
			Matched:    true,
			Confidence: 0.75,
			Priority:   60,
			Reason:     "ArgoCD app is Degraded and OutOfSync — sync likely failing",
		}
	case degraded:
		return skill.MatchResult{
			Matched:    true,
			Confidence: 0.65,
			Priority:   55,
			Reason:     "ArgoCD app is Degraded",
		}
	}
	return skill.MatchResult{}
}

func (s *ArgoCDSyncFailedSkill) Diagnose(_ context.Context, _ *ticket.Ticket, ev skill.EvidenceSet) (*skill.DiagnosisResult, error) {
	status := ev.Get("argocd_status")

	switch {
	case storedVersionConflict(status):
		return &skill.DiagnosisResult{
			Diagnosis: "ArgoCD sync is blocked by a CRD `status.storedVersions` conflict — typically the long-tail of a chart major-version revert (e.g. external-secrets 2.x → 0.10.x). " +
				"Manual recovery (do not auto-apply): " +
				"1) Backup affected CRs. " +
				"2) `kubectl get <crd> -A -o yaml | kubectl replace -f -` to rewrite storage version. " +
				"3) `kubectl patch crd <name> --subresource=status --type=merge -p '{\"status\":{\"storedVersions\":[\"<current-storage>\"]}}'`. " +
				"4) Re-sync the ArgoCD app. " +
				"Full drill: ~/.claude/projects/-Users-dmitriimashkov-PycharmProjects-mctlhq/memory/reference_eso_storedversion_recovery.md",
			Confidence: ticket.ConfidenceHigh,
			Fixable:    false,
			SkillName:  s.Name(),
		}, nil
	case invalidGroupVersion(status):
		return &skill.DiagnosisResult{
			Diagnosis: "ArgoCD sync hits `request to convert CR from an invalid group/version`. CRs have stale `managedFields` referencing an apiVersion no longer in `spec.versions`. " +
				"Manual recovery (do not auto-apply): " +
				"1) Pause autosync on the CRD-owning ArgoCD app. " +
				"2) Apply a temp CRD that re-adds the missing version with `conversion.strategy=None` and uniform schemas across all served versions. " +
				"3) Bulk `kubectl apply --server-side --force-conflicts --field-manager=storage-cleanup` over every CR of the affected kind. " +
				"4) Resume autosync — chart-managed CRD shape returns. " +
				"5) `kubectl rollout restart deployment argocd-redis -n argocd` if app health stays stuck on a stale lastTransitionTime. " +
				"DO NOT trigger sync on parent apps while the temp CRD is active — that distributes the v1 markers to all CRs of the kind. " +
				"Full drill: ~/.claude/projects/-Users-dmitriimashkov-PycharmProjects-mctlhq/memory/reference_eso_storedversion_recovery.md",
			Confidence: ticket.ConfidenceHigh,
			Fixable:    false,
			SkillName:  s.Name(),
		}, nil
	}

	return &skill.DiagnosisResult{
		Diagnosis: "ArgoCD application is Degraded with no auto-recognizable signature. Check `argocd app get` and the latest sync operation for the failure reason. " +
			"Common causes: failed pod readiness, RBAC drift, image pull errors on a referenced workload, or a transient webhook timeout. Inspect resource-tree health to find the offending child resource.",
		Confidence: ticket.ConfidenceLow,
		Fixable:    false,
		SkillName:  s.Name(),
	}, nil
}

func (s *ArgoCDSyncFailedSkill) Fix(_ context.Context, _ *ticket.Ticket, _ *skill.DiagnosisResult) (*skill.FixResult, error) {
	// Recovery is cluster-side (kubectl), not a gitops PR. Manual approval
	// required — diagnosis is delivered to Telegram for an operator to run.
	return nil, nil
}

func storedVersionConflict(status string) bool {
	return strings.Contains(status, "must remain in spec.versions") ||
		strings.Contains(status, "missing from spec.versions")
}

func invalidGroupVersion(status string) bool {
	return strings.Contains(status, "request to convert CR from an invalid group/version")
}
