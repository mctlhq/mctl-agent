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

// Package telemetry installs OpenTelemetry tracing for mctl-agent and names
// the span attributes it emits (mctlhq/mctl-agent#38).
//
// Spans go over OTLP to the platform OpenTelemetry Collector
// (mctlhq/mctl-gitops#902), which owns enrichment and redaction; no backend
// SDK enters this repo. Attribute names follow the platform catalog,
// mctl-docs docs/reference/telemetry-attributes.md: upstream conventions
// (gen_ai.*) where they exist, reviewed mctl.* names otherwise.
package telemetry

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"runtime/debug"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// ServiceName is the service.name resource attribute: the one resource
// attribute the catalog requires a producer to set itself.
const ServiceName = "mctl-agent"

const tracerName = "github.com/mctlhq/mctl-agent"

// Span attribute keys. Upstream gen_ai.* for the model call; mctl.* from the
// catalog's "incident agent" and "correlation identity" tables otherwise.
var (
	TicketID            = attribute.Key("mctl.ticket.id")
	TicketType          = attribute.Key("mctl.ticket.type")
	TicketOutcome       = attribute.Key("mctl.ticket.outcome")
	SkillName           = attribute.Key("mctl.skill.name")
	DiagnosisConfidence = attribute.Key("mctl.diagnosis.confidence")
	DiagnosisFixable    = attribute.Key("mctl.diagnosis.fixable")
	RepositoryName      = attribute.Key("mctl.repository.name")
	PRNumber            = attribute.Key("mctl.pr.number")

	GenAIOperationName    = attribute.Key("gen_ai.operation.name")
	GenAIProviderName     = attribute.Key("gen_ai.provider.name")
	GenAIRequestModel     = attribute.Key("gen_ai.request.model")
	GenAIUsageInputTokens = attribute.Key("gen_ai.usage.input_tokens")
	// GenAIUsageOutputTokens: the catalog notes both token counters are
	// masked by the Collector's redaction today (mctlhq/mctl-gitops#1332).
	// They are emitted under the correct names anyway; the Prometheus
	// counter in internal/metrics is the usable source until that is fixed.
	GenAIUsageOutputTokens = attribute.Key("gen_ai.usage.output_tokens")
)

// Ticket outcomes, the bounded mctl.ticket.outcome vocabulary.
const (
	OutcomePRCreated   = "pr_created"
	OutcomeFixProposed = "fix_proposed"
	OutcomeEscalated   = "escalated"
	OutcomeFailed      = "failed"
)

// Tracer is mctl-agent's tracer. Until Setup installs a provider it is the
// global no-op one, so instrumented code costs nothing when tracing is off.
func Tracer() trace.Tracer { return otel.Tracer(tracerName) }

// Enabled reports whether the environment asks for trace export: an OTLP
// endpoint is configured and the SDK is not disabled. The standard OTel
// variables are the whole interface, so deployment is configuration only.
func Enabled() bool {
	if strings.EqualFold(os.Getenv("OTEL_SDK_DISABLED"), "true") {
		return false
	}
	if strings.EqualFold(os.Getenv("OTEL_TRACES_EXPORTER"), "none") {
		return false
	}
	return os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") != "" || os.Getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT") != ""
}

// Setup installs the global tracer provider and W3C propagators when
// Enabled, and returns a shutdown function that flushes pending spans.
// When tracing is not enabled it installs nothing and the shutdown is a
// no-op: the agent must run unchanged without a collector.
func Setup(ctx context.Context) (func(context.Context) error, error) {
	noop := func(context.Context) error { return nil }
	if !Enabled() {
		slog.Info("tracing disabled: no OTLP endpoint configured")
		return noop, nil
	}
	exp, err := otlptracegrpc.New(ctx)
	if err != nil {
		return noop, err
	}
	// WithFromEnv last, so OTEL_SERVICE_NAME / OTEL_RESOURCE_ATTRIBUTES win.
	attrs := []attribute.KeyValue{attribute.String("service.name", ServiceName)}
	if v := buildRevision(); v != "" {
		attrs = append(attrs, attribute.String("service.version", v))
	}
	res, err := resource.New(ctx, resource.WithAttributes(attrs...), resource.WithFromEnv())
	if err != nil && !errors.Is(err, resource.ErrPartialResource) {
		_ = exp.Shutdown(ctx)
		return noop, err
	}
	tp := sdktrace.NewTracerProvider(sdktrace.WithBatcher(exp), sdktrace.WithResource(res))
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))
	slog.Info("tracing enabled", "exporter", "otlp")
	return tp.Shutdown, nil
}

// buildRevision is the VCS revision the binary was built from, or "" when
// the build did not stamp one. An absent service.version is honest; a
// made-up one is not.
func buildRevision() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	for _, kv := range info.Settings {
		if kv.Key == "vcs.revision" {
			return kv.Value
		}
	}
	return ""
}
