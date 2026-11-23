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

	"github.com/mctlhq/mctl-agent/internal/fixer"
	"github.com/mctlhq/mctl-agent/internal/skill"
	"github.com/mctlhq/mctl-agent/internal/ticket"
)

// ScaleUpSkill detects an HPA that reports it cannot scale further and suggests a max replica increase.
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

	// A Degraded app with timeouts or 503s in its logs is deliberately not a
	// match. A pod that crashes on startup produces exactly that picture: on
	// 2026-11-23 labs/mctl-telegram-preview crash-looped on "authorization
	// server metadata unusable: HTTP 503" with one replica and no HPA
	// pressure, and this skill told the operator to raise maxReplicas (as it
	// had on 2026-10-29, 2026-10-30 and 2026-11-13). The diagnosis below is
	// a fixed sentence about HPA limits, so only an HPA signal may select it.
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
		FilePath:   fixer.DetectFilePath(t.Tenant, t.Service),
		Summary:    "Increase HPA maxReplicas from 5 to 10",
		NextSkills: []string{"quota_adjust"},
	}, nil
}
