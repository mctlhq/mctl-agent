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

// ScaleUpSkill detects when HPA is maxed out and latency is high, suggesting max replica increase.
type ScaleUpSkill struct{}

func NewScaleUpSkill() *ScaleUpSkill { return &ScaleUpSkill{} }

func (s *ScaleUpSkill) Name() string    { return "scale_up" }
func (s *ScaleUpSkill) Version() string { return "1.0.0" }

func (s *ScaleUpSkill) Description() string {
	return "Detects HPA at max replicas with high load and suggests scaling up"
}

func (s *ScaleUpSkill) RequiredCapabilities() []skill.CapabilityID {
	return []skill.CapabilityID{skill.CapReadLogs, skill.CapReadStatus, skill.CapModifyGitOps, skill.CapCreatePR}
}

func (s *ScaleUpSkill) Match(_ context.Context, _ *ticket.Ticket, ev skill.EvidenceSet) skill.MatchResult {
	logs := ev.Get("logs") + "\n" + ev.Get("alert")
	if containsAny(logs,
		"FailedComputeMetricsReplicas",
		"unable to scale",
		"max replicas reached",
		"ScaleUpLimited",
	) {
		return skill.MatchResult{
			Matched:    true,
			Confidence: 0.75,
			Priority:   75,
			Reason:     "HPA scaling limitation detected",
		}
	}

	// Check for high request latency combined with status indicators.
	status := ev.Get("argocd_status")
	if status != "" && strings.Contains(status, `"health":"Degraded"`) &&
		containsAny(logs, "timeout", "503", "upstream connect error") {
		return skill.MatchResult{
			Matched:    true,
			Confidence: 0.65,
			Priority:   65,
			Reason:     "Degraded health with timeout/503 errors — possible capacity issue",
		}
	}

	return skill.MatchResult{}
}

func (s *ScaleUpSkill) Diagnose(_ context.Context, _ *ticket.Ticket, _ skill.EvidenceSet) (*skill.DiagnosisResult, error) {
	return &skill.DiagnosisResult{
		Diagnosis:      "Service is at maximum replica count and cannot handle current load. HPA maxReplicas should be increased.",
		Confidence:     ticket.ConfidenceMedium,
		Fixable:        true,
		FixType:        "scale_up",
		YAMLField:      "autoscaling.maxReplicas",
		SuggestedValue: "10",
		SkillName:      s.Name(),
	}, nil
}

func (s *ScaleUpSkill) Fix(_ context.Context, t *ticket.Ticket, _ *skill.DiagnosisResult) (*skill.FixResult, error) {
	return &skill.FixResult{
		Applied:    true,
		FilePath:   detectFilePath(t.Tenant, t.Service),
		Summary:    "Increase HPA maxReplicas from 5 to 10",
		NextSkills: []string{"quota_adjust"},
	}, nil
}
