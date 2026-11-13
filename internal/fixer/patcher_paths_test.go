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
	"strings"
	"testing"
)

// changedLines returns "old -> new" for every line that differs between two
// documents of equal line count, so a test can assert that a patch touched
// exactly the one line it was meant to.
func changedLines(t *testing.T, before, after string) []string {
	t.Helper()
	b, a := strings.Split(before, "\n"), strings.Split(after, "\n")
	if len(b) != len(a) {
		t.Fatalf("line count changed: %d -> %d", len(b), len(a))
	}
	var out []string
	for i := range b {
		if b[i] != a[i] {
			out = append(out, b[i]+" -> "+a[i])
		}
	}
	return out
}

// Shapes taken from real platform-gitops/services/*/values.yaml files: the
// chart-level block lists requests first, sidecars under extraContainers carry
// their own limits, and containers embedded in extraObjects nest resources
// deeper still. Only the chart-level limit may change.
const multiContainerValues = `image:
  repository: ghcr.io/mctlhq/kuptsi-app
  tag: "1.4.0"
resources:
  # Sized for the API process only; the worker sidecar has its own block.
  requests:
    cpu: 75m
    memory: 384Mi
  limits:
    # Raised after the 2026-10 OOM.
    cpu: 300m
    memory: 768Mi  # chart-level limit
extraContainers:
  - name: temporal-worker
    resources:
      requests:
        cpu: 100m
        memory: 384Mi
      limits:
        cpu: 500m
        memory: 768Mi
extraObjects:
  - apiVersion: batch/v1
    kind: CronJob
    spec:
      jobTemplate:
        spec:
          template:
            spec:
              containers:
                - name: refresh
                  resources:
                    limits:
                      cpu: 200m
                      memory: 128Mi
`

func TestGenerateMemoryBumpTouchesOnlyChartLimit(t *testing.T) {
	out, summary, err := GenerateMemoryBump(multiContainerValues)
	if err != nil {
		t.Fatal(err)
	}
	got := changedLines(t, multiContainerValues, out)
	want := []string{"    memory: 768Mi  # chart-level limit ->     memory: 1152Mi  # chart-level limit"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("changed lines:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if summary != "Bump memory limit from 768Mi to 1152Mi" {
		t.Errorf("summary = %q", summary)
	}
}

func TestGenerateCPUBumpTouchesOnlyChartLimit(t *testing.T) {
	out, summary, err := GenerateCPUBump(multiContainerValues)
	if err != nil {
		t.Fatal(err)
	}
	got := changedLines(t, multiContainerValues, out)
	if len(got) != 1 || got[0] != "    cpu: 300m ->     cpu: 450m" {
		t.Fatalf("changed lines: %q", got)
	}
	if summary != "Bump CPU limit from 300m to 450m" {
		t.Errorf("summary = %q", summary)
	}
}

// The bug report's shape: limits before requests. The old CPU patcher kept its
// "in limits" flag across the sibling requests: block and bumped both.
func TestResourceBumpLimitsBeforeRequests(t *testing.T) {
	content := `resources:
  limits:
    cpu: 1
    memory: 512Mi
  requests:
    cpu: 500m
    memory: 256Mi`

	cpuOut, _, err := GenerateCPUBump(content)
	if err != nil {
		t.Fatal(err)
	}
	if got := changedLines(t, content, cpuOut); len(got) != 1 || got[0] != "    cpu: 1 ->     cpu: 1500m" {
		t.Errorf("cpu changed lines: %q", got)
	}

	memOut, _, err := GenerateMemoryBump(content)
	if err != nil {
		t.Fatal(err)
	}
	if got := changedLines(t, content, memOut); len(got) != 1 || got[0] != "    memory: 512Mi ->     memory: 768Mi" {
		t.Errorf("memory changed lines: %q", got)
	}
}

func TestResourceBumpKeepsQuotingAndBlockComments(t *testing.T) {
	content := `resources:
  limits: # per-container
    memory: "1Gi" # keep quoted
  requests:
    memory: "512Mi"`
	out, _, err := GenerateMemoryBump(content)
	if err != nil {
		t.Fatal(err)
	}
	got := changedLines(t, content, out)
	if len(got) != 1 || got[0] != `    memory: "1Gi" # keep quoted ->     memory: "1.5Gi" # keep quoted` {
		t.Errorf("changed lines: %q", got)
	}
}

// Every case here must fail instead of patching something else.
func TestResourceBumpRefusesToGuess(t *testing.T) {
	cases := map[string]string{
		"requests only": `resources:
  requests:
    memory: 256Mi`,
		"no chart-level resources, sidecar has limits": `extraContainers:
  - name: worker
    resources:
      limits:
        memory: 256Mi`,
		"resources nested under another key": `worker:
  resources:
    limits:
      memory: 256Mi`,
		"flow-style limits": `resources:
  limits: {memory: 256Mi}`,
		"unsupported unit": `resources:
  limits:
    memory: 1G`,
		"limit is a block, not a scalar": `resources:
  limits:
    memory:
      value: 256Mi`,
		"limits only under a later root key": `resources:
  requests:
    memory: 256Mi
worker:
  limits:
    memory: 256Mi`,
		"fragment indented from the first line": `  resources:
    limits:
      memory: 256Mi`,
		"empty document": ``,
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			out, summary, err := GenerateMemoryBump(content)
			if err == nil {
				t.Fatalf("expected an error, got summary %q and:\n%s", summary, out)
			}
			if out != content {
				t.Errorf("content changed on error")
			}
		})
	}
}

