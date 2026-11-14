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
	"time"

	"github.com/mctlhq/mctl-agent/internal/fixer"
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
		FilePath: fixer.DetectFilePath(t.Tenant, t.Service),
		Summary:  "Rollback to previous image tag (post-deploy crash)",
	}, nil
}

// deployOperation is mctl-api's operation name for a service release. Its
// audit row carries the service as parameters team_name and component_name.
const deployOperation = "deploy-service"

// deployWindow is how recent a deploy must be to blame a crash-loop on it.
const deployWindow = 30 * time.Minute

// hasRecentDeploy reports whether the audit evidence holds a deploy of
// exactly this service within deployWindow.
//
// The evidence is the JSON of []mctlclient.AuditEntry, so the field names are
// mctl-api's: operation and parameters. A row counts only when
//   - its operation is deploy-service (a scale, a config read or a rollback
//     is not a deploy),
//   - team_name and component_name equal the ticket's tenant and service
//     exactly (a deploy of "api-gateway" is not a deploy of "api"), and
//   - its status shows the workflow was submitted: a denied or failed
//     request never reached the cluster.
func hasRecentDeploy(tenant, service, auditJSON string) bool {
	if auditJSON == "" || tenant == "" || service == "" {
		return false
	}

	type entry struct {
		Operation  string            `json:"operation"`
		Parameters map[string]string `json:"parameters"`
		Status     string            `json:"status"`
		Timestamp  time.Time         `json:"timestamp"`
	}
	var entries []entry
	if err := json.Unmarshal([]byte(auditJSON), &entries); err != nil {
		// The raw API response wraps the list in {"items": [...]}.
		var wrapped struct {
			Items []entry `json:"items"`
		}
		if err := json.Unmarshal([]byte(auditJSON), &wrapped); err != nil {
			return false
		}
		entries = wrapped.Items
	}

	cutoff := time.Now().UTC().Add(-deployWindow)
	for _, e := range entries {
		if e.Operation != deployOperation || !e.Timestamp.After(cutoff) {
			continue
		}
		switch e.Status {
		case "submitted", "succeeded":
		default:
			continue
		}
		if e.Parameters["team_name"] == tenant && e.Parameters["component_name"] == service {
			return true
		}
	}
	return false
}
