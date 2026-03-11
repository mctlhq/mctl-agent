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

// CPUThrottleSkill detects CPU throttling and suggests CPU limit increases.
type CPUThrottleSkill struct{}

func NewCPUThrottleSkill() *CPUThrottleSkill { return &CPUThrottleSkill{} }

func (s *CPUThrottleSkill) Name() string    { return "cpu_throttle" }
func (s *CPUThrottleSkill) Version() string { return "1.0.0" }

func (s *CPUThrottleSkill) Description() string {
	return "Detects CPU throttling events and suggests CPU limit increases"
}

func (s *CPUThrottleSkill) RequiredCapabilities() []skill.CapabilityID {
	return []skill.CapabilityID{skill.CapReadLogs, skill.CapReadResources, skill.CapModifyGitOps, skill.CapCreatePR}
}

func (s *CPUThrottleSkill) Match(_ context.Context, t *ticket.Ticket, ev skill.EvidenceSet) skill.MatchResult {
	// Match on resource limit tickets with CPU indicators.
	if t.Type == ticket.TypeResourceLimit {
		summary := strings.ToLower(t.Summary)
		if strings.Contains(summary, "cpu") {
			return skill.MatchResult{
				Matched:    true,
				Confidence: 0.85,
				Priority:   85,
				Reason:     "CPU quota/throttling alert",
			}
		}
	}

	logs := ev.Get("logs") + "\n" + ev.Get("alert")
	if containsAny(logs, "cpu throttl", "CPUThrottlingHigh", "TenantCPUQuotaHigh") {
		return skill.MatchResult{
			Matched:    true,
			Confidence: 0.80,
			Priority:   80,
			Reason:     "CPU throttling indicators in logs/alert",
		}
	}

	return skill.MatchResult{}
}

func (s *CPUThrottleSkill) Diagnose(_ context.Context, _ *ticket.Ticket, _ skill.EvidenceSet) (*skill.DiagnosisResult, error) {
	return &skill.DiagnosisResult{
		Diagnosis:  "Service is experiencing CPU throttling. CPU limit needs to be increased to prevent performance degradation.",
		Confidence: ticket.ConfidenceMedium,
		Fixable:    true,
		FixType:    "bump_cpu",
		YAMLField:  "resources.limits.cpu",
		SkillName:  s.Name(),
	}, nil
}

func (s *CPUThrottleSkill) Fix(_ context.Context, t *ticket.Ticket, _ *skill.DiagnosisResult) (*skill.FixResult, error) {
	return &skill.FixResult{
		Applied:  true,
		FilePath: detectFilePath(t.Tenant, t.Service),
		Summary:  "Increase CPU limit to reduce throttling",
	}, nil
}
