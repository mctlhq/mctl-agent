package monitor

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGitHubWebhookRejectsMissingSecret(t *testing.T) {
	store := newTestStore(t)
	h := NewGitHubWebhookHandler(store, "", nil)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/github-webhook", bytes.NewReader([]byte(`{}`)))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("empty secret: status=%d, want 401", w.Code)
	}
}

func TestGitHubWebhookRejectsBadSignature(t *testing.T) {
	store := newTestStore(t)
	h := NewGitHubWebhookHandler(store, "whsec", nil)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/github-webhook", bytes.NewReader([]byte(`{}`)))
	req.Header.Set("X-Hub-Signature-256", "sha256=deadbeef")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("bad signature: status=%d, want 401", w.Code)
	}
}

// A failed workflow_run with a head_sha shorter than 8 characters (or none
// at all) used to panic on sha[:8]. It must create the ticket instead.
func TestProcessWorkflowRunShortHeadSHA(t *testing.T) {
	for _, sha := range []string{"", "abc", "0123456789abcdef"} {
		t.Run("sha="+sha, func(t *testing.T) {
			store := newTestStore(t)
			h := NewGitHubWebhookHandler(store, "whsec", nil)

			var ev workflowRunEvent
			ev.Action = "completed"
			ev.WorkflowRun.Conclusion = "failure"
			ev.WorkflowRun.Name = "ci"
			ev.WorkflowRun.HeadBranch = "main"
			ev.WorkflowRun.HeadSHA = sha
			ev.Repository.Name = "svc"
			ev.Repository.FullName = "mctlhq/svc"

			h.processWorkflowRun(context.Background(), ev)

			all, err := store.ListAll(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if len(all) != 1 {
				t.Fatalf("tickets=%d, want 1", len(all))
			}
			want := "commit: " + shortSHA(sha) + ")"
			if !strings.Contains(all[0].Summary, want) {
				t.Fatalf("summary %q does not contain %q", all[0].Summary, want)
			}
		})
	}
	if got := shortSHA("0123456789abcdef"); got != "01234567" {
		t.Fatalf("shortSHA(long)=%q, want 01234567", got)
	}
}
