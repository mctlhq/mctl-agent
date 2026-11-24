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
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mctlhq/mctl-agent/internal/fixer"
	"github.com/mctlhq/mctl-agent/internal/gitopspath"
	"github.com/mctlhq/mctl-agent/internal/mctlclient"
	"github.com/mctlhq/mctl-agent/internal/skill"
	"github.com/mctlhq/mctl-agent/internal/skill/builtin"
	"github.com/mctlhq/mctl-agent/internal/ticket"
)

const crashingPods = `{"items":[{"name":"billing-api-5c7-xk2","containers":[{"name":"app","ready":false,"restart_count":12,"state":{"kind":"waiting","reason":"CrashLoopBackOff"},"last_termination":{"kind":"terminated","reason":"Error","exit_code":1}}]}]}`

// workloadAPI stubs mctl-api: pods and events answer as given (an empty body
// means 503), every other route answers {}.
func workloadAPI(t *testing.T, p *Pipeline, pods, events string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := `{}`
		switch {
		case strings.HasPrefix(r.URL.Path, "/api/v1/pods/billing/api"):
			body = pods
		case strings.HasPrefix(r.URL.Path, "/api/v1/events/billing"):
			body = events
		case strings.HasPrefix(r.URL.Path, "/api/v1/logs/"):
			body = `{"lines":[{"line":"level=error msg=\"db: connection refused\""}]}`
		}
		if body == "" {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	p.apiClient = mctlclient.NewClient(srv.URL, "test-token")
}

func crashloopTicket(t *testing.T, store *ticket.Store) *ticket.Ticket {
	t.Helper()
	tk := &ticket.Ticket{Source: ticket.SourceAlertManager, Type: ticket.TypePodCrashloop, Tenant: "billing", Service: "api", Summary: "crashloop", Status: ticket.StatusAnalyzing}
	if err := store.Create(context.Background(), tk); err != nil {
		t.Fatal(err)
	}
	return tk
}

func evidenceTypes(t *testing.T, store *ticket.Store, id string) map[string]string {
	t.Helper()
	got, err := store.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, e := range got.Evidence {
		out[e.Type] = e.Content
	}
	return out
}

// A crash-looping service is diagnosed from its pod state: the ticket ends
// escalated with the container, the exit code and the error line, not with
// "no skill matched".
//
// Mutation check: remove the pods block from collectEvidence and the skill no
// longer matches, so the analysis stays empty and this fails.
func TestCrashLoopIsDiagnosedFromPodState(t *testing.T) {
	p, store := newAsyncTestPipeline(t, 1)
	workloadAPI(t, p, crashingPods, `{"items":[{"reason":"BackOff","count":30}]}`)
	p.registry.Register(builtin.NewCrashLoopSkill())
	tk := crashloopTicket(t, store)

	p.processTicketSync(context.Background(), tk)

	ev := evidenceTypes(t, store, tk.ID)
	if ev["pods"] != crashingPods {
		t.Errorf("pods evidence = %q", ev["pods"])
	}
	if !strings.Contains(ev["events"], "BackOff") {
		t.Errorf("events evidence = %q", ev["events"])
	}
	got, err := store.Get(context.Background(), tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != ticket.StatusEscalated {
		t.Errorf("status = %q, want %q", got.Status, ticket.StatusEscalated)
	}
	for _, want := range []string{"Container app of pod billing-api-5c7-xk2", "12 restart(s), last exit code 1", "db: connection refused", "BackOff ×30"} {
		if !strings.Contains(got.Analysis, want) {
			t.Errorf("analysis lacks %q:\n%s", want, got.Analysis)
		}
	}
}

// When mctl-api cannot list pods or events, nothing is stored for them: no
// empty listing a skill could read as "no restarts".
//
// Mutation check: store the evidence regardless of the error and this fails.
func TestUnreadablePodStateLeavesNoEvidence(t *testing.T) {
	p, store := newAsyncTestPipeline(t, 1)
	workloadAPI(t, p, "", "")
	p.registry.Register(builtin.NewCrashLoopSkill())
	tk := crashloopTicket(t, store)

	p.processTicketSync(context.Background(), tk)

	ev := evidenceTypes(t, store, tk.ID)
	for _, typ := range []string{"pods", "events"} {
		if _, ok := ev[typ]; ok {
			t.Errorf("%s evidence stored from a failed read: %q", typ, ev[typ])
		}
	}
	if _, ok := ev["logs"]; !ok {
		t.Error("logs evidence missing; the stub no longer reaches collectEvidence")
	}
	got, err := store.Get(context.Background(), tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got.Analysis, "no skill matched") {
		t.Errorf("analysis = %q; an unreadable pod state must not be diagnosed", got.Analysis)
	}
}

const (
	billingAPIValues = "platform-gitops/services/billing/api/values.yaml"
	badDigest        = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	goodDigest       = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
)

func billingValues(tag, digest string) string {
	return "image:\n  repository: ghcr.io/acme/billing-api\n  tag: \"" + tag + "\"\n  digest: \"" + digest + "\"\nservice:\n  port: 8080\n"
}

// gitopsRepo stubs the GitHub API for the billing/api values file: a bump
// commit `bump` (parent `base`) made bumpAge ago moved the image from
// 1.4.0 to 1.5.0. With historyStatus set the commit listing answers that
// status instead. written holds the file content of every PUT.
//
// A test that needs another history sets commits (the listing, newest first)
// and files (the values file by ref, "main" included; a ref that is not
// there answers 404) instead. historyReads counts the listings served.
type gitopsRepo struct {
	bumpAge       time.Duration
	historyStatus int
	commits       []map[string]any
	files         map[string]string
	historyReads  atomic.Int32

	mu      sync.Mutex
	written []string
}

func (g *gitopsRepo) attach(t *testing.T, p *Pipeline, store *ticket.Store) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/commits"):
			if got := r.URL.Query().Get("path"); got != billingAPIValues {
				t.Errorf("history asked for %q", got)
			}
			g.historyReads.Add(1)
			if g.historyStatus != 0 {
				http.Error(w, `{"message":"unavailable"}`, g.historyStatus)
				return
			}
			if g.commits != nil {
				_ = json.NewEncoder(w).Encode(g.commits)
				return
			}
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{
					"sha": "bump",
					"commit": map[string]any{
						"message":   "chore: bump billing/api to 1.5.0",
						"committer": map[string]any{"date": time.Now().UTC().Add(-g.bumpAge).Format(time.RFC3339)},
					},
					"parents": []map[string]any{{"sha": "base"}},
				},
				{
					"sha": "base",
					"commit": map[string]any{
						"message":   "chore: bump billing/api to 1.4.0",
						"committer": map[string]any{"date": time.Now().UTC().Add(-g.bumpAge - 72*time.Hour).Format(time.RFC3339)},
					},
					"parents": []map[string]any{{"sha": "older"}},
				},
			})
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/contents/"):
			ref := r.URL.Query().Get("ref")
			body := billingValues("1.5.0", badDigest)
			if ref == "base" || ref == "older" {
				body = billingValues("1.4.0", goodDigest)
			}
			if g.files != nil {
				var ok bool
				if body, ok = g.files[ref]; !ok {
					http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
					return
				}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"type": "file", "encoding": "base64", "sha": "blob",
				"content": base64.StdEncoding.EncodeToString([]byte(body)),
			})
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/git/ref/"):
			_ = json.NewEncoder(w).Encode(map[string]any{"ref": "refs/heads/main", "object": map[string]any{"sha": "m1"}})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/git/refs"):
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"ref": "refs/heads/x"})
		case r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/contents/"):
			raw, _ := io.ReadAll(r.Body)
			var put struct {
				Content []byte `json:"content"`
			}
			if err := json.Unmarshal(raw, &put); err != nil {
				t.Errorf("PUT body: %v", err)
			}
			g.mu.Lock()
			g.written = append(g.written, string(put.Content))
			g.mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/pulls"):
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"number": 7, "html_url": "https://github.com/owner/repo/pull/7"})
		default:
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	gh := fixer.NewGitHubFixer("", "", "owner", "repo", store, false, 10, 10, gitopspath.DefaultAllowlist())
	if err := gh.SetBaseURL(srv.URL + "/"); err != nil {
		t.Fatal(err)
	}
	p.github = gh
}

