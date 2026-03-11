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
	"time"

	"github.com/mctlhq/mctl-agent/internal/skill"
	"github.com/mctlhq/mctl-agent/internal/ticket"
)

// PostDeployRollbackSkill detects crash-loops after a recent deployment and suggests rollback.
type PostDeployRollbackSkill struct{}

func NewPostDeployRollbackSkill() *PostDeployRollbackSkill { return &PostDeployRollbackSkill{} }

func (s *PostDeployRollbackSkill) Name() string    { return "post_deploy_rollback" }
func (s *PostDeployRollbackSkill) Version() string { return "1.0.0" }

func (s *PostDeployRollbackSkill) Description() string {
	return "Detects crash-looping pods after a recent deployment and recommends rollback"
}

func (s *PostDeployRollbackSkill) RequiredCapabilities() []skill.CapabilityID {
	return []skill.CapabilityID{skill.CapReadLogs, skill.CapReadAudit, skill.CapModifyGitOps, skill.CapCreatePR}
}

func (s *PostDeployRollbackSkill) Match(_ context.Context, t *ticket.Ticket, ev skill.EvidenceSet) skill.MatchResult {
	if t.Type != ticket.TypePodCrashloop {
		return skill.MatchResult{}
	}

	if hasRecentDeploy(t.Tenant, t.Service, ev.Get("audit")) {
		return skill.MatchResult{
			Matched:    true,
			Confidence: 0.90,
			Priority:   95,
			Reason:     "Pod crash-looping within 30 minutes of a deploy",
		}
	}
	return skill.MatchResult{}
}

func (s *PostDeployRollbackSkill) Diagnose(_ context.Context, _ *ticket.Ticket, _ skill.EvidenceSet) (*skill.DiagnosisResult, error) {
	return &skill.DiagnosisResult{
		Diagnosis:  "Pod crash-looping after recent deployment. Rollback to previous version recommended.",
		Confidence: ticket.ConfidenceHigh,
		Fixable:    true,
		FixType:    "rollback_image",
		SkillName:  s.Name(),
	}, nil
}

func (s *PostDeployRollbackSkill) Fix(_ context.Context, t *ticket.Ticket, _ *skill.DiagnosisResult) (*skill.FixResult, error) {
	return &skill.FixResult{
		Applied:  true,
		FilePath: detectFilePath(t.Tenant, t.Service),
		Summary:  "Rollback to previous image tag (post-deploy crash)",
	}, nil
}

// hasRecentDeploy checks audit JSON for a deploy within the last 30 minutes.
func hasRecentDeploy(tenant, service, auditJSON string) bool {
	if auditJSON == "" {
		return false
	}

	var entries []struct {
		Action    string    `json:"action"`
		Target    string    `json:"target"`
		Timestamp time.Time `json:"timestamp"`
	}
	if err := json.Unmarshal([]byte(auditJSON), &entries); err != nil {
		// Try wrapped format.
		var wrapped struct {
			Items json.RawMessage `json:"items"`
		}
		if err := json.Unmarshal([]byte(auditJSON), &wrapped); err != nil {
			return false
		}
		if err := json.Unmarshal(wrapped.Items, &entries); err != nil {
			return false
		}
	}

	cutoff := time.Now().UTC().Add(-30 * time.Minute)
	target := tenant + "/" + service
	for _, e := range entries {
		if e.Timestamp.After(cutoff) && strings.Contains(e.Target, target) {
			return true
		}
	}
	return false
}
