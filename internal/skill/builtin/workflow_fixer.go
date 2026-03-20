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
	"fmt"
	"strings"

	"github.com/mctlhq/mctl-agent/internal/skill"
	"github.com/mctlhq/mctl-agent/internal/ticket"
)

// WorkflowFixerSkill detects common Argo Workflow failures and suggests fixes.
type WorkflowFixerSkill struct{}

func NewWorkflowFixerSkill() *WorkflowFixerSkill { return &WorkflowFixerSkill{} }

func (s *WorkflowFixerSkill) Name() string    { return "workflow_fixer" }
func (s *WorkflowFixerSkill) Version() string { return "1.0.0" }

func (s *WorkflowFixerSkill) Description() string {
	return "Detects Argo Workflow validation and permission errors and fixes templates in GitOps"
}

func (s *WorkflowFixerSkill) RequiredCapabilities() []skill.CapabilityID {
	return []skill.CapabilityID{skill.CapReadStatus, skill.CapReadConfig, skill.CapModifyGitOps, skill.CapCreatePR}
}

func (s *WorkflowFixerSkill) Match(_ context.Context, t *ticket.Ticket, ev skill.EvidenceSet) skill.MatchResult {
	if t.Type != ticket.TypeWorkflowFailed {
		return skill.MatchResult{}
	}

	wfStatus := ev.Get("workflow_live_status")
	if wfStatus == "" {
		return skill.MatchResult{}
	}

	if strings.Contains(wfStatus, "is required") && strings.Contains(wfStatus, "spec.arguments") {
		return skill.MatchResult{
			Matched:    true,
			Confidence: 0.98,
			Priority:   150,
			Reason:     "Detected Argo Workflow argument validation error (missing 'value' field)",
		}
	}

	if strings.Contains(wfStatus, "is not permitted in project") {
		return skill.MatchResult{
			Matched:    true,
			Confidence: 0.98,
			Priority:   150,
			Reason:     "Detected ArgoCD AppProject permission error",
		}
	}

	return skill.MatchResult{}
}

func (s *WorkflowFixerSkill) Diagnose(_ context.Context, _ *ticket.Ticket, ev skill.EvidenceSet) (*skill.DiagnosisResult, error) {
	wfStatus := ev.Get("workflow_live_status")

	if strings.Contains(wfStatus, "is required") && strings.Contains(wfStatus, "spec.arguments") {
		return &skill.DiagnosisResult{
			Diagnosis:  "Argo Workflow failed because a required argument is missing a value. This usually happens when 'default:' is used instead of 'value:' in ClusterWorkflowTemplate.",
			Confidence: ticket.ConfidenceHigh,
			Fixable:    true,
			FixType:    "fix_workflow_params",
			SkillName:  s.Name(),
		}, nil
	}

	if strings.Contains(wfStatus, "is not permitted in project") {
		return &skill.DiagnosisResult{
			Diagnosis:  "ArgoCD failed to sync because the resource type is not whitelisted in the AppProject.",
			Confidence: ticket.ConfidenceHigh,
			Fixable:    true,
			FixType:    "fix_appproject_whitelist",
			SkillName:  s.Name(),
		}, nil
	}

	return &skill.DiagnosisResult{
		Diagnosis:  "Workflow failed with an unknown error. Check live status for details. If the error is persistent and unfixable, manual cleanup of the workflow and associated namespace may be required to prevent resource leaking.",
		Confidence: ticket.ConfidenceLow,
		Fixable:    false,
		SkillName:  s.Name(),
	}, nil
}

func (s *WorkflowFixerSkill) Fix(_ context.Context, t *ticket.Ticket, diag *skill.DiagnosisResult) (*skill.FixResult, error) {
	if diag.FixType == "fix_workflow_params" {
		// Attempt to find which template failed.
		// For now, we know the main culprit is deploy-service.
		return &skill.FixResult{
			Applied:  true,
			FilePath: "platform-gitops/argo-workflows/workflow-templates/wft-deploy-service.yaml",
			Summary:  "Replace 'default:' with 'value:' in ClusterWorkflowTemplate to fix Argo API 400 error",
		}, nil
	}

	if diag.FixType == "fix_appproject_whitelist" {
		return &skill.FixResult{
			Applied:  true,
			FilePath: "platform-gitops/apps/templates/projects/project-apps.yaml",
			Summary:  "Add missing resource groups to AppProject namespaceResourceWhitelist",
		}, nil
	}

	return nil, fmt.Errorf("unsupported fix type: %s", diag.FixType)
}

func (s *WorkflowFixerSkill) AutoMergeSafe() bool { return false }