func (g *gitopsRepo) writes() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.written...)
}

// A service that starts crash-looping right after a GitOps image bump gets a
// rollback PR, although mctl-api's audit holds no deploy-service row for it.
// The PR restores the tag and the digest of the revision before the bump.
//
// Mutation checks: drop the collectGitOpsBumps call from collectEvidence and
// the rollback skill no longer matches, so no PR is opened; have rollbackImage
// pass only the tag and the PR is refused for want of a digest.
func TestCrashLoopAfterGitOpsBumpGetsRollbackPR(t *testing.T) {
	p, store := newAsyncTestPipeline(t, 1)
	workloadAPI(t, p, crashingPods, `{"items":[{"reason":"BackOff","count":30}]}`)
	repo := &gitopsRepo{bumpAge: 5 * time.Minute}
	repo.attach(t, p, store)
	p.registry.Register(builtin.NewPostDeployRollbackSkill())
	p.registry.Register(builtin.NewCrashLoopSkill())
	tk := crashloopTicket(t, store)

	p.processTicketSync(context.Background(), tk)

	ev := evidenceTypes(t, store, tk.ID)
	var history fixer.ImageBumpHistory
	if err := json.Unmarshal([]byte(ev["gitops_bumps"]), &history); err != nil {
		t.Fatalf("gitops_bumps evidence %q: %v", ev["gitops_bumps"], err)
	}
	if history.Path != billingAPIValues || len(history.Bumps) != 1 ||
		history.Bumps[0].FromTag != "1.4.0" || history.Bumps[0].ToTag != "1.5.0" {
		t.Errorf("gitops_bumps evidence = %+v", history)
	}

	got, err := store.Get(context.Background(), tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != ticket.StatusFixProposed || got.PRNumber != 7 {
		t.Fatalf("status = %q, pr = %d, analysis = %q", got.Status, got.PRNumber, got.Analysis)
	}
	if !strings.Contains(got.Analysis, "GitOps image bump of billing/api from 1.4.0 to 1.5.0 (commit bump") {
		t.Errorf("analysis does not name the bump: %q", got.Analysis)
	}
	writes := repo.writes()
	if len(writes) != 1 {
		t.Fatalf("file writes = %d, want 1", len(writes))
	}
	if want := billingValues("1.4.0", goodDigest); writes[0] != want {
		t.Errorf("rollback wrote\n%s\nwant\n%s", writes[0], want)
	}
}

// Without an observed recent bump the rollback skill stays out of it and the
// crash-loop is only diagnosed: an old bump is not to blame, and a history
// that could not be read leaves no evidence at all, least of all an empty
// list.
//
// Mutation check: store the evidence although RecentImageBumps failed (or
// have it return an empty history on a failed listing) and the unreadable
// case finds gitops_bumps evidence.
func TestCrashLoopWithoutObservedRecentBumpIsNotRolledBack(t *testing.T) {
	tests := []struct {
		name         string
		repo         *gitopsRepo
		wantEvidence bool
	}{
		{"bump older than the window", &gitopsRepo{bumpAge: 45 * time.Minute}, true},
		{"history unreadable", &gitopsRepo{bumpAge: 5 * time.Minute, historyStatus: http.StatusServiceUnavailable}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, store := newAsyncTestPipeline(t, 1)
			workloadAPI(t, p, crashingPods, `{"items":[{"reason":"BackOff","count":30}]}`)
			tt.repo.attach(t, p, store)
			p.registry.Register(builtin.NewPostDeployRollbackSkill())
			p.registry.Register(builtin.NewCrashLoopSkill())
			tk := crashloopTicket(t, store)

			p.processTicketSync(context.Background(), tk)

			ev := evidenceTypes(t, store, tk.ID)
			if _, ok := ev["gitops_bumps"]; ok != tt.wantEvidence {
				t.Errorf("gitops_bumps evidence present = %v, want %v: %q", ok, tt.wantEvidence, ev["gitops_bumps"])
			}
			got, err := store.Get(context.Background(), tk.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.Status != ticket.StatusEscalated || got.PRNumber != 0 {
				t.Errorf("status = %q, pr = %d", got.Status, got.PRNumber)
			}
			if !strings.Contains(got.Analysis, "Container app of pod billing-api-5c7-xk2") {
				t.Errorf("crash-loop was not diagnosed from pod state: %q", got.Analysis)
			}
			if strings.Contains(got.Analysis, "image bump") {
				t.Errorf("analysis blames a bump: %q", got.Analysis)
			}
			if w := tt.repo.writes(); len(w) != 0 {
				t.Errorf("gitops file written %d time(s)", len(w))
			}
		})
	}
}

// repoCommit is one row of a gitopsRepo commit listing.
func repoCommit(sha, parent, subject string, age time.Duration) map[string]any {
	return map[string]any{
		"sha": sha,
		"commit": map[string]any{
			"message":   subject,
			"committer": map[string]any{"date": time.Now().UTC().Add(-age).Format(time.RFC3339)},
		},
		"parents": []map[string]any{{"sha": parent}},
	}
}

const midDigest = "sha256:3333333333333333333333333333333333333333333333333333333333333333"

// Two bumps land inside the window, 1.4.0 -> 1.5.0 and then 1.5.0 -> 1.6.0,
// as they do on a preview service that is bumped on every push. The alert
// fires only after its `for` period, so the newer bump may well be younger
// than the crash-loop. The PR goes back to 1.4.0, the image before both, not
// one step back to 1.5.0, which may be the image that started it.
//
// Mutation checks: have recentImageRollout stop at the newest bump, or have
// Diagnose leave CurrentValue and SuggestedValue empty, and the PR restores
// 1.5.0.
func TestCrashLoopAfterTwoGitOpsBumpsRollsBackPastBoth(t *testing.T) {
	p, store := newAsyncTestPipeline(t, 1)
	workloadAPI(t, p, crashingPods, `{"items":[{"reason":"BackOff","count":30}]}`)
	repo := &gitopsRepo{
		commits: []map[string]any{
			repoCommit("c2", "c1", "chore: bump billing/api to 1.6.0", 3*time.Minute),
			repoCommit("c1", "c0", "chore: bump billing/api to 1.5.0", 19*time.Minute),
			repoCommit("c0", "older", "chore: bump billing/api to 1.4.0", 72*time.Hour),
		},
		files: map[string]string{
			"main": billingValues("1.6.0", badDigest),
			"c2":   billingValues("1.6.0", badDigest),
			"c1":   billingValues("1.5.0", midDigest),
			"c0":   billingValues("1.4.0", goodDigest),
		},
	}
	repo.attach(t, p, store)
	p.registry.Register(builtin.NewPostDeployRollbackSkill())
	p.registry.Register(builtin.NewCrashLoopSkill())
	tk := crashloopTicket(t, store)

	p.processTicketSync(context.Background(), tk)

	got, err := store.Get(context.Background(), tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != ticket.StatusFixProposed || got.PRNumber != 7 {
		t.Fatalf("status = %q, pr = %d, analysis = %q", got.Status, got.PRNumber, got.Analysis)
	}
	if !strings.Contains(got.Analysis, "2 GitOps image bumps of billing/api") ||
		!strings.Contains(got.Analysis, "from 1.4.0 to 1.6.0") {
		t.Errorf("analysis does not name the rollout: %q", got.Analysis)
	}
	writes := repo.writes()
	if len(writes) != 1 {
		t.Fatalf("file writes = %d, want 1", len(writes))
	}
	if want := billingValues("1.4.0", goodDigest); writes[0] != want {
		t.Errorf("rollback wrote\n%s\nwant\n%s", writes[0], want)
	}
}

// The evidence saw 1.4.0 -> 1.5.0. By the time the fix reads main, somebody
// has moved the file on to 1.6.0. The diagnosis is about a file that is gone:
// no patch is made, least of all one that "rolls back" the newer image under
// a ticket that blames 1.5.0.
//
// Mutation check: drop the comparison of main's tag with diag.CurrentValue
// in rollbackImage and a PR replacing 1.6.0 is opened.
func TestRollbackIsRefusedWhenTheFileMovedAfterTheEvidence(t *testing.T) {
	p, store := newAsyncTestPipeline(t, 1)
	workloadAPI(t, p, crashingPods, `{"items":[{"reason":"BackOff","count":30}]}`)
	repo := &gitopsRepo{
		commits: []map[string]any{
			repoCommit("bump", "base", "chore: bump billing/api to 1.5.0", 5*time.Minute),
			repoCommit("base", "older", "chore: bump billing/api to 1.4.0", 72*time.Hour),
		},
		files: map[string]string{
			"main": billingValues("1.6.0", midDigest), // landed after the listing
			"bump": billingValues("1.5.0", badDigest),
			"base": billingValues("1.4.0", goodDigest),
		},
	}
	repo.attach(t, p, store)
	p.registry.Register(builtin.NewPostDeployRollbackSkill())
	p.registry.Register(builtin.NewCrashLoopSkill())
	tk := crashloopTicket(t, store)

	p.processTicketSync(context.Background(), tk)

	got, err := store.Get(context.Background(), tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if w := repo.writes(); len(w) != 0 {
		t.Fatalf("gitops file written although it had moved on:\n%s", w[0])
	}
	if got.Status != ticket.StatusEscalated || got.PRNumber != 0 {
		t.Errorf("status = %q, pr = %d", got.Status, got.PRNumber)
	}
	for _, want := range []string{`now carries image tag "1.6.0"`, `made for "1.5.0"`} {
		if !strings.Contains(got.Analysis, want) {
			t.Errorf("analysis lacks %q: %s", want, got.Analysis)
		}
	}
}

// The GitOps history is read for crash-loop tickets only: no other ticket
// type has a skill that blames a deploy, and each read is GitHub API quota.
//
// Mutation check: remove the type condition in collectEvidence and the
// resource-limit ticket reads the history and stores it.
func TestGitOpsBumpsAreCollectedForCrashLoopsOnly(t *testing.T) {
	tests := []struct {
		typ       string
		wantReads int32
	}{
		{ticket.TypePodCrashloop, 1},
		{ticket.TypeResourceLimit, 0},
	}
	for _, tt := range tests {
		t.Run(tt.typ, func(t *testing.T) {
			p, store := newAsyncTestPipeline(t, 1)
			workloadAPI(t, p, crashingPods, `{"items":[]}`)
			repo := &gitopsRepo{bumpAge: 5 * time.Minute}
			repo.attach(t, p, store)
			tk := &ticket.Ticket{Source: ticket.SourceAlertManager, Type: tt.typ, Tenant: "billing", Service: "api", Summary: "x", Status: ticket.StatusAnalyzing}
			if err := store.Create(context.Background(), tk); err != nil {
				t.Fatal(err)
			}

			p.collectEvidence(context.Background(), tk)

			if got := repo.historyReads.Load(); got != tt.wantReads {
				t.Errorf("history listings = %d, want %d", got, tt.wantReads)
			}
			_, stored := evidenceTypes(t, store, tk.ID)["gitops_bumps"]
			if stored != (tt.wantReads > 0) {
				t.Errorf("gitops_bumps evidence stored = %v", stored)
			}
		})
	}
}

// rollbackImage holds the patch to the two ends the diagnosis names, and
// falls back to "previous distinct tag" only when it names neither.
//
// Mutation checks: drop the refusal of a half-named rollback, the comparison
// of main's tag with CurrentValue, or the "tag not found" error, and the
// case of that name fails.
func TestRollbackImageIsBoundToTheDiagnosis(t *testing.T) {
	tests := []struct {
		name    string
		diag    *skill.DiagnosisResult
		want    string // the patched file; "" when wantErr is set
		wantErr string
	}{
		{"no image named: previous distinct tag", &skill.DiagnosisResult{FixType: "rollback_image"}, billingValues("1.4.0", goodDigest), ""},
		{"both ends named", &skill.DiagnosisResult{CurrentValue: "1.5.0", SuggestedValue: "1.4.0"}, billingValues("1.4.0", goodDigest), ""},
		{"only the target named", &skill.DiagnosisResult{SuggestedValue: "1.4.0"}, "", "incomplete rollback"},
		{"only the current image named", &skill.DiagnosisResult{CurrentValue: "1.5.0"}, "", "incomplete rollback"},
		{"a rollback onto the same image", &skill.DiagnosisResult{CurrentValue: "1.5.0", SuggestedValue: "1.5.0"}, "", "incomplete rollback"},
		{"main carries another image", &skill.DiagnosisResult{CurrentValue: "1.4.9", SuggestedValue: "1.4.0"}, "", "changed after the evidence was read"},
		{"target not in the history", &skill.DiagnosisResult{CurrentValue: "1.5.0", SuggestedValue: "1.3.0"}, "", "image tag 1.3.0 not found"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, store := newAsyncTestPipeline(t, 1)
			repo := &gitopsRepo{bumpAge: 5 * time.Minute}
			repo.attach(t, p, store)

			got, _, err := p.rollbackImage(context.Background(), billingAPIValues, billingValues("1.5.0", badDigest), tt.diag)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want it to contain %q (patch %q)", err, tt.wantErr, got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Errorf("patched file\n%s\nwant\n%s", got, tt.want)
			}
		})
	}
}
