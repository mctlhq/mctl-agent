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
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"go.opentelemetry.io/otel/codes"

	"github.com/mctlhq/mctl-agent/internal/mctlclient"
	"github.com/mctlhq/mctl-agent/internal/skill"
	"github.com/mctlhq/mctl-agent/internal/telemetry"
	"github.com/mctlhq/mctl-agent/internal/telemetry/telemetrytest"
	"github.com/mctlhq/mctl-agent/internal/ticket"
)

// countingSkill matches everything and counts its diagnoses.
type countingSkill struct {
	mu    *sync.Mutex
	calls *int
}

func (countingSkill) Name() string        { return "counting_test" }
func (countingSkill) Version() string     { return "0" }
func (countingSkill) Description() string { return "test" }
func (countingSkill) Match(context.Context, *ticket.Ticket, skill.EvidenceSet) skill.MatchResult {
	return skill.MatchResult{Matched: true, Confidence: 1}
}
func (s countingSkill) Diagnose(context.Context, *ticket.Ticket, skill.EvidenceSet) (*skill.DiagnosisResult, error) {
	s.mu.Lock()
	*s.calls++
	s.mu.Unlock()
	return &skill.DiagnosisResult{Diagnosis: "diagnosed", Confidence: ticket.ConfidenceLow}, nil
}
func (countingSkill) Fix(context.Context, *ticket.Ticket, *skill.DiagnosisResult) (*skill.FixResult, error) {
	return nil, nil
}
func (countingSkill) RequiredCapabilities() []skill.CapabilityID { return nil }

// A ticket waits for one of the pipeline's slots as an in-memory copy, for
// longer than a diagnosis timeout in a burst. If something resolves the row
// meanwhile — the poller's recovered-app pass does so after ten minutes — the
// run must not start: it used to begin with a full-row Update from the copy,
// which wrote analyzing and a nil resolved_at over the resolution, published
// the incident to mctl-api again, and announced and diagnosed an application
// that was fine.
//
// The second row is the control: a ticket still at its queued status runs.
//
// Mutation check: replace SetStatusFromStatus in processTicketSync with the
// old `t.Status = analyzing; p.store.Update(ctx, t)` and "resolved while
// queued" fails on the status, resolved_at, the incident writes, the diagnosis
// and the Telegram message; drop `p.telegram.DropNewTicket(t)` and it fails on
// the Telegram message alone; drop `superseded = true` and it fails on the
// span.
func TestQueuedTicketIsNotStartedOnceItLeftItsStatus(t *testing.T) {
	for _, tc := range []struct {
		name          string
		resolveFirst  bool
		wantStatus    string
		wantDiagnoses int
		wantWrites    bool
		wantOutcome   string
	}{
		{"resolved while queued", true, ticket.StatusResolved, 0, false, telemetry.OutcomeSuperseded},
		{"still queued", false, ticket.StatusEscalated, 1, true, telemetry.OutcomeEscalated},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, store := newAsyncTestPipeline(t, 1)
			tg, bot := newRecordingTelegram(t)
			p.telegram = tg

			var mu sync.Mutex
			var incidentWrites []string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/v1/incidents") {
					mu.Lock()
					incidentWrites = append(incidentWrites, r.Method+" "+r.URL.Path)
					mu.Unlock()
				}
				_, _ = w.Write([]byte(`{}`))
			}))
			t.Cleanup(srv.Close)
			p.apiClient = mctlclient.NewClient(srv.URL, "test-token")

			calls := 0
			p.registry.Register(countingSkill{mu: &mu, calls: &calls})
			rec := telemetrytest.RecordSpans(t)

			queued := &ticket.Ticket{
				Source: ticket.SourcePolling, Type: ticket.TypeArgoCDDegraded,
				Tenant: "labs", Service: "web", Summary: "ArgoCD app labs-web health: Degraded",
				Severity: ticket.SeverityWarning,
			}
			if err := store.Create(ctx, queued); err != nil {
				t.Fatal(err)
			}
			if tc.resolveFirst {
				ok, err := store.ResolveByIDFromStatus(ctx, queued.ID, ticket.StatusOpen, "recovered")
				if err != nil || !ok {
					t.Fatalf("resolve: ok=%v err=%v", ok, err)
				}
			}

			p.processTicketSync(ctx, queued)

			got, err := store.Get(ctx, queued.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.Status != tc.wantStatus {
				t.Errorf("status = %q, want %q", got.Status, tc.wantStatus)
			}
			if tc.resolveFirst {
				if got.ResolvedAt == nil {
					t.Error("resolved_at was cleared: the resolved ticket was reopened")
				}
				if !strings.Contains(got.Analysis, "recovered") {
					t.Errorf("analysis = %q, want the resolution reason kept", got.Analysis)
				}
			}
			mu.Lock()
			gotCalls, gotWrites := calls, append([]string(nil), incidentWrites...)
			mu.Unlock()
			if gotCalls != tc.wantDiagnoses {
				t.Errorf("diagnoses = %d, want %d", gotCalls, tc.wantDiagnoses)
			}
			if (len(gotWrites) > 0) != tc.wantWrites {
				t.Errorf("incident writes to mctl-api = %v, want any=%v", gotWrites, tc.wantWrites)
			}
			if sent := bot.sent(); (len(sent) > 0) != tc.wantWrites {
				t.Errorf("telegram messages = %q, want any=%v", sent, tc.wantWrites)
			}

			root, ok := ticketSpans(rec.Ended(), queued.ID)["mctl_agent.process_ticket"]
			if !ok {
				t.Fatal("no root span")
			}
			if o := telemetrytest.SpanAttrs(root)[telemetry.TicketOutcome].AsString(); o != tc.wantOutcome {
				t.Errorf("outcome = %q, want %q", o, tc.wantOutcome)
			}
			if root.Status().Code == codes.Error {
				t.Error("root span is in error; neither row is a processing failure")
			}
		})
	}
}

