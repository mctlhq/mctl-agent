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
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"

	"github.com/mctlhq/mctl-agent/internal/fixer"
	"github.com/mctlhq/mctl-agent/internal/gitopspath"
	"github.com/mctlhq/mctl-agent/internal/mctlclient"
	"github.com/mctlhq/mctl-agent/internal/skill"
	"github.com/mctlhq/mctl-agent/internal/telemetry"
	"github.com/mctlhq/mctl-agent/internal/telemetry/telemetrytest"
	"github.com/mctlhq/mctl-agent/internal/ticket"
)

// mediumSkill diagnoses every ticket as MEDIUM and fixable, and is not
// auto-merge safe, so the pipeline proposes the fix without opening a PR.
type mediumSkill struct{}

func (mediumSkill) Name() string        { return "medium_test" }
func (mediumSkill) Version() string     { return "0" }
func (mediumSkill) Description() string { return "test" }
func (mediumSkill) Match(context.Context, *ticket.Ticket, skill.EvidenceSet) skill.MatchResult {
	return skill.MatchResult{Matched: true, Confidence: 0.9}
}
func (mediumSkill) Diagnose(context.Context, *ticket.Ticket, skill.EvidenceSet) (*skill.DiagnosisResult, error) {
	return &skill.DiagnosisResult{Diagnosis: "d", Confidence: ticket.ConfidenceMedium, Fixable: true, SkillName: "medium_test"}, nil
}
func (mediumSkill) Fix(context.Context, *ticket.Ticket, *skill.DiagnosisResult) (*skill.FixResult, error) {
	return nil, nil
}
func (mediumSkill) RequiredCapabilities() []skill.CapabilityID { return nil }

// ticketSpans returns, by name, the spans of the one trace whose root is
// ticketID's. The recorder is process-global, so it also sees spans from
// pipeline goroutines that earlier tests left running; selecting by the
// ticket's own trace keeps those out.
func ticketSpans(spans []sdktrace.ReadOnlySpan, ticketID string) map[string]sdktrace.ReadOnlySpan {
	var tid trace.TraceID
	for _, s := range spans {
		if s.Name() == "mctl_agent.process_ticket" && telemetrytest.SpanAttrs(s)[telemetry.TicketID].AsString() == ticketID {
			tid = s.SpanContext().TraceID()
		}
	}
	out := map[string]sdktrace.ReadOnlySpan{}
	for _, s := range spans {
		if tid.IsValid() && s.SpanContext().TraceID() == tid {
			out[s.Name()] = s
		}
	}
	return out
}

func processTraced(t *testing.T, p *Pipeline, store *ticket.Store) (map[string]sdktrace.ReadOnlySpan, *ticket.Ticket) {
	t.Helper()
	rec := telemetrytest.RecordSpans(t)
	tk := &ticket.Ticket{Source: ticket.SourceAlertManager, Type: ticket.TypePodCrashloop, Tenant: "billing", Service: "api", Summary: "crashloop", Status: ticket.StatusAnalyzing}
	if err := store.Create(context.Background(), tk); err != nil {
		t.Fatal(err)
	}
	p.processTicketSync(context.Background(), tk)
	return ticketSpans(rec.Ended(), tk.ID), tk
}

// Every stage span must hang off the one root span for the ticket, and the
// root must carry the ticket id, type and the outcome it ended with.
//
// Mutation check: drop the outcome defer in processTicketSync, or start a
// stage span from the pre-root context, and this test fails.
func TestProcessTicketEmitsOneTraceWithOutcome(t *testing.T) {
	p, store := newAsyncTestPipeline(t, 1)
	spans, tk := processTraced(t, p, store)

	root, ok := spans["mctl_agent.process_ticket"]
	if !ok {
		t.Fatalf("no root span; got %v", keys(spans))
	}
	ra := telemetrytest.SpanAttrs(root)
	if ra[telemetry.TicketID].AsString() != tk.ID || ra[telemetry.TicketType].AsString() != ticket.TypePodCrashloop {
		t.Errorf("root ids = %v", ra)
	}
	if got := ra[telemetry.TicketOutcome].AsString(); got != telemetry.OutcomeEscalated {
		t.Errorf("outcome = %q, want %q (no skill matched)", got, telemetry.OutcomeEscalated)
	}
	if root.Status().Code == codes.Error {
		t.Errorf("an escalation is a terminal state, not an error: %q", root.Status().Description)
	}
	for _, name := range []string{"mctl_agent.collect_evidence", "mctl_agent.match_skills"} {
		s, ok := spans[name]
		if !ok {
			t.Errorf("missing %s span", name)
			continue
		}
		if s.Parent().SpanID() != root.SpanContext().SpanID() || s.SpanContext().TraceID() != root.SpanContext().TraceID() {
			t.Errorf("%s is not a child of the root span", name)
		}
	}
}

