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

package fixer

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/mctlhq/mctl-agent/internal/gitopspath"
	"github.com/mctlhq/mctl-agent/internal/ticket"
)

// ctx is the context this package's store calls run under; nothing here
// exercises cancellation.
var ctx = context.Background()

func newTestTicketStore(t *testing.T) *ticket.Store {
	t.Helper()
	store, err := ticket.NewStore(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// seedPRTicket records one ticket with a PR URL, which is what
// CountPRsInWindow counts.
func seedPRTicket(t *testing.T, store *ticket.Store) {
	t.Helper()
	tk := &ticket.Ticket{
		Source:  ticket.SourcePolling,
		Type:    ticket.TypePodCrashloop,
		Tenant:  "t",
		Service: "s",
		PRURL:   "https://github.com/mctlhq/mctl-gitops/pull/1",
	}
	if err := store.Create(ctx, tk); err != nil {
		t.Fatal(err)
	}
}

// TestCreatePRHourlyLimitUsesConfiguredValue pins the fix for the bug where
// GitHubFixer.CreatePR compared against the hardcoded literal 5 instead of
// the configured maxPRPerHour, silently ignoring MAX_PR_PER_HOUR.
func TestCreatePRHourlyLimitUsesConfiguredValue(t *testing.T) {
	store := newTestTicketStore(t)
	seedPRTicket(t, store)

	req := PRRequest{
		Ticket: &ticket.Ticket{ID: "11111111-1111-1111-1111-111111111111", Service: "s", Type: ticket.TypePodCrashloop},
	}

	// A fixer configured with maxPRPerHour=1 must reject once one PR has
	// already been created in the window.
	tight := NewGitHubFixer("", "", "owner", "repo", store, false, 1, 20, gitopspath.DefaultAllowlist())
	_, _, err := tight.CreatePR(context.Background(), req)
	if err == nil {
		t.Fatal("expected hourly limit error, got nil")
	}
	if !strings.Contains(err.Error(), "hourly PR limit reached (1/1)") {
		t.Errorf("error = %q, want it to reference the configured limit 1/1", err.Error())
	}

	// A fixer at the historical default (5) must NOT hit the limit with the
	// same single seeded ticket, proving the value is actually threaded
	// through rather than coincidentally still comparing to a stray literal.
	loose := NewGitHubFixer("", "", "owner", "repo", store, false, 5, 20, gitopspath.DefaultAllowlist())
	_, _, err = loose.CreatePR(context.Background(), req)
	if err != nil && strings.Contains(err.Error(), "hourly PR limit reached") {
		t.Errorf("did not expect hourly limit to trigger with maxPRPerHour=5, got: %v", err)
	}
}

// TestCreatePRDailyLimitUsesConfiguredValue mirrors the hourly case for the
// daily limit.
func TestCreatePRDailyLimitUsesConfiguredValue(t *testing.T) {
	store := newTestTicketStore(t)
	seedPRTicket(t, store)

	req := PRRequest{
		Ticket: &ticket.Ticket{ID: "22222222-2222-2222-2222-222222222222", Service: "s", Type: ticket.TypePodCrashloop},
	}

	tight := NewGitHubFixer("", "", "owner", "repo", store, false, 5, 1, gitopspath.DefaultAllowlist())
	_, _, err := tight.CreatePR(context.Background(), req)
	if err == nil {
		t.Fatal("expected daily limit error, got nil")
	}
	if !strings.Contains(err.Error(), "daily PR limit reached (1/1)") {
		t.Errorf("error = %q, want it to reference the configured limit 1/1", err.Error())
	}
}

// TestCreatePRRejectsOutOfAllowlistPath asserts a traversal or off-prefix
// FilePath is rejected before any GitHub API call is made (no client is
// configured, so a call through f.client would panic on a nil pointer
// dereference and fail the test).
func TestCreatePRRejectsOutOfAllowlistPath(t *testing.T) {
	tests := []struct {
		name string
		path string
	}{
		{"traversal", "../.github/workflows/ci.yml"},
		{"absolute", "/etc/passwd"},
		{"off-prefix", "bootstrap/x.yaml"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := newTestTicketStore(t)
			f := &GitHubFixer{
				owner:        "mctlhq",
				repo:         "mctl-gitops",
				store:        store,
				maxPRPerHour: 5,
				maxPRPerDay:  20,
				allowlist:    gitopspath.DefaultAllowlist(),
			}
			req := PRRequest{
				Ticket:   &ticket.Ticket{ID: "33333333-3333-3333-3333-333333333333", Service: "s", Type: ticket.TypePodCrashloop},
				FilePath: tt.path,
			}
			_, _, err := f.CreatePR(context.Background(), req)
			if err == nil {
				t.Fatal("expected allowlist rejection, got nil")
			}
			if !strings.Contains(err.Error(), "gitops path rejected") {
				t.Errorf("error = %q, want it to mention gitops path rejection", err.Error())
			}
		})
	}
}

// TestGetFileContentRejectsOutOfAllowlistPath mirrors the CreatePR case for
// the read path.
func TestGetFileContentRejectsOutOfAllowlistPath(t *testing.T) {
	f := &GitHubFixer{
		owner:     "mctlhq",
		repo:      "mctl-gitops",
		allowlist: gitopspath.DefaultAllowlist(),
	}
	_, err := f.GetFileContent(context.Background(), "../.github/workflows/ci.yml", "main")
	if err == nil {
		t.Fatal("expected allowlist rejection, got nil")
	}
	if !strings.Contains(err.Error(), "gitops path rejected") {
		t.Errorf("error = %q, want it to mention gitops path rejection", err.Error())
	}
}

// TestCreatePRRejectsSymlink asserts a symlink-typed existing blob is
// rejected before UpdateFile is ever called.
func TestCreatePRRejectsSymlink(t *testing.T) {
	const path = "platform-gitops/services/acme/api/values.yaml"
	store := newTestTicketStore(t)

	f, cleanup := fakeGH(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/git/ref/heads/main"):
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ref":    "refs/heads/main",
				"object": map[string]any{"sha": "mainsha"},
			})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/git/refs"):
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ref":    "refs/heads/agent/fix/x",
				"object": map[string]any{"sha": "mainsha"},
			})
		case strings.HasPrefix(r.URL.Path, "/repos/mctlhq/mctl-gitops/contents/"):
			if r.Method != http.MethodGet {
				t.Fatalf("unexpected %s to contents endpoint; UpdateFile must not run after a symlink is detected", r.Method)
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"name": "values.yaml",
				"path": path,
				"sha":  "blobsha",
				"type": "symlink",
			})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	})
	defer cleanup()
	f.store = store
	f.maxPRPerHour = 5
	f.maxPRPerDay = 20

	req := PRRequest{
		Ticket:   &ticket.Ticket{ID: "44444444-4444-4444-4444-444444444444", Service: "api", Type: ticket.TypePodCrashloop},
		FilePath: path,
	}
	_, _, err := f.CreatePR(context.Background(), req)
	if err == nil {
		t.Fatal("expected symlink rejection, got nil")
	}
	if !strings.Contains(err.Error(), "symlink") {
		t.Errorf("error = %q, want it to mention symlink", err.Error())
	}
}

// TestGetFileContentRejectsSymlink mirrors the CreatePR symlink case for the
// read path.
func TestGetFileContentRejectsSymlink(t *testing.T) {
	const path = "platform-gitops/services/acme/api/values.yaml"

	f, cleanup := fakeGH(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"name": "values.yaml",
			"path": path,
			"sha":  "blobsha",
			"type": "symlink",
		})
	})
	defer cleanup()

	_, err := f.GetFileContent(context.Background(), path, "main")
	if err == nil {
		t.Fatal("expected symlink rejection, got nil")
	}
	if !strings.Contains(err.Error(), "symlink") {
		t.Errorf("error = %q, want it to mention symlink", err.Error())
	}
}
