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

package pipeline

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/mctlhq/mctl-agent/internal/fixer"
	"github.com/mctlhq/mctl-agent/internal/mctlclient"
	"github.com/mctlhq/mctl-agent/internal/skill"
	"github.com/mctlhq/mctl-agent/internal/ticket"
)

// proposingSkill returns a fixed diagnosis and fix, so one test table can
// drive every way a fixable diagnosis leaves the pipeline.
type proposingSkill struct {
	diag *skill.DiagnosisResult
	fix  *skill.FixResult
}

func (proposingSkill) Name() string        { return "proposing_test" }
func (proposingSkill) Version() string     { return "0" }
func (proposingSkill) Description() string { return "test" }
func (proposingSkill) Match(context.Context, *ticket.Ticket, skill.EvidenceSet) skill.MatchResult {
	return skill.MatchResult{Matched: true, Confidence: 1}
}
func (s proposingSkill) Diagnose(context.Context, *ticket.Ticket, skill.EvidenceSet) (*skill.DiagnosisResult, error) {
	return s.diag, nil
}
func (s proposingSkill) Fix(context.Context, *ticket.Ticket, *skill.DiagnosisResult) (*skill.FixResult, error) {
	return s.fix, nil
}
func (proposingSkill) RequiredCapabilities() []skill.CapabilityID { return nil }

// incidentStatuses makes the pipeline's mctl-api client record the status of
// every incident update (PATCH) it sends, in order.
func incidentStatuses(t *testing.T, p *Pipeline) func() []string {
	t.Helper()
	var mu sync.Mutex
	var statuses []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, "/api/v1/incidents/") {
			raw, _ := io.ReadAll(r.Body)
			var m map[string]any
			_ = json.Unmarshal(raw, &m)
			if st, ok := m["status"].(string); ok {
				mu.Lock()
				statuses = append(statuses, st)
				mu.Unlock()
			}
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)
	p.apiClient = mctlclient.NewClient(srv.URL, "test-token")
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), statuses...)
	}
}