// Mutation check: remove the confidence/fixable attributes from the
// diagnose span, or map fix_proposed onto another outcome, and this fails.
func TestProcessTicketDiagnoseSpanCarriesTheVerdict(t *testing.T) {
	p, store := newAsyncTestPipeline(t, 1)
	p.registry.Register(mediumSkill{})
	spans, _ := processTraced(t, p, store)

	d, ok := spans["mctl_agent.diagnose"]
	if !ok {
		t.Fatalf("no diagnose span; got %v", keys(spans))
	}
	da := telemetrytest.SpanAttrs(d)
	if da[telemetry.SkillName].AsString() != "medium_test" ||
		da[telemetry.DiagnosisConfidence].AsString() != string(ticket.ConfidenceMedium) ||
		!da[telemetry.DiagnosisFixable].AsBool() {
		t.Errorf("diagnose attributes = %v", da)
	}
	if got := telemetrytest.SpanAttrs(spans["mctl_agent.process_ticket"])[telemetry.TicketOutcome].AsString(); got != telemetry.OutcomeFixProposed {
		t.Errorf("outcome = %q, want %q", got, telemetry.OutcomeFixProposed)
	}
	if d.Parent().SpanID() != spans["mctl_agent.process_ticket"].SpanContext().SpanID() {
		t.Error("diagnose span is not a child of the root span")
	}
}

func TestTicketOutcomeMapping(t *testing.T) {
	for _, tc := range []struct {
		tk   ticket.Ticket
		want string
	}{
		{ticket.Ticket{PRNumber: 7, Status: ticket.StatusFixProposed}, telemetry.OutcomePRCreated},
		{ticket.Ticket{Status: ticket.StatusFixProposed}, telemetry.OutcomeFixProposed},
		{ticket.Ticket{Status: ticket.StatusFixApplied}, telemetry.OutcomeFixProposed},
		{ticket.Ticket{Status: ticket.StatusEscalated}, telemetry.OutcomeEscalated},
		{ticket.Ticket{Status: ticket.StatusAnalyzing}, telemetry.OutcomeFailed},
	} {
		if got := ticketOutcome(&tc.tk); got != tc.want {
			t.Errorf("ticketOutcome(status=%s pr=%d) = %q, want %q", tc.tk.Status, tc.tk.PRNumber, got, tc.want)
		}
	}
}

// processTicketSync reassigns t from the post-evidence reload, which is nil
// when that read fails. The deferred outcome must survive it: the defer
// runs on the ticket goroutine, where a panic takes the whole agent down.
//
// Mutation check: drop the nil case from ticketOutcome and this test panics.
func TestProcessTicketSpanSurvivesAFailedReload(t *testing.T) {
	p, store := newAsyncTestPipeline(t, 1)
	// Close the store from inside evidence collection (the first GET to
	// mctl-api), so the status update before it succeeds and only the
	// reload after it fails.
	var once sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			once.Do(func() { _ = store.Close() })
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)
	p.apiClient = mctlclient.NewClient(srv.URL, "test-token")
	rec := telemetrytest.RecordSpans(t)
	tk := &ticket.Ticket{Source: ticket.SourceAlertManager, Type: ticket.TypePodCrashloop, Tenant: "billing", Service: "api", Summary: "crashloop", Status: ticket.StatusAnalyzing}
	if err := store.Create(context.Background(), tk); err != nil {
		t.Fatal(err)
	}

	p.processTicketSync(context.Background(), tk)

	root, ok := ticketSpans(rec.Ended(), tk.ID)["mctl_agent.process_ticket"]
	if !ok {
		t.Fatal("no root span")
	}
	if got := telemetrytest.SpanAttrs(root)[telemetry.TicketOutcome].AsString(); got != telemetry.OutcomeFailed {
		t.Errorf("outcome = %q, want %q", got, telemetry.OutcomeFailed)
	}
	if root.Status().Code != codes.Error {
		t.Error("a failed reload must leave the root span in error")
	}
}

