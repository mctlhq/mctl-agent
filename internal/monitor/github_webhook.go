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

package monitor

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/mctlhq/mctl-agent/internal/ticket"
)

// GitHubWebhookHandler handles GitHub webhook events (workflow_run).
type GitHubWebhookHandler struct {
	store    *ticket.Store
	secret   string
	onTicket func(*ticket.Ticket)
}

// NewGitHubWebhookHandler creates a new GitHub webhook handler.
func NewGitHubWebhookHandler(store *ticket.Store, secret string, onTicket func(*ticket.Ticket)) *GitHubWebhookHandler {
	return &GitHubWebhookHandler{store: store, secret: secret, onTicket: onTicket}
}

type workflowRunEvent struct {
	Action      string `json:"action"`
	WorkflowRun struct {
		ID         int64  `json:"id"`
		Name       string `json:"name"`
		Conclusion string `json:"conclusion"`
		HTMLURL    string `json:"html_url"`
		HeadBranch string `json:"head_branch"`
		HeadSHA    string `json:"head_sha"`
	} `json:"workflow_run"`
	Repository struct {
		Name     string `json:"name"`
		FullName string `json:"full_name"`
	} `json:"repository"`
}

// ServeHTTP handles POST /api/v1/github-webhook.
func (h *GitHubWebhookHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "failed to read body", http.StatusBadRequest)
		return
	}

	// Validate signature.
	if h.secret != "" {
		sig := r.Header.Get("X-Hub-Signature-256")
		if !h.verifySignature(body, sig) {
			http.Error(w, "invalid signature", http.StatusUnauthorized)
			return
		}
	}

	eventType := r.Header.Get("X-GitHub-Event")
	if eventType != "workflow_run" {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ignored","reason":"not workflow_run event"}`))
		return
	}

	var event workflowRunEvent
	if err := json.Unmarshal(body, &event); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}

	h.processWorkflowRun(event)

	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"accepted"}`))
}

func (h *GitHubWebhookHandler) processWorkflowRun(event workflowRunEvent) {
	if event.Action != "completed" || event.WorkflowRun.Conclusion != "failure" {
		return
	}

	repo := event.Repository.Name
	workflow := event.WorkflowRun.Name
	branch := event.WorkflowRun.HeadBranch
	sha := event.WorkflowRun.HeadSHA
	runURL := event.WorkflowRun.HTMLURL

	// Convention: repo name = service name.
	service := repo
	// Tenant defaults to repo org — for mctlhq repos, map to "platform".
	tenant := "platform"
	if parts := strings.SplitN(event.Repository.FullName, "/", 2); len(parts) == 2 && parts[0] != "mctlhq" {
		tenant = parts[0]
	}

	// Dedup check.
	existing, err := h.store.FindDuplicate(tenant, service, ticket.TypeGitHubActionsFailed)
	if err != nil {
		slog.Error("dedup check failed for github webhook", "error", err)
	}
	if existing != nil {
		// Bump UpdatedAt so the stale-ticket GC recognizes the failure
		// is still recurring; otherwise a workflow that keeps failing
		// on every push would silently auto-resolve after StaleAfter.
		if err := h.store.Touch(existing.ID); err != nil {
			slog.Error("failed to touch github webhook ticket", "error", err, "id", existing.ID)
		}
		slog.Debug("duplicate GitHub Actions ticket exists", "id", existing.ID, "repo", repo)
		return
	}

	summary := fmt.Sprintf("GitHub Actions workflow %q failed on %s (branch: %s, commit: %s)",
		workflow, repo, branch, sha[:8])

	t := &ticket.Ticket{
		Source:   ticket.SourceGitHubWebhook,
		Type:     ticket.TypeGitHubActionsFailed,
		Tenant:   tenant,
		Service:  service,
		Summary:  summary,
		Severity: ticket.SeverityWarning,
	}

	if err := h.store.Create(t); err != nil {
		slog.Error("failed to create ticket from github webhook", "error", err, "repo", repo)
		return
	}

	// Store evidence.
	evidence := map[string]string{
		"repo":     event.Repository.FullName,
		"workflow": workflow,
		"branch":   branch,
		"sha":      sha,
		"run_url":  runURL,
	}
	evJSON, _ := json.Marshal(evidence)
	_ = h.store.AddEvidence(t.ID, ticket.Evidence{
		Type:        "github_workflow_run",
		Content:     string(evJSON),
		CollectedAt: time.Now().UTC(),
	})

	slog.Info("ticket created from GitHub Actions failure",
		"id", t.ID, "repo", repo, "workflow", workflow, "branch", branch)

	if h.onTicket != nil {
		h.onTicket(t)
	}
}

func (h *GitHubWebhookHandler) verifySignature(payload []byte, signature string) bool {
	if !strings.HasPrefix(signature, "sha256=") {
		return false
	}
	sig, err := hex.DecodeString(strings.TrimPrefix(signature, "sha256="))
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(h.secret))
	mac.Write(payload)
	expected := mac.Sum(nil)
	return hmac.Equal(sig, expected)
}
