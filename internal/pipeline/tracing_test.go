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
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

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

func spansByName(spans []sdktrace.ReadOnlySpan) map[string]sdktrace.ReadOnlySpan {
	out := map[string]sdktrace.ReadOnlySpan{}
	for _, s := range spans {
		out[s.Name()] = s
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
	return spansByName(rec.Ended()), tk
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
	if n := telemetrytest.SpanAttrs(spans["mctl_agent.match_skills"])[attribute.Key("mctl_agent.matched_skills.count")]; n.AsInt64() != 0 {
		t.Errorf("matched_skills.count = %d, want 0", n.AsInt64())
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

func keys(m map[string]sdktrace.ReadOnlySpan) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
