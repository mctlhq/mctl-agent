package monitor

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mctlhq/mctl-agent/internal/ticket"
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

// A dedup lookup that fails does not say there is no ticket. The handler used
// to log the error and carry on as if nothing was found, opening and
// announcing a second ticket for a workflow failure that already had one.
//
// The second row is the control: with a readable store the same event opens
// a ticket.
//
// Mutation check: delete the `return` after the "dedup check failed" log and
// "lookup fails" fails.
func TestGitHubWebhookFailedDedupLookupOpensNothing(t *testing.T) {
	for _, tc := range []struct {
		name        string
		breakLookup bool
		wantOpened  int
	}{
		{"lookup fails", true, 0},
		{"lookup succeeds", false, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newTestStore(t)
			opened := 0
			h := NewGitHubWebhookHandler(store, "", func(*ticket.Ticket) { opened++ })

			var ev workflowRunEvent
			ev.Action = "completed"
			ev.WorkflowRun.Name, ev.WorkflowRun.Conclusion = "ci", "failure"
			ev.Repository.Name, ev.Repository.FullName = "mctl-api", "mctlhq/mctl-api"

			if tc.breakLookup {
				// An existing ticket for the same key whose row cannot be
				// decoded: the lookup errors, every other statement works.
				existing := &ticket.Ticket{
					Source: ticket.SourceGitHubWebhook, Type: ticket.TypeGitHubActionsFailed,
					Tenant: "platform", Service: "mctl-api", Summary: "ci failed",
				}
				if err := store.Create(context.Background(), existing); err != nil {
					t.Fatal(err)
				}
				if _, err := store.DB().Exec(`UPDATE tickets SET created_at='~not a time~' WHERE id=?`, existing.ID); err != nil {
					t.Fatal(err)
				}
				if _, err := store.FindDuplicate(context.Background(), "platform", "mctl-api", ticket.TypeGitHubActionsFailed, ""); err == nil {
					t.Fatal("precondition: the dedup lookup must fail")
				}
			}

			h.processWorkflowRun(context.Background(), ev)

			if opened != tc.wantOpened {
				t.Errorf("tickets handed to the pipeline = %d, want %d", opened, tc.wantOpened)
			}
			var n int
			if err := store.DB().QueryRow(`SELECT COUNT(*) FROM tickets`).Scan(&n); err != nil {
				t.Fatal(err)
			}
			want := tc.wantOpened
			if tc.breakLookup {
				want = 1 // the pre-existing row only
			}
			if n != want {
				t.Errorf("ticket rows = %d, want %d", n, want)
			}
		})
	}
}
