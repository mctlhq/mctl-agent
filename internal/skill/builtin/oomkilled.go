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

// OOMKilledSkill detects OOM-killed containers and bumps memory limits.
type OOMKilledSkill struct{}

func NewOOMKilledSkill() *OOMKilledSkill { return &OOMKilledSkill{} }

func (s *OOMKilledSkill) Name() string    { return "oomkilled" }
func (s *OOMKilledSkill) Version() string { return "1.0.0" }

func (s *OOMKilledSkill) Description() string {
	return "Detects OOM-killed containers and increases memory limits by 50%"
}

func (s *OOMKilledSkill) RequiredCapabilities() []skill.CapabilityID {
	return []skill.CapabilityID{skill.CapReadLogs, skill.CapReadConfig, skill.CapModifyGitOps, skill.CapCreatePR}
}

func (s *OOMKilledSkill) Match(_ context.Context, _ *ticket.Ticket, ev skill.EvidenceSet) skill.MatchResult {
	logs := ev.Get("logs") + "\n" + ev.Get("alert")
	if containsAny(logs, "OOMKilled", "oom-kill", "Out of memory") {
		return skill.MatchResult{
			Matched:    true,
			Confidence: 0.95,
			Priority:   100,
			Reason:     "OOMKilled signature found in logs/alert",
		}
	}
	return skill.MatchResult{}
}

func (s *OOMKilledSkill) Diagnose(_ context.Context, _ *ticket.Ticket, _ skill.EvidenceSet) (*skill.DiagnosisResult, error) {
	return &skill.DiagnosisResult{
		Diagnosis:  "Container killed due to OOM (Out of Memory). Memory limit needs to be increased.",
		Confidence: ticket.ConfidenceHigh,
		Fixable:    true,
		FixType:    "bump_memory",
		SkillName:  s.Name(),
	}, nil
}

func (s *OOMKilledSkill) Fix(_ context.Context, t *ticket.Ticket, _ *skill.DiagnosisResult) (*skill.FixResult, error) {
	// Fix is handled by the pipeline via fixer.GenerateMemoryBump.
	// The skill returns the fix type; the pipeline does the actual patching.
	return &skill.FixResult{
		Applied:    true,
		FilePath:   detectFilePath(t.Tenant, t.Service),
		Summary:    "Bump memory limit by 50% (OOMKilled)",
		NextSkills: []string{"quota_adjust"},
	}, nil
}

func (s *OOMKilledSkill) AutoMergeSafe() bool { return true }

// containsAny checks if s contains any of the substrings.
func containsAny(s string, substrs ...string) bool {
	for _, sub := range substrs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// detectFilePath determines the gitops values file for a service.
func detectFilePath(tenant, service string) string {
	platformServices := map[string]bool{
		"mctl-api":   true,
		"mctl-agent": true,
	}
	if platformServices[service] {
		return "platform-gitops/apps/templates/" + service + ".yaml"
	}
	return "platform-gitops/services/" + tenant + "/" + service + "/values.yaml"
}