// fix_proposed promises a pull request somebody can approve: the Telegram
// approve command refuses a ticket without one, and nothing else moves a
// fix_proposed ticket on. So a ticket must reach it only with a PR recorded,
// and every fixable diagnosis that ends without a PR must go to a human
// (escalated) with the proposal kept. Ticket 288d5629 (skill scale_up, MEDIUM)
// sat in fix_proposed with no PR until the stale TTL, for an app that had
// recovered within minutes.
//
// Both directions are covered: the PR-less paths must NOT be fix_proposed, and
// the path that really opens a PR still must be.
//
// A response that names only half a pull request is still a pull request:
// with the number it can be approved, so it is fix_proposed; with only the
// URL it cannot, so it is escalated — but the URL stays on the ticket, and
// neither is ever reported as a dry run in which nothing was changed.
//
// Mutation check: restore `t.Status = ticket.StatusFixProposed` +
// persistDiagnosisAndMirror in the MEDIUM branch and "medium suggestion"
// fails; drop the no-PR guard after CreatePR and "dry run" fails; escalate
// unconditionally after CreatePR and "pull request opened" fails; drop the
// ProposedFix assignment in the MEDIUM branch and "medium suggestion" fails
// on the proposal text; turn the no-PR guard's `&&` back into `||` and both
// "pull request without" rows fail; drop the `if p.dryRun` split and "no
// pull request outside dry run" fails; drop the `prNumber == 0` branch and
// "pull request without a number" fails.
func TestFixProposedOnlyWithAPullRequest(t *testing.T) {
	const valuesPath = "platform-gitops/services/billing/api/values.yaml"
	appliedFix := &skill.FixResult{Applied: true, FilePath: valuesPath, NewContent: "replicas: 2\n", Summary: "scale to 2 replicas"}

	cases := []struct {
		name         string
		s            proposingSkill
		github       func(t *testing.T, store *ticket.Store) *fixer.GitHubFixer
		wantStatus   string
		wantPR       int
		wantProposal []string
		wantAnalysis string
		dryRun       bool
		wantPRURL    string
		notAnalysis  []string
	}{
		{
			name: "medium suggestion",
			s: proposingSkill{diag: &skill.DiagnosisResult{
				Diagnosis: "one replica cannot absorb the load", Confidence: ticket.ConfidenceMedium, Fixable: true,
				FixType: "scale_up", YAMLField: "replicaCount", CurrentValue: "1", SuggestedValue: "2",
				Reasoning: "CPU saturated on the only replica.",
			}},
			wantStatus:   ticket.StatusEscalated,
			wantProposal: []string{"proposing_test", "scale_up", "replicaCount", "from 1", "to 2", "CPU saturated"},
			wantAnalysis: "no PR was created",
		},
		{
			name: "dry run",
			s: proposingSkill{
				diag: &skill.DiagnosisResult{Diagnosis: "needs more replicas", Confidence: ticket.ConfidenceHigh, Fixable: true},
				fix:  appliedFix,
			},
			github: func(t *testing.T, store *ticket.Store) *fixer.GitHubFixer {
				return stubGitHubFixerMode(t, store, true)
			},
			dryRun:       true,
			wantStatus:   ticket.StatusEscalated,
			wantProposal: []string{"scale to 2 replicas"},
			wantAnalysis: "no PR was opened (dry-run)",
		},
		{
			// CreatePR answers ("", 0, nil) while the pipeline is not in
			// dry-run: nothing may claim the GitOps repo is untouched.
			name: "no pull request outside dry run",
			s: proposingSkill{
				diag: &skill.DiagnosisResult{Diagnosis: "needs more replicas", Confidence: ticket.ConfidenceHigh, Fixable: true},
				fix:  appliedFix,
			},
			github: func(t *testing.T, store *ticket.Store) *fixer.GitHubFixer {
				return stubGitHubFixerMode(t, store, true)
			},
			wantStatus:   ticket.StatusEscalated,
			wantProposal: []string{"scale to 2 replicas"},
			wantAnalysis: "returned no pull request",
			notAnalysis:  []string{"dry-run", "Nothing was changed"},
		},
		{
			name: "pull request without a url",
			s: proposingSkill{
				diag: &skill.DiagnosisResult{Diagnosis: "needs more replicas", Confidence: ticket.ConfidenceHigh, Fixable: true},
				fix:  appliedFix,
			},
			github: func(t *testing.T, store *ticket.Store) *fixer.GitHubFixer {
				return stubGitHubFixerPR(t, store, false, map[string]any{"number": 42})
			},
			wantStatus:   ticket.StatusFixProposed,
			wantPR:       42,
			wantProposal: []string{"scale to 2 replicas"},
			notAnalysis:  []string{"dry-run", "Nothing was changed"},
		},
		{
			name: "pull request without a number",
			s: proposingSkill{
				diag: &skill.DiagnosisResult{Diagnosis: "needs more replicas", Confidence: ticket.ConfidenceHigh, Fixable: true},
				fix:  appliedFix,
			},
			github: func(t *testing.T, store *ticket.Store) *fixer.GitHubFixer {
				return stubGitHubFixerPR(t, store, false, map[string]any{"html_url": "https://github.com/owner/repo/pull/42"})
			},
			wantStatus:   ticket.StatusEscalated,
			wantPRURL:    "https://github.com/owner/repo/pull/42",
			wantProposal: []string{"scale to 2 replicas"},
			wantAnalysis: "needs more replicas\n\n[escalated] A PR was opened (https://github.com/owner/repo/pull/42)",
			notAnalysis:  []string{"dry-run", "Nothing was changed"},
		},
		{
			name: "pull request opened",
			s: proposingSkill{
				diag: &skill.DiagnosisResult{Diagnosis: "needs more replicas", Confidence: ticket.ConfidenceHigh, Fixable: true},
				fix:  appliedFix,
			},
			github:       stubGitHubFixer,
			wantStatus:   ticket.StatusFixProposed,
			wantPR:       42,
			wantPRURL:    "https://github.com/owner/repo/pull/42",
			wantProposal: []string{"scale to 2 replicas"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, store := newAsyncTestPipeline(t, 1)
			statuses := incidentStatuses(t, p)
			if tc.github != nil {
				p.github = tc.github(t, store)
			}
			p.dryRun = tc.dryRun
			p.registry.Register(tc.s)

			tk := &ticket.Ticket{
				Source: ticket.SourceAlertManager, Type: ticket.TypePodCrashloop,
				Tenant: "billing", Service: "api", Summary: "crashloop",
			}
			if err := store.Create(ctx, tk); err != nil {
				t.Fatal(err)
			}
			p.processTicketSync(ctx, tk)

			got, err := store.Get(ctx, tk.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.Status != tc.wantStatus {
				t.Errorf("status = %q, want %q (analysis=%q)", got.Status, tc.wantStatus, got.Analysis)
			}
			if got.PRNumber != tc.wantPR {
				t.Errorf("pr_number = %d, want %d", got.PRNumber, tc.wantPR)
			}
			if got.PRURL != tc.wantPRURL {
				t.Errorf("pr_url = %q, want %q", got.PRURL, tc.wantPRURL)
			}
			// The approve command merges by number, so that is what
			// fix_proposed must never be without.
			if got.Status == ticket.StatusFixProposed && got.PRNumber == 0 {
				t.Errorf("fix_proposed without a PR number (url=%q): nothing can approve it", got.PRURL)
			}
			for _, not := range tc.notAnalysis {
				if strings.Contains(got.Analysis, not) {
					t.Errorf("analysis = %q, must not contain %q", got.Analysis, not)
				}
			}
			for _, want := range tc.wantProposal {
				if !strings.Contains(got.ProposedFix, want) {
					t.Errorf("proposed_fix = %q, want it to keep %q", got.ProposedFix, want)
				}
			}
			if !strings.Contains(got.Analysis, tc.wantAnalysis) {
				t.Errorf("analysis = %q, want it to contain %q", got.Analysis, tc.wantAnalysis)
			}

			// mctl-api must end on the status the store holds, and must never
			// have been told fix_proposed for a ticket that has no PR.
			sent := statuses()
			if len(sent) == 0 || sent[len(sent)-1] != tc.wantStatus {
				t.Errorf("mirrored statuses = %v, want the last to be %q", sent, tc.wantStatus)
			}
			if tc.wantPR == 0 {
				for _, st := range sent {
					if st == ticket.StatusFixProposed {
						t.Errorf("mirrored statuses = %v: fix_proposed was sent for a ticket without a PR", sent)
					}
				}
			}
		})
	}
}

// The proposal line must say what is known and nothing more: a diagnosis
// that names no field still records who proposed and that nothing was applied.
func TestProposalSummary(t *testing.T) {
	cases := []struct {
		name    string
		d       skill.DiagnosisResult
		want    []string
		wantNot []string
	}{
		{
			name:    "bare diagnosis",
			d:       skill.DiagnosisResult{Confidence: ticket.ConfidenceMedium},
			want:    []string{"Proposed by skill s at MEDIUM confidence", "no PR opened"},
			wantNot: []string{"Change", "Fix type"},
		},
		{
			name: "full diagnosis",
			d: skill.DiagnosisResult{
				Confidence: ticket.ConfidenceMedium, FixType: "bump_memory", YAMLPath: "a/values.yaml",
				YAMLField: "resources.limits.memory", CurrentValue: "256Mi", SuggestedValue: "512Mi", Reasoning: "OOM at 250Mi.",
			},
			want: []string{"Fix type: bump_memory.", "Change resources.limits.memory from 256Mi to 512Mi in a/values.yaml.", "OOM at 250Mi."},
		},
		{
			name:    "suggested value without a field",
			d:       skill.DiagnosisResult{Confidence: ticket.ConfidenceMedium, SuggestedValue: "3"},
			want:    []string{"Change value to 3."},
			wantNot: []string{"from"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := proposalSummary("s", &tc.d)
			for _, w := range tc.want {
				if !strings.Contains(got, w) {
					t.Errorf("summary = %q, want it to contain %q", got, w)
				}
			}
			for _, w := range tc.wantNot {
				if strings.Contains(got, w) {
					t.Errorf("summary = %q, must not contain %q", got, w)
				}
			}
		})
	}
}
