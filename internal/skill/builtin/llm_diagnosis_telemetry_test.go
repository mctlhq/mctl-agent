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

package builtin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"

	"github.com/mctlhq/mctl-agent/internal/metrics"
	"github.com/mctlhq/mctl-agent/internal/skill"
	"github.com/mctlhq/mctl-agent/internal/telemetry"
	"github.com/mctlhq/mctl-agent/internal/ticket"
)

const llmSecretMarker = "SECRET-PROMPT-AND-BODY-MARKER"

func stubLLM(t *testing.T, status int, body string) *LLMDiagnosisSkill {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	s := NewLLMDiagnosisSkill("test-key")
	s.apiURL = srv.URL
	return s
}

func llmTicket() *ticket.Ticket {
	// The summary lands in the prompt; it must never reach the span.
	return &ticket.Ticket{ID: "tkt-38", Type: ticket.TypePodCrashloop, Tenant: "billing", Service: "api", Summary: llmSecretMarker}
}

func TestLLMDiagnosisRecordsUsageOnSpanAndCounters(t *testing.T) {
	rec := telemetry.RecordSpans(t)
	s := stubLLM(t, http.StatusOK, `{"content":[{"type":"text","text":"{\"diagnosis\":\"d\",\"confidence\":\"HIGH\",\"fixable\":false}"}],"usage":{"input_tokens":1234,"output_tokens":56}}`)

	tk := llmTicket()
	in := metrics.LLMTokens.WithLabelValues("claude-sonnet-5", "llm_diagnosis", tk.Type, "input")
	out := metrics.LLMTokens.WithLabelValues("claude-sonnet-5", "llm_diagnosis", tk.Type, "output")
	ok := metrics.LLMRequests.WithLabelValues("claude-sonnet-5", "llm_diagnosis", "ok")
	in0, out0, ok0 := testutil.ToFloat64(in), testutil.ToFloat64(out), testutil.ToFloat64(ok)

	res, err := s.Diagnose(context.Background(), tk, skill.NewEvidenceSet(nil))
	if err != nil {
		t.Fatal(err)
	}
	if res.Confidence != ticket.ConfidenceHigh {
		t.Fatalf("diagnosis not parsed: %+v", res)
	}
	if d := testutil.ToFloat64(in) - in0; d != 1234 {
		t.Errorf("input token counter delta = %v, want 1234", d)
	}
	if d := testutil.ToFloat64(out) - out0; d != 56 {
		t.Errorf("output token counter delta = %v, want 56", d)
	}
	if d := testutil.ToFloat64(ok) - ok0; d != 1 {
		t.Errorf("ok request counter delta = %v, want 1", d)
	}

	spans := rec.Ended()
	if len(spans) != 1 {
		t.Fatalf("got %d spans, want 1", len(spans))
	}
	sp := spans[0]
	if sp.Name() != "chat claude-sonnet-5" || sp.SpanKind() != trace.SpanKindClient {
		t.Errorf("span = %q kind %v, want client span \"chat claude-sonnet-5\"", sp.Name(), sp.SpanKind())
	}
	a := telemetry.SpanAttrs(sp)
	for k, want := range map[string]any{
		"gen_ai.operation.name":      "chat",
		"gen_ai.provider.name":       "anthropic",
		"gen_ai.request.model":       "claude-sonnet-5",
		"gen_ai.usage.input_tokens":  int64(1234),
		"gen_ai.usage.output_tokens": int64(56),
		"mctl.skill.name":            "llm_diagnosis",
		"mctl.ticket.id":             "tkt-38",
		"mctl.ticket.type":           tk.Type,
	} {
		v, found := a[attribute.Key(k)]
		if !found || v.AsInterface() != want {
			t.Errorf("attribute %s = %v (present %v), want %v", k, v.AsInterface(), found, want)
		}
	}
	if sp.Status().Code == codes.Error {
		t.Errorf("successful call has error status %q", sp.Status().Description)
	}
	assertNoMarker(t, sp)
}

func TestLLMDiagnosisFailureSetsErrorTypeWithoutBody(t *testing.T) {
	rec := telemetry.RecordSpans(t)
	s := stubLLM(t, http.StatusTooManyRequests, `{"error":"`+llmSecretMarker+`"}`)
	errs := metrics.LLMRequests.WithLabelValues("claude-sonnet-5", "llm_diagnosis", "error")
	e0 := testutil.ToFloat64(errs)

	if _, err := s.Diagnose(context.Background(), llmTicket(), skill.NewEvidenceSet(nil)); err == nil {
		t.Fatal("expected an error for a 429")
	}
	if d := testutil.ToFloat64(errs) - e0; d != 1 {
		t.Errorf("error request counter delta = %v, want 1", d)
	}
	spans := rec.Ended()
	if len(spans) != 1 {
		t.Fatalf("got %d spans, want 1", len(spans))
	}
	sp := spans[0]
	if sp.Status().Code != codes.Error {
		t.Errorf("status = %v, want Error", sp.Status().Code)
	}
	if v := telemetry.SpanAttrs(sp)[attribute.Key("error.type")]; v.AsString() != "429" {
		t.Errorf("error.type = %q, want \"429\"", v.AsString())
	}
	assertNoMarker(t, sp)
}

// assertNoMarker fails if the prompt or the provider's body leaked into the
// span through an attribute, the status description or an event.
func assertNoMarker(t *testing.T, sp sdktrace.ReadOnlySpan) {
	t.Helper()
	parts := []string{sp.Name(), sp.Status().Description}
	for _, kv := range sp.Attributes() {
		parts = append(parts, kv.Value.String())
	}
	for _, ev := range sp.Events() {
		parts = append(parts, ev.Name)
		for _, kv := range ev.Attributes {
			parts = append(parts, kv.Value.String())
		}
	}
	if strings.Contains(strings.Join(parts, "\n"), llmSecretMarker) {
		t.Fatal("prompt or response body recorded on the span")
	}
}
