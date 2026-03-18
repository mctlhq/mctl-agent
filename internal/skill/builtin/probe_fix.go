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

// ProbeFixSkill detects liveness/readiness probe failures and suggests timeout adjustments.
type ProbeFixSkill struct{}

func NewProbeFixSkill() *ProbeFixSkill { return &ProbeFixSkill{} }

func (s *ProbeFixSkill) Name() string    { return "probe_fix" }
func (s *ProbeFixSkill) Version() string { return "1.0.0" }

func (s *ProbeFixSkill) Description() string {
	return "Detects liveness/readiness probe failures and suggests timeout adjustments"
}

func (s *ProbeFixSkill) RequiredCapabilities() []skill.CapabilityID {
	return []skill.CapabilityID{skill.CapReadLogs, skill.CapReadConfig, skill.CapModifyGitOps, skill.CapCreatePR}
}

func (s *ProbeFixSkill) Match(_ context.Context, _ *ticket.Ticket, ev skill.EvidenceSet) skill.MatchResult {
	logs := ev.Get("logs") + "\n" + ev.Get("alert")
	if containsAny(logs, "Liveness probe failed", "Readiness probe failed", "probe failed") {
		return skill.MatchResult{
			Matched:    true,
			Confidence: 0.80,
			Priority:   80,
			Reason:     "Health probe failure detected in logs",
		}
	}
	return skill.MatchResult{}
}

func (s *ProbeFixSkill) Diagnose(_ context.Context, _ *ticket.Ticket, ev skill.EvidenceSet) (*skill.DiagnosisResult, error) {
	logs := ev.Get("logs") + "\n" + ev.Get("alert")

	probeType := "liveness/readiness"
	if strings.Contains(logs, "Liveness probe failed") {
		probeType = "liveness"
	} else if strings.Contains(logs, "Readiness probe failed") {
		probeType = "readiness"
	}

	return &skill.DiagnosisResult{
		Diagnosis:      "Container " + probeType + " probe is failing. The application may need more startup time or the probe configuration needs adjustment.",
		Confidence:     ticket.ConfidenceMedium,
		Fixable:        true,
		FixType:        "adjust_probe",
		YAMLField:      probeType + "Probe.initialDelaySeconds",
		SuggestedValue: "30",
		SkillName:      s.Name(),
	}, nil
}

func (s *ProbeFixSkill) AutoMergeSafe() bool { return true }

func (s *ProbeFixSkill) Fix(_ context.Context, t *ticket.Ticket, diag *skill.DiagnosisResult) (*skill.FixResult, error) {
	return &skill.FixResult{
		Applied:  true,
		FilePath: detectFilePath(t.Tenant, t.Service),
		Summary:  "Increase probe initialDelaySeconds to 30s",
	}, nil
}
