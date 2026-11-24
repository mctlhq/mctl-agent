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
	"fmt"
	"strings"
	"time"

	"github.com/mctlhq/mctl-agent/internal/fixer"
	"github.com/mctlhq/mctl-agent/internal/skill"
	"github.com/mctlhq/mctl-agent/internal/ticket"
)

// PostDeployRollbackSkill detects crash-loops after a recent deployment and
// suggests rollback. A deployment is either a deploy-service operation in
// the mctl-api audit or an image bump committed to the service's GitOps
// values file, which is how preview and release rollouts are made.
type PostDeployRollbackSkill struct{}

func NewPostDeployRollbackSkill() *PostDeployRollbackSkill { return &PostDeployRollbackSkill{} }

func (s *PostDeployRollbackSkill) Name() string    { return "post_deploy_rollback" }
func (s *PostDeployRollbackSkill) Version() string { return "1.1.0" }

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

	// Both sources carry the same weight: each one names exactly this
	// service and a deploy inside the window. Either outranks the crashloop
	// skill (0.78), which only reports how the container died.
	if bump, ok := recentImageBump(t.Tenant, t.Service, ev.Get(gitopsBumpsEvidence)); ok {
		return skill.MatchResult{
			Matched:    true,
			Confidence: 0.90,
			Priority:   95,
			Reason: fmt.Sprintf("Pod crash-looping within 30 minutes of a GitOps image bump (%s, %s -> %s)",
				shortSHA(bump.SHA), bump.FromTag, bump.ToTag),
		}
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

func (s *PostDeployRollbackSkill) Diagnose(_ context.Context, t *ticket.Ticket, ev skill.EvidenceSet) (*skill.DiagnosisResult, error) {
	diagnosis := "Pod crash-looping after recent deployment. Rollback to previous version recommended."
	if bump, ok := recentImageBump(t.Tenant, t.Service, ev.Get(gitopsBumpsEvidence)); ok {
		diagnosis = fmt.Sprintf(
			"Pod crash-looping after a GitOps image bump of %s/%s from %s to %s (commit %s, %s). "+
				"Rollback to the previous image recommended.",
			t.Tenant, t.Service, bump.FromTag, bump.ToTag, shortSHA(bump.SHA),
			bump.CommittedAt.UTC().Format("2006-01-02 15:04 UTC"))
	}
	return &skill.DiagnosisResult{
		Diagnosis:  diagnosis,
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

// gitopsBumpsEvidence is the evidence type the pipeline stores a
// fixer.ImageBumpHistory under.
const gitopsBumpsEvidence = "gitops_bumps"

// recentImageBump returns the image bump to blame a crash-loop on: the
// newest change of the service's own values file, when it is inside
// deployWindow and is not itself a rollback.
//
// The evidence is the JSON of a fixer.ImageBumpHistory, written only after a
// complete read of the file's history. Absent or unparseable evidence means
// the history was not observed, and that is never a match. A history counts
// only when its path is exactly this service's values file: a bump of
// "api-gateway" is not a bump of "api".
//
// Only the newest change is considered. If that one put an older image back
// (the agent's own rollback PR, or a revert), the service is already on the
// image it ran before, and the bump underneath it is not the running one:
// proposing a "rollback" from there would re-deploy the image that crashed.
func recentImageBump(tenant, service, historyJSON string) (fixer.ImageBump, bool) {
	if historyJSON == "" || tenant == "" || service == "" {
		return fixer.ImageBump{}, false
	}
	var history fixer.ImageBumpHistory
	if err := json.Unmarshal([]byte(historyJSON), &history); err != nil {
		return fixer.ImageBump{}, false
	}
	path := fixer.DetectFilePath(tenant, service)
	if path == "" || history.Path != path {
		return fixer.ImageBump{}, false
	}

	var newest fixer.ImageBump
	for _, b := range history.Bumps {
		if b.CommittedAt.After(newest.CommittedAt) {
			newest = b
		}
	}
	if newest.SHA == "" || newest.FromTag == "" || newest.ToTag == "" {
		return fixer.ImageBump{}, false
	}
	if !newest.CommittedAt.After(time.Now().UTC().Add(-deployWindow)) {
		return fixer.ImageBump{}, false
	}
	if isRollbackSubject(newest.Subject) {
		return fixer.ImageBump{}, false
	}
	return newest, true
}

// isRollbackSubject reports whether a commit subject says the commit put an
// older version back: the agent's own "Rollback image tag from X to Y", or a
// revert.
func isRollbackSubject(subject string) bool {
	s := strings.ToLower(subject)
	return strings.Contains(s, "rollback") || strings.Contains(s, "roll back") || strings.Contains(s, "revert")
}

func shortSHA(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}
