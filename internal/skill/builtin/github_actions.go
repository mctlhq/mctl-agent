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
	"encoding/json"
	"strings"

	"github.com/mctlhq/mctl-agent/internal/skill"
	"github.com/mctlhq/mctl-agent/internal/ticket"
)

// GitHubActionsSkill diagnoses common GitHub Actions CI failures.
type GitHubActionsSkill struct{}

func NewGitHubActionsSkill() *GitHubActionsSkill { return &GitHubActionsSkill{} }

func (s *GitHubActionsSkill) Name() string    { return "github_actions" }
func (s *GitHubActionsSkill) Version() string { return "1.0.0" }

func (s *GitHubActionsSkill) Description() string {
	return "Diagnoses GitHub Actions CI/CD failures from workflow_run webhook data"
}

func (s *GitHubActionsSkill) RequiredCapabilities() []skill.CapabilityID {
	return []skill.CapabilityID{skill.CapSendNotify}
}

func (s *GitHubActionsSkill) Match(_ context.Context, t *ticket.Ticket, _ skill.EvidenceSet) skill.MatchResult {
	if t.Type == ticket.TypeGitHubActionsFailed {
		return skill.MatchResult{
			Matched:    true,
			Confidence: 0.80,
			Priority:   85,
			Reason:     "GitHub Actions failure detected",
		}
	}
	return skill.MatchResult{}
}

func (s *GitHubActionsSkill) Diagnose(_ context.Context, t *ticket.Ticket, ev skill.EvidenceSet) (*skill.DiagnosisResult, error) {
	raw := ev.Get("github_workflow_run")
	var data map[string]string
	_ = json.Unmarshal([]byte(raw), &data)

	workflow := data["workflow"]
	runURL := data["run_url"]

	failureType := classifyCIFailure(workflow, t.Summary)
	diagnosis := failureType + " Check the workflow run for details."
	if runURL != "" {
		diagnosis += " Run URL: " + runURL
	}

	return &skill.DiagnosisResult{
		Diagnosis:  diagnosis,
		Confidence: ticket.ConfidenceMedium,
		Fixable:    false,
		SkillName:  s.Name(),
	}, nil
}

func (s *GitHubActionsSkill) Fix(_ context.Context, _ *ticket.Ticket, _ *skill.DiagnosisResult) (*skill.FixResult, error) {
	return nil, nil
}

func classifyCIFailure(workflow, summary string) string {
	lower := strings.ToLower(workflow + " " + summary)
	switch {
	case containsAny(lower, "test", "spec", "jest", "pytest", "go test"):
		return "Test failure in CI."
	case containsAny(lower, "build", "compile", "docker"):
		return "Build/compile failure in CI."
	case containsAny(lower, "lint", "fmt", "vet", "eslint", "golangci"):
		return "Lint/format check failure in CI."
	case containsAny(lower, "deploy", "release", "publish"):
		return "Deployment/release pipeline failure."
	default:
		return "CI workflow failure."
	}
}
