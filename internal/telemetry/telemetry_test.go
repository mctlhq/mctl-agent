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
	"context"
	"testing"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

func clearOTelEnv(t *testing.T) {
	for _, k := range []string{"OTEL_SDK_DISABLED", "OTEL_TRACES_EXPORTER", "OTEL_EXPORTER_OTLP_ENDPOINT", "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT"} {
		t.Setenv(k, "")
	}
}

func TestEnabledFollowsTheStandardVariables(t *testing.T) {
	for name, tc := range map[string]struct {
		env  map[string]string
		want bool
	}{
		"nothing set":         {nil, false},
		"endpoint":            {map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://collector:4317"}, true},
		"traces endpoint":     {map[string]string{"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT": "http://collector:4317"}, true},
		"sdk disabled":        {map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://collector:4317", "OTEL_SDK_DISABLED": "TRUE"}, false},
		"traces exporter off": {map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://collector:4317", "OTEL_TRACES_EXPORTER": "none"}, false},
	} {
		t.Run(name, func(t *testing.T) {
			clearOTelEnv(t)
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			if got := Enabled(); got != tc.want {
				t.Fatalf("Enabled() = %v, want %v", got, tc.want)
			}
		})
	}
}

// Without an endpoint the agent must run exactly as before: no provider is
// installed, so every span stays the global no-op.
func TestSetupWithoutEndpointInstallsNothing(t *testing.T) {
	clearOTelEnv(t)
	shutdown, err := Setup(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, isSDK := otel.GetTracerProvider().(*sdktrace.TracerProvider); isSDK {
		t.Fatal("an SDK tracer provider was installed without an OTLP endpoint")
	}
	if err := shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// With an endpoint a real provider is installed and shuts down cleanly. The
// gRPC exporter connects lazily, so no collector is needed here.
func TestSetupWithEndpointInstallsAProvider(t *testing.T) {
	clearOTelEnv(t)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://127.0.0.1:1")
	prev := otel.GetTracerProvider()
	t.Cleanup(func() { otel.SetTracerProvider(prev) })

	shutdown, err := Setup(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, isSDK := otel.GetTracerProvider().(*sdktrace.TracerProvider); !isSDK {
		t.Fatal("no SDK tracer provider installed despite an OTLP endpoint")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // nothing to flush; an unreachable collector must not hang shutdown
	_ = shutdown(ctx)
}
