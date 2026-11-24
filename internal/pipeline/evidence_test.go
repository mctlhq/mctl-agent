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
	"testing"
	"time"

	"github.com/mctlhq/mctl-agent/internal/fixer"
	"github.com/mctlhq/mctl-agent/internal/gitopspath"
	"github.com/mctlhq/mctl-agent/internal/mctlclient"
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
type gitopsRepo struct {
	bumpAge       time.Duration
	historyStatus int

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
			if g.historyStatus != 0 {
				http.Error(w, `{"message":"unavailable"}`, g.historyStatus)
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
			body := billingValues("1.5.0", badDigest)
			if ref := r.URL.Query().Get("ref"); ref == "base" || ref == "older" {
				body = billingValues("1.4.0", goodDigest)
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
