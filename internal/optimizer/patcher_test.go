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

package optimizer

import (
	"strings"
	"testing"
)

// kuptsiFixture mirrors the load-bearing shape of
// services/labs/kuptsi-app/values.yaml: incident-history comments adjacent
// to the resources block and a sidecar with its own resources under
// extraContainers. Everything outside the two top-level request lines must
// survive byte-identical.
const kuptsiFixture = `# Service: kuptsi-app
# Team: labs

image:
  repository: ghcr.io/mctlhq/kuptsi-app
  tag: "0.2.17"
service:
  port: 5000
resources:
  requests:
    cpu: 200m
    memory: 256Mi
  limits:
    cpu: 500m
    memory: 512Mi
# NOTE: do NOT let mctl_deploy_service stomp this block on tag bump — the
# uvicorn entrypoint needs HOST/PORT/WEB_CONCURRENCY or it silently exits 0.
env:
  APP_ENV: production
  HOST: 0.0.0.0
extraContainers:
  - name: temporal-worker
    image: ghcr.io/mctlhq/kuptsi-app:0.2.17
    resources:
      requests:
        cpu: 100m
        memory: 256Mi
      limits:
        cpu: 500m
        memory: 512Mi
ingress:
  enabled: true
`

func TestGenerateRequestsPatchKuptsiStyle(t *testing.T) {
	patched, summary, err := GenerateRequestsPatch(kuptsiFixture, "80m", "224Mi")
	if err != nil {
		t.Fatalf("GenerateRequestsPatch: %v", err)
	}

	if !strings.Contains(summary, "cpu request 200m -> 80m") ||
		!strings.Contains(summary, "memory request 256Mi -> 224Mi") {
		t.Errorf("summary = %q", summary)
	}

	// Exactly the two top-level request lines changed, byte-identical elsewhere.
	origLines := strings.Split(kuptsiFixture, "\n")
	newLines := strings.Split(patched, "\n")
	if len(origLines) != len(newLines) {
		t.Fatalf("line count changed: %d -> %d", len(origLines), len(newLines))
	}
	var diffs []int
	for i := range origLines {
		if origLines[i] != newLines[i] {
			diffs = append(diffs, i)
		}
	}
	if len(diffs) != 2 {
		t.Fatalf("changed lines = %v, want exactly 2", diffs)
	}
	if newLines[diffs[0]] != "    cpu: 80m" {
		t.Errorf("cpu line = %q", newLines[diffs[0]])
	}
	if newLines[diffs[1]] != "    memory: 224Mi" {
		t.Errorf("memory line = %q", newLines[diffs[1]])
	}

	// Sidecar resources untouched.
	if !strings.Contains(patched, "        cpu: 100m") {
		t.Error("sidecar cpu request modified")
	}
	// Top-level limits untouched.
	if !strings.Contains(patched, "  limits:\n    cpu: 500m\n    memory: 512Mi") {
		t.Error("top-level limits modified")
	}
	// Incident comments intact.
	if !strings.Contains(patched, "do NOT let mctl_deploy_service stomp this block") {
		t.Error("load-bearing comment lost")
	}
}

func TestGenerateRequestsPatchInlineComment(t *testing.T) {
	in := "resources:\n  requests:\n    cpu: 200m # sized 2026-01 after incident\n    memory: 256Mi\n"
	patched, _, err := GenerateRequestsPatch(in, "100m", "")
	if err != nil {
		t.Fatalf("GenerateRequestsPatch: %v", err)
	}
	if !strings.Contains(patched, "cpu: 100m # sized 2026-01 after incident") {
		t.Errorf("inline comment not preserved:\n%s", patched)
	}
	if !strings.Contains(patched, "memory: 256Mi") {
		t.Errorf("memory changed unexpectedly:\n%s", patched)
	}
}

func TestGenerateRequestsPatchCPUOnly(t *testing.T) {
	patched, summary, err := GenerateRequestsPatch(kuptsiFixture, "80m", "")
	if err != nil {
		t.Fatalf("GenerateRequestsPatch: %v", err)
	}
	if strings.Contains(summary, "memory") {
		t.Errorf("summary mentions memory: %q", summary)
	}
	if !strings.Contains(patched, "    memory: 256Mi") {
		t.Error("memory request modified on cpu-only patch")
	}
}

func TestGenerateRequestsPatchErrors(t *testing.T) {
	if _, _, err := GenerateRequestsPatch("image:\n  tag: \"1.0\"\n", "100m", "128Mi"); err == nil {
		t.Error("expected error for missing resources block")
	}
	// Same values → nothing to change.
	if _, _, err := GenerateRequestsPatch(kuptsiFixture, "200m", "256Mi"); err == nil {
		t.Error("expected error when values already match")
	}
}
