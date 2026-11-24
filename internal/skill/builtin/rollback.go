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
	"regexp"
	"sort"
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
	if rollout, ok := recentImageRollout(t.Tenant, t.Service, ev.Get(gitopsBumpsEvidence)); ok {
		reason := fmt.Sprintf("Pod crash-looping within 30 minutes of a GitOps image bump (%s, %s -> %s)",
			shortSHA(rollout.Last.SHA), rollout.From(), rollout.To())
		if rollout.Count > 1 {
			reason = fmt.Sprintf("Pod crash-looping within 30 minutes of %d GitOps image bumps (%s to %s, %s -> %s)",
				rollout.Count, shortSHA(rollout.First.SHA), shortSHA(rollout.Last.SHA), rollout.From(), rollout.To())
		}
		return skill.MatchResult{
			Matched:    true,
			Confidence: 0.90,
			Priority:   95,
			Reason:     reason,
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
	result := &skill.DiagnosisResult{
		Diagnosis:  "Pod crash-looping after recent deployment. Rollback to previous version recommended.",
		Confidence: ticket.ConfidenceHigh,
		Fixable:    true,
		FixType:    "rollback_image",
		SkillName:  s.Name(),
	}
	rollout, ok := recentImageRollout(t.Tenant, t.Service, ev.Get(gitopsBumpsEvidence))
	if !ok {
		return result, nil
	}
	const stamp = "2006-01-02 15:04 UTC"
	if rollout.Count == 1 {
		result.Diagnosis = fmt.Sprintf(
			"Pod crash-looping after a GitOps image bump of %s/%s from %s to %s (commit %s, %s). "+
				"Rollback to the previous image recommended.",
			t.Tenant, t.Service, rollout.From(), rollout.To(), shortSHA(rollout.Last.SHA),
			rollout.Last.CommittedAt.UTC().Format(stamp))
	} else {
		result.Diagnosis = fmt.Sprintf(
			"Pod crash-looping after %d GitOps image bumps of %s/%s inside 30 minutes, from %s to %s "+
				"(first: commit %s, %s; last: commit %s, %s). Which of them started the crash-loop is not known, "+
				"so rollback to %s, the image that ran before the first of them, is recommended.",
			rollout.Count, t.Tenant, t.Service, rollout.From(), rollout.To(),
			shortSHA(rollout.First.SHA), rollout.First.CommittedAt.UTC().Format(stamp),
			shortSHA(rollout.Last.SHA), rollout.Last.CommittedAt.UTC().Format(stamp),
			rollout.From())
	}
	// Both ends of the rollback, so the patch is made for exactly the change
	// this diagnosis describes and for no other.
	result.CurrentValue = rollout.To()
	result.SuggestedValue = rollout.From()
	return result, nil
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

// commitClockSkew is how far ahead of this process's clock a bump's commit
// time may be. Committer dates are set by whoever made the commit; one in
// the future would stay "recent", and the newest change, until that date.
const commitClockSkew = fixer.MaxCommitClockSkew

// imageRollout is the run of image bumps a crash-loop is blamed on: every
// tag change of the service's values file inside deployWindow, back to the
// last rollback. First is the oldest of them, Last the newest; they are the
// same bump when Count is 1.
type imageRollout struct {
	First, Last fixer.ImageBump
	Count       int
}

// From is the image that ran before the rollout, the rollback target.
func (r imageRollout) From() string { return r.First.FromTag }

// To is the image the rollout left the service on.
func (r imageRollout) To() string { return r.Last.ToTag }

// recentImageRollout returns the image bumps to blame a crash-loop on.
//
// The evidence is the JSON of a fixer.ImageBumpHistory, written only after a
// complete read of the file's history. Absent or unparseable evidence means
// the history was not observed, and that is never a match. A history counts
// only when its path is exactly this service's values file: a bump of
// "api-gateway" is not a bump of "api".
//
// The newest change decides whether there is anything to blame. It has to be
// inside deployWindow, not dated ahead of the clock, and a deploy: if it put
// an older image back (see returnsToOlderImage), the service already runs
// what it ran before, and a "rollback" from there would re-deploy the image
// that crashed.
//
// Then every earlier bump inside the window belongs to the same rollout, as
// far back as the last rollback. Nothing in the evidence says which of them
// the crash-loop began with: the alert fires only after its `for` period, so
// with bumps 15 minutes apart the newest one is routinely younger than the
// crash. Blaming the newest would roll back one step, onto the image that
// may have started it. The only image the window gives no reason to suspect
// is the one before the first bump, and that is From().
//
// A history that does not chain (one bump's FromTag is not the ToTag of the
// bump before it) or holds an incomplete bump was not understood, and is not
// a match either.
func recentImageRollout(tenant, service, historyJSON string) (imageRollout, bool) {
	if historyJSON == "" || tenant == "" || service == "" {
		return imageRollout{}, false
	}
	var history fixer.ImageBumpHistory
	if err := json.Unmarshal([]byte(historyJSON), &history); err != nil {
		return imageRollout{}, false
	}
	path := fixer.DetectFilePath(tenant, service)
	if path == "" || history.Path != path || len(history.Bumps) == 0 {
		return imageRollout{}, false
	}

	bumps := append([]fixer.ImageBump(nil), history.Bumps...)
	sort.SliceStable(bumps, func(i, j int) bool { return bumps[i].CommittedAt.After(bumps[j].CommittedAt) })

	now := time.Now().UTC()
	cutoff := now.Add(-deployWindow)
	if bumps[0].CommittedAt.After(now.Add(commitClockSkew)) {
		return imageRollout{}, false
	}

	rollout := imageRollout{}
	for i, b := range bumps {
		if !b.CommittedAt.After(cutoff) || returnsToOlderImage(bumps, i) {
			break
		}
		if b.SHA == "" || b.FromTag == "" || b.ToTag == "" {
			return imageRollout{}, false
		}
		if rollout.Count > 0 && b.ToTag != rollout.First.FromTag {
			return imageRollout{}, false
		}
		if rollout.Count == 0 {
			rollout.Last = b
		}
		rollout.First = b
		rollout.Count++
	}
	return rollout, rollout.Count > 0
}

// returnsToOlderImage reports whether bumps[i] (bumps is newest first) put
// an older image back instead of deploying a new one. Two signs, either is
// enough:
//   - its subject says so (isRollbackSubject), which is all there is to go
//     on when the image it returned to left the file before the history
//     starts;
//   - the tag it wrote is one an older bump in the history moved away from.
//     That catches a downgrade made by hand under an ordinary "bump" subject.
func returnsToOlderImage(bumps []fixer.ImageBump, i int) bool {
	if isRollbackSubject(bumps[i].Subject) {
		return true
	}
	if bumps[i].ToTag == "" {
		return false
	}
	for _, older := range bumps[i+1:] {
		if older.FromTag == bumps[i].ToTag {
			return true
		}
	}
	return false
}

// rollbackSubjectPattern matches a subject whose description, after an
// optional conventional-commit "type(scope):" prefix, begins with rollback,
// roll back or revert.
var rollbackSubjectPattern = regexp.MustCompile(`(?i)^\s*(?:[a-z]+(?:\([^)]*\))?!?:\s*)?(?:rollback|roll back|revert)`)

// isRollbackSubject reports whether a commit subject says the commit put an
// older version back: the agent's own "fix(<service>): Rollback image tag
// from X to Y", the platform's "chore: rollback ...", or a git revert.
//
// Only the start of the description counts. The bots write the service name
// into the subject ("chore: bump <team>/<service> to <tag>"), so looking for
// the words anywhere would make every bump of a service called
// "revert-proxy" a rollback, and the skill would never match for it.
func isRollbackSubject(subject string) bool {
	return rollbackSubjectPattern.MatchString(subject)
}

func shortSHA(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}