// stubGitHubFixer is a GitHubFixer whose API calls land on a stub that
// serves one file and opens PR #42.
func stubGitHubFixer(t *testing.T, store *ticket.Store) *fixer.GitHubFixer {
	t.Helper()
	file := map[string]any{"type": "file", "encoding": "base64", "sha": "f1", "content": base64.StdEncoding.EncodeToString([]byte("replicas: 1\n"))}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var body any
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/contents/"):
			body = file
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/git/ref/"):
			body = map[string]any{"ref": "refs/heads/main", "object": map[string]any{"sha": "m1"}}
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/git/refs"):
			w.WriteHeader(http.StatusCreated)
			body = map[string]any{"ref": "refs/heads/x"}
		case r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/contents/"):
			body = map[string]any{}
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/pulls"):
			w.WriteHeader(http.StatusCreated)
			body = map[string]any{"number": 42, "html_url": "https://github.com/owner/repo/pull/42"}
		default:
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(srv.Close)

	f := fixer.NewGitHubFixer("test-token", "", "owner", "repo", store, false, 10, 10, gitopspath.DefaultAllowlist())
	if err := f.SetBaseURL(srv.URL + "/"); err != nil {
		t.Fatal(err)
	}
	return f
}

// fixTrace runs one ticket through a HIGH-confidence fixable stub skill and
// returns the spans it produced.
func fixTrace(t *testing.T, s stubSkill, withGitHub bool) map[string]sdktrace.ReadOnlySpan {
	t.Helper()
	p, store := newAsyncTestPipeline(t, 1)
	if withGitHub {
		p.github = stubGitHubFixer(t, store)
	}
	p.registry.Register(s)
	spans, _ := processTraced(t, p, store)
	return spans
}

// The PR path: the fix span hangs off the root, names the repository it
// read and the PR it opened, and the ticket's outcome is pr_created.
//
// Mutation check: drop the PR-number or repository attribute, or mark the
// span failed unconditionally, and this fails.
func TestFixSpanOnTheOpenedPR(t *testing.T) {
	spans := fixTrace(t, stubSkill{fix: &skill.FixResult{
		Applied: true, FilePath: "platform-gitops/services/billing/api/values.yaml",
		NewContent: "replicas: 2\n", Summary: "scale",
	}}, true)

	fix, ok := spans["mctl_agent.fix"]
	if !ok {
		t.Fatalf("no fix span; got %v", keys(spans))
	}
	root := spans["mctl_agent.process_ticket"]
	if fix.Parent().SpanID() != root.SpanContext().SpanID() {
		t.Error("fix span is not a child of the root span")
	}
	fa := telemetrytest.SpanAttrs(fix)
	if fa[telemetry.SkillName].AsString() != "stub" || fa[telemetry.RepositoryName].AsString() != "owner/repo" || fa[telemetry.PRNumber].AsInt64() != 42 {
		t.Errorf("fix attributes = %v", fa)
	}
	if fix.Status().Code == codes.Error {
		t.Errorf("fix span failed on the success path: %q", fix.Status().Description)
	}
	if got := telemetrytest.SpanAttrs(root)[telemetry.TicketOutcome].AsString(); got != telemetry.OutcomePRCreated {
		t.Errorf("outcome = %q, want %q", got, telemetry.OutcomePRCreated)
	}
}

// A skill declining its own fix is a decision, not a failure, and it never
// touched the repository.
//
// Mutation check: mark the span failed whenever no PR exists, or set the
// repository name before the file is read, and this fails.
func TestFixSpanOnADeclinedFix(t *testing.T) {
	spans := fixTrace(t, stubSkill{fix: &skill.FixResult{Applied: false, Summary: "declined"}}, true)
	fix, ok := spans["mctl_agent.fix"]
	if !ok {
		t.Fatalf("no fix span; got %v", keys(spans))
	}
	if fix.Status().Code == codes.Error {
		t.Errorf("a declined fix marked the span failed: %q", fix.Status().Description)
	}
	if _, set := telemetrytest.SpanAttrs(fix)[telemetry.RepositoryName]; set {
		t.Error("repository name set although the repository was never read")
	}
}

// Mutation check: drop failed() from the Fix-error branch and this fails.
func TestFixSpanOnAFailedFix(t *testing.T) {
	spans := fixTrace(t, stubSkill{fixErr: errors.New("boom")}, false)
	fix, ok := spans["mctl_agent.fix"]
	if !ok {
		t.Fatalf("no fix span; got %v", keys(spans))
	}
	if fix.Status().Code != codes.Error {
		t.Error("a failed fix left the span unmarked")
	}
}

func keys(m map[string]sdktrace.ReadOnlySpan) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
