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
	"strings"
	"testing"

	"github.com/mctlhq/mctl-agent/internal/ticket"
)

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
	if err := store.Create(tk); err != nil {
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
	tight := NewGitHubFixer("", "", "owner", "repo", store, false, 1, 20)
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
	loose := NewGitHubFixer("", "", "owner", "repo", store, false, 5, 20)
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

	tight := NewGitHubFixer("", "", "owner", "repo", store, false, 5, 1)
	_, _, err := tight.CreatePR(context.Background(), req)
	if err == nil {
		t.Fatal("expected daily limit error, got nil")
	}
	if !strings.Contains(err.Error(), "daily PR limit reached (1/1)") {
		t.Errorf("error = %q, want it to reference the configured limit 1/1", err.Error())
	}
}