func TestFindScalarLine(t *testing.T) {
	lines := strings.Split(multiContainerValues, "\n")
	cases := []struct {
		path    string
		wantErr bool
		want    string
	}{
		{path: "image.tag", want: `  tag: "1.4.0"`},
		{path: "resources.requests.memory", want: "    memory: 384Mi"},
		{path: "resources.limits.cpu", want: "    cpu: 300m"},
		{path: "resources.limits", wantErr: true}, // block, not scalar
		{path: "image.tag.minor", wantErr: true},  // tag is a scalar
		{path: "resources.limits.storage", wantErr: true},
		{path: "limits.cpu", wantErr: true}, // not at the root
		{path: "resources..cpu", wantErr: true},
	}
	for _, c := range cases {
		t.Run(c.path, func(t *testing.T) {
			idx, err := findScalarLine(lines, strings.Split(c.path, "."))
			if c.wantErr {
				if err == nil {
					t.Fatalf("expected error, got line %q", lines[idx])
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if lines[idx] != c.want {
				t.Errorf("line = %q, want %q", lines[idx], c.want)
			}
		})
	}
}

// The value lives on the diagnosed field, not wherever the same text first
// appears in the file.
func TestGenerateFromDiagnosisPatchesNamedField(t *testing.T) {
	content := `replicas: 30
probes:
  liveness:
    timeoutSeconds: 30`
	out, summary, err := GenerateFromDiagnosis(content, &DiagnosisCompat{
		YAMLField:      "probes.liveness.timeoutSeconds",
		CurrentValue:   "30",
		SuggestedValue: "60",
	})
	if err != nil {
		t.Fatal(err)
	}
	got := changedLines(t, content, out)
	if len(got) != 1 || got[0] != "    timeoutSeconds: 30 ->     timeoutSeconds: 60" {
		t.Fatalf("changed lines: %q", got)
	}
	if summary != "Update probes.liveness.timeoutSeconds from 30 to 60" {
		t.Errorf("summary = %q", summary)
	}
}

func TestGenerateFromDiagnosisRefusesMismatch(t *testing.T) {
	content := `replicas: 30
autoscaling:
  maxReplicas: 5`
	cases := map[string]*DiagnosisCompat{
		"current value differs": {YAMLField: "autoscaling.maxReplicas", CurrentValue: "30", SuggestedValue: "10"},
		"leaf-only field name":  {YAMLField: "maxReplicas", CurrentValue: "5", SuggestedValue: "10"},
		"missing current value": {YAMLField: "autoscaling.maxReplicas", SuggestedValue: "10"},
		"suggested value adds a line": {YAMLField: "autoscaling.maxReplicas", CurrentValue: "5",
			SuggestedValue: "10\nhostNetwork: true"},
		"suggested value carriage return":  {YAMLField: "autoscaling.maxReplicas", CurrentValue: "5", SuggestedValue: "10\rx"},
		"suggested value nests a mapping":  {YAMLField: "autoscaling.maxReplicas", CurrentValue: "5", SuggestedValue: "a: b"},
		"suggested value starts a comment": {YAMLField: "autoscaling.maxReplicas", CurrentValue: "5", SuggestedValue: "10 # x"},
		"suggested value is a flow map":    {YAMLField: "autoscaling.maxReplicas", CurrentValue: "5", SuggestedValue: "{a: 1}"},
		"suggested value is an alias":      {YAMLField: "autoscaling.maxReplicas", CurrentValue: "5", SuggestedValue: "*ref"},
		"suggested value has padding":      {YAMLField: "autoscaling.maxReplicas", CurrentValue: "5", SuggestedValue: `" 10"`},
		"suggested value has NEL":          {YAMLField: "autoscaling.maxReplicas", CurrentValue: "5", SuggestedValue: "10\u0085hostNetwork:\u0085true"},
		"suggested value has LS":           {YAMLField: "autoscaling.maxReplicas", CurrentValue: "5", SuggestedValue: "10 hostNetwork: true"},
		"suggested value has PS":           {YAMLField: "autoscaling.maxReplicas", CurrentValue: "5", SuggestedValue: "10 x"},
		"suggested value equals current":   {YAMLField: "autoscaling.maxReplicas", CurrentValue: "5", SuggestedValue: `"5"`},
	}
	for name, d := range cases {
		t.Run(name, func(t *testing.T) {
			out, _, err := GenerateFromDiagnosis(content, d)
			if err == nil {
				t.Fatalf("expected an error, got:\n%s", out)
			}
			if out != content {
				t.Errorf("content changed on error")
			}
		})
	}
}

func TestGenerateFromDiagnosisQuotedValue(t *testing.T) {
	content := `image:
  tag: "1.4.0"`
	ok := &DiagnosisCompat{YAMLField: "image.tag", CurrentValue: "1.4.0", SuggestedValue: "1.4.1"}
	out, _, err := GenerateFromDiagnosis(content, ok)
	if err != nil {
		t.Fatal(err)
	}
	if got := changedLines(t, content, out); len(got) != 1 || got[0] != `  tag: "1.4.0" ->   tag: "1.4.1"` {
		t.Fatalf("changed lines: %q", got)
	}
	// A quote inside the value would close the scalar early.
	bad := &DiagnosisCompat{YAMLField: "image.tag", CurrentValue: "1.4.0", SuggestedValue: `1.4.1" # x`}
	if out, _, err := GenerateFromDiagnosis(content, bad); err == nil {
		t.Fatalf("expected an error, got:\n%s", out)
	}
}

func TestGenerateWorkflowParamFixOnlyRewritesDefaultKeys(t *testing.T) {
	content := `spec:
  arguments:
    parameters:
      - name: tenant
        default: ""
      - default: main
        name: branch
      - name: region
        default:
          eu
  metadata:
    isdefault: y
    x-default: z
    notes:
      - default:plain-scalar`
	out, _, err := GenerateWorkflowParamFix(content)
	if err != nil {
		t.Fatal(err)
	}
	got := changedLines(t, content, out)
	want := []string{
		`        default: "" ->         value: ""`,
		"      - default: main ->       - value: main",
		"        default: ->         value:",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("changed lines:\n%s", strings.Join(got, "\n"))
	}

	if _, _, err := GenerateWorkflowParamFix("isdefault: y\n"); err == nil {
		t.Error("expected an error when no default: key exists")
	}

	crlf := "- name: a\r\n  default:\r\n    x\r\n"
	out, _, err = GenerateWorkflowParamFix(crlf)
	if err != nil || out != "- name: a\r\n  value:\r\n    x\r\n" {
		t.Errorf("CRLF: out=%q err=%v", out, err)
	}
}

// The bump paths must refuse a value they cannot raise rather than return an
// unchanged document as a successful patch.
func TestResourceBumpRefusesNoOp(t *testing.T) {
	content := `resources:
  limits:
    memory: 0Mi`
	if out, s, err := GenerateMemoryBump(content); err == nil {
		t.Fatalf("expected an error, got %q:\n%s", s, out)
	}
}
