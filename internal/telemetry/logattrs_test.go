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

package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"

	"go.opentelemetry.io/otel/trace"
)

var (
	testTraceID = trace.TraceID{0x16, 0x4a, 0xc9, 0x7d, 0x8d, 0x72, 0x64, 0x32, 0x6c, 0x40, 0x62, 0x40, 0xd2, 0x63, 0xdc, 0xc1}
	testSpanID  = trace.SpanID{0x8e, 0x6d, 0x7a, 0x44, 0xb1, 0x73, 0x5a, 0x35}
)

func ctxWith(flags trace.TraceFlags) context.Context {
	sc := trace.NewSpanContext(trace.SpanContextConfig{TraceID: testTraceID, SpanID: testSpanID, TraceFlags: flags})
	return trace.ContextWithSpanContext(context.Background(), sc)
}

// logLine renders one JSON log record with LogAttrs(ctx) bound, the way the
// pipeline binds them, and returns its fields.
func logLine(t *testing.T, ctx context.Context) map[string]any {
	t.Helper()
	var buf bytes.Buffer
	slog.New(slog.NewJSONHandler(&buf, nil)).With(LogAttrs(ctx)...).Info("x")
	var m map[string]any
	if err := json.Unmarshal(buf.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestLogAttrsOnASampledSpan(t *testing.T) {
	ctx := ctxWith(trace.FlagsSampled)
	m := logLine(t, ctx)
	if m[LogKeyTraceID] != "164ac97d8d7264326c406240d263dcc1" || m[LogKeySpanID] != "8e6d7a44b1735a35" {
		t.Fatalf("fields = %v", m)
	}
	if got := TraceID(ctx); got != "164ac97d8d7264326c406240d263dcc1" {
		t.Fatalf("TraceID = %q", got)
	}
}

// Mutation check: drop the IsSampled guard from LogAttrs or TraceID and this
// fails, because the span context below is valid, only unsampled.
func TestLogAttrsOnAnUnsampledSpan(t *testing.T) {
	ctx := ctxWith(0)
	if !trace.SpanContextFromContext(ctx).IsValid() {
		t.Fatal("fixture must be a valid span context, or this tests nothing")
	}
	m := logLine(t, ctx)
	if _, ok := m[LogKeyTraceID]; ok {
		t.Errorf("unsampled span logged trace_id: %v", m)
	}
	if _, ok := m[LogKeySpanID]; ok {
		t.Errorf("unsampled span logged span_id: %v", m)
	}
	if got := TraceID(ctx); got != "" {
		t.Errorf("TraceID on an unsampled span = %q", got)
	}
}

func TestLogAttrsWithoutASpan(t *testing.T) {
	for name, ctx := range map[string]context.Context{
		"no span": context.Background(),
		"invalid": trace.ContextWithSpanContext(context.Background(), trace.NewSpanContext(trace.SpanContextConfig{TraceFlags: trace.FlagsSampled})),
		// What Tracer().Start returns when no provider is installed.
		"no-op tracer": func() context.Context { ctx, _ := Tracer().Start(context.Background(), "x"); return ctx }(),
	} {
		t.Run(name, func(t *testing.T) {
			if got := LogAttrs(ctx); got != nil {
				t.Errorf("LogAttrs = %v", got)
			}
			if got := TraceID(ctx); got != "" {
				t.Errorf("TraceID = %q", got)
			}
		})
	}
}
