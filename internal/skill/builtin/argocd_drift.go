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

// ArgoCDDriftSkill detects ArgoCD apps that are OutOfSync but Healthy.
type ArgoCDDriftSkill struct{}

func NewArgoCDDriftSkill() *ArgoCDDriftSkill { return &ArgoCDDriftSkill{} }

func (s *ArgoCDDriftSkill) Name() string    { return "argocd_drift" }
func (s *ArgoCDDriftSkill) Version() string { return "1.0.0" }

func (s *ArgoCDDriftSkill) Description() string {
	return "Detects ArgoCD apps that are OutOfSync but Healthy (benign drift)"
}

func (s *ArgoCDDriftSkill) RequiredCapabilities() []skill.CapabilityID {
	return []skill.CapabilityID{skill.CapReadStatus, skill.CapSendNotify}
}

func (s *ArgoCDDriftSkill) Match(_ context.Context, _ *ticket.Ticket, ev skill.EvidenceSet) skill.MatchResult {
	status := ev.Get("argocd_status")
	if status == "" {
		return skill.MatchResult{}
	}

	if strings.Contains(status, `"syncStatus":"OutOfSync"`) &&
		strings.Contains(status, `"health":"Healthy"`) {
		return skill.MatchResult{
			Matched:    true,
			Confidence: 0.80,
			Priority:   50,
			Reason:     "ArgoCD app is OutOfSync but Healthy",
		}
	}
	return skill.MatchResult{}
}

func (s *ArgoCDDriftSkill) Diagnose(_ context.Context, _ *ticket.Ticket, _ skill.EvidenceSet) (*skill.DiagnosisResult, error) {
	return &skill.DiagnosisResult{
		Diagnosis:  "ArgoCD app is OutOfSync but healthy. Sync will happen automatically or may be intentional drift.",
		Confidence: ticket.ConfidenceLow,
		Fixable:    false,
		SkillName:  s.Name(),
	}, nil
}

func (s *ArgoCDDriftSkill) Fix(_ context.Context, _ *ticket.Ticket, _ *skill.DiagnosisResult) (*skill.FixResult, error) {
	return nil, nil // Not auto-fixable.
}