// History evidence is handed to skills and to the LLM as how this kind of
// incident went before. A resolved ticket's ProposedFix is not a record of an
// applied fix: it is also kept for a suggestion nobody acted on and for a PR
// nobody merged, and the label must not say otherwise.
//
// Mutation check: put the literal "Fix applied" label back in
// collectHistoricalEvidence and both rows fail; make pastFixLabel ignore the
// PR and "with a pull request" fails.
func TestHistoryEvidenceDoesNotCallAProposalApplied(t *testing.T) {
	for _, tc := range []struct {
		name    string
		pr      int
		prURL   string
		want    string
		wantNot string
	}{
		{"no pull request", 0, "", "Fix proposed, no PR opened (not applied by the agent): scale to 2", "in a PR"},
		{"with a pull request", 7, "https://github.com/o/r/pull/7", "Fix proposed in a PR (not recorded whether it was merged): scale to 2", "no PR opened"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, store := newAsyncTestPipeline(t, 1)
			past := &ticket.Ticket{
				Source: ticket.SourcePolling, Type: ticket.TypeArgoCDDegraded, Tenant: "labs", Service: "web",
				Summary: "degraded", ProposedFix: "scale to 2", PRNumber: tc.pr, PRURL: tc.prURL,
			}
			if err := store.Create(ctx, past); err != nil {
				t.Fatal(err)
			}
			if ok, err := store.ResolveByIDFromStatus(ctx, past.ID, ticket.StatusOpen, "recovered"); err != nil || !ok {
				t.Fatalf("resolve: ok=%v err=%v", ok, err)
			}
			cur := &ticket.Ticket{Source: ticket.SourcePolling, Type: ticket.TypeArgoCDDegraded, Tenant: "labs", Service: "api", Summary: "degraded"}
			if err := store.Create(ctx, cur); err != nil {
				t.Fatal(err)
			}

			p.collectHistoricalEvidence(ctx, cur)

			history := evidenceTypes(t, store, cur.ID)["history"]
			if !strings.Contains(history, tc.want) {
				t.Errorf("history = %q, want it to contain %q", history, tc.want)
			}
			for _, not := range []string{"Fix applied", tc.wantNot} {
				if strings.Contains(history, not) {
					t.Errorf("history = %q, must not contain %q", history, not)
				}
			}
		})
	}
}
