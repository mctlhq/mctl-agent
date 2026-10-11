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
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/mctlhq/mctl-agent/internal/mctlclient"
	"github.com/mctlhq/mctl-agent/internal/telemetry"
	"github.com/mctlhq/mctl-agent/internal/ticket"
)

// syncBuffer is a log sink the pipeline's goroutines and the test can share.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

// captureLogs routes the default slog logger to JSON in memory for the test
// and returns the records that name ticketID.
func captureLogs(t *testing.T) func(ticketID string) []map[string]any {
	t.Helper()
	sink := &syncBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(sink, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return func(ticketID string) []map[string]any {
		sink.mu.Lock()
		defer sink.mu.Unlock()
		var out []map[string]any
		sc := bufio.NewScanner(bytes.NewReader(sink.buf.Bytes()))
		for sc.Scan() {
			var m map[string]any
			if json.Unmarshal(sc.Bytes(), &m) == nil && m["ticket"] == ticketID {
				out = append(out, m)
			}
		}
		return out
	}
}

// incidentCapture makes the pipeline's mctl-api client record the incident
// POST bodies it sends.
func incidentCapture(t *testing.T, p *Pipeline) func() []map[string]any {
	t.Helper()
	var mu sync.Mutex
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/api/v1/incidents" {
			raw, _ := io.ReadAll(r.Body)
			var m map[string]any
			_ = json.Unmarshal(raw, &m)
			mu.Lock()
			bodies = append(bodies, m)
			mu.Unlock()
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)
	p.apiClient = mctlclient.NewClient(srv.URL, "test-token")
	return func() []map[string]any {
		mu.Lock()
		defer mu.Unlock()
		return append([]map[string]any(nil), bodies...)
	}
}

// A sampled run stamps the root trace on every pipeline log line, on the
// stored ticket and on the incident published to mctl-api, and stores no
// span id as ticket identity.
//
// Mutation check: drop the log.With(LogAttrs) binding, the SetTraceID call or
// the trace_id key in PublishAlert, and this fails.
func TestProcessTicketCorrelatesLogsTicketAndIncident(t *testing.T) {
	logsFor := captureLogs(t)
	p, store := newAsyncTestPipeline(t, 1)
	incidents := incidentCapture(t, p)
	spans, tk := processTraced(t, p, store)

	root, ok := spans["mctl_agent.process_ticket"]
	if !ok {
		t.Fatal("no root span")
	}
	wantTrace := root.SpanContext().TraceID().String()
	wantSpan := root.SpanContext().SpanID().String()

	lines := logsFor(tk.ID)
	if len(lines) == 0 {
		t.Fatal("the pipeline logged nothing for the ticket")
	}
	for _, l := range lines {
		if l[telemetry.LogKeyTraceID] != wantTrace || l[telemetry.LogKeySpanID] != wantSpan {
			t.Errorf("log line %q: trace_id=%v span_id=%v, want %s/%s", l["msg"], l[telemetry.LogKeyTraceID], l[telemetry.LogKeySpanID], wantTrace, wantSpan)
		}
	}

	stored, err := store.Get(context.Background(), tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.TraceID != wantTrace {
		t.Errorf("stored trace_id = %q, want %q", stored.TraceID, wantTrace)
	}

	sent := incidents()
	if len(sent) == 0 {
		t.Fatal("no incident published")
	}
	if sent[0]["trace_id"] != wantTrace {
		t.Errorf("published trace_id = %v, want %s", sent[0]["trace_id"], wantTrace)
	}
	if _, ok := sent[0]["span_id"]; ok {
		t.Error("span_id must not be published as incident identity")
	}
}

// With no tracer provider (the default, and production until OTLP export is
// configured) the ticket is processed exactly as before, with no ids.
func TestProcessTicketWithTelemetryDisabled(t *testing.T) {
	if _, isSDK := otel.GetTracerProvider().(*sdktrace.TracerProvider); isSDK {
		t.Fatal("precondition: a test left an SDK provider installed")
	}
	logsFor := captureLogs(t)
	p, store := newAsyncTestPipeline(t, 1)
	incidents := incidentCapture(t, p)
	tk := &ticket.Ticket{Source: ticket.SourceAlertManager, Type: ticket.TypePodCrashloop, Tenant: "billing", Service: "api", Summary: "crashloop", Status: ticket.StatusAnalyzing}
	if err := store.Create(context.Background(), tk); err != nil {
		t.Fatal(err)
	}
	p.processTicketSync(context.Background(), tk)

	got, err := store.Get(context.Background(), tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != ticket.StatusEscalated {
		t.Errorf("status = %q, want %q", got.Status, ticket.StatusEscalated)
	}
	if got.TraceID != "" {
		t.Errorf("trace_id = %q without tracing", got.TraceID)
	}
	for _, l := range logsFor(tk.ID) {
		if _, ok := l[telemetry.LogKeyTraceID]; ok {
			t.Errorf("log line %q carries a trace_id without tracing", l["msg"])
		}
	}
	for _, b := range incidents() {
		if _, ok := b["trace_id"]; ok {
			t.Errorf("incident published with trace_id without tracing: %v", b["trace_id"])
		}
	}
}

type failingExporter struct{}

func (failingExporter) ExportSpans(context.Context, []sdktrace.ReadOnlySpan) error {
	return errors.New("collector unavailable")
}
func (failingExporter) Shutdown(context.Context) error { return nil }

// An exporter that fails every export, called synchronously on each span end
// so the failure lands inside the processing path, must not change the
// outcome. The trace id is still recorded: the span was sampled, and whether
// the backend received it is the backend's concern, not the ticket's.
func TestProcessTicketCompletesWhenTheExporterFails(t *testing.T) {
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(failingExporter{}))
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() {
		otel.SetTracerProvider(prev)
		_ = tp.Shutdown(context.Background())
	})
	p, store := newAsyncTestPipeline(t, 1)
	tk := &ticket.Ticket{Source: ticket.SourceAlertManager, Type: ticket.TypePodCrashloop, Tenant: "billing", Service: "api", Summary: "crashloop", Status: ticket.StatusAnalyzing}
	if err := store.Create(context.Background(), tk); err != nil {
		t.Fatal(err)
	}
	p.processTicketSync(context.Background(), tk)

	got, err := store.Get(context.Background(), tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != ticket.StatusEscalated {
		t.Errorf("status = %q, want %q", got.Status, ticket.StatusEscalated)
	}
	if len(got.TraceID) != 32 {
		t.Errorf("trace_id = %q", got.TraceID)
	}
}
