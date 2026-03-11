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

package yaml

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/mctlhq/mctl-agent/internal/skill"
	"github.com/mctlhq/mctl-agent/internal/ticket"
)

func TestYAMLSkillMatchAndDiagnose(t *testing.T) {
	def := SkillDef{
		Name:        "test-redis-timeout",
		Version:     "1.0",
		Description: "Test Redis timeout skill",
		Trigger: Trigger{
			AlertTypes:  []string{"pod_crashloop"},
			LogPatterns: []string{"redis.*timeout", "ETIMEDOUT"},
		},
		Diagnosis: Diag{
			Template:   "Service {{.Tenant}}/{{.Service}} has Redis timeouts.",
			Confidence: "MEDIUM",
			Fixable:    false,
		},
		Notification: &Notify{
			ExtraContext: "Run: kubectl -n {{.Tenant}} logs {{.Service}}",
		},
		Capabilities: []string{"read_logs", "send_notification"},
	}

	ys, err := NewFromDef(def)
	if err != nil {
		t.Fatal(err)
	}

	if ys.Name() != "test-redis-timeout" {
		t.Errorf("expected name test-redis-timeout, got %s", ys.Name())
	}
	if len(ys.RequiredCapabilities()) != 2 {
		t.Errorf("expected 2 capabilities, got %d", len(ys.RequiredCapabilities()))
	}

	ctx := context.Background()

	// Test Match — correct type + matching logs.
	tk := &ticket.Ticket{Type: "pod_crashloop", Tenant: "billing", Service: "payment-api"}
	ev := skill.NewEvidenceSet([]ticket.Evidence{
		{Type: "logs", Content: "ERROR: redis connection timeout after 5000ms"},
	})

	result := ys.Match(ctx, tk, ev)
	if !result.Matched {
		t.Fatal("expected match")
	}
	if result.Confidence != 0.65 {
		t.Errorf("expected confidence 0.65, got %f", result.Confidence)
	}

	// Test Match — wrong alert type.
	tk2 := &ticket.Ticket{Type: "workflow_failed"}
	result2 := ys.Match(ctx, tk2, ev)
	if result2.Matched {
		t.Error("should not match wrong alert type")
	}

	// Test Match — right type but no matching logs.
	ev2 := skill.NewEvidenceSet([]ticket.Evidence{
		{Type: "logs", Content: "INFO: server started on port 8080"},
	})
	result3 := ys.Match(ctx, tk, ev2)
	if result3.Matched {
		t.Error("should not match when logs don't contain pattern")
	}

	// Test Diagnose.
	diag, err := ys.Diagnose(ctx, tk, ev)
	if err != nil {
		t.Fatal(err)
	}
	if diag.Confidence != ticket.ConfidenceMedium {
		t.Errorf("expected MEDIUM confidence, got %s", diag.Confidence)
	}
	if diag.Fixable {
		t.Error("should not be fixable")
	}
	if !contains(diag.Diagnosis, "billing/payment-api") {
		t.Errorf("diagnosis should contain tenant/service, got: %s", diag.Diagnosis)
	}
	if !contains(diag.Diagnosis, "kubectl") {
		t.Errorf("diagnosis should contain notification extra context, got: %s", diag.Diagnosis)
	}

	// Test Fix — should fail for YAML skills.
	_, err = ys.Fix(ctx, tk, diag)
	if err == nil {
		t.Error("expected error from YAML skill Fix()")
	}
}

func TestYAMLSkillHighConfidence(t *testing.T) {
	def := SkillDef{
		Name:    "high-conf",
		Version: "1.0",
		Trigger: Trigger{},
		Diagnosis: Diag{
			Template:   "High confidence diagnosis for {{.Service}}.",
			Confidence: "HIGH",
		},
	}
	ys, err := NewFromDef(def)
	if err != nil {
		t.Fatal(err)
	}

	tk := &ticket.Ticket{Service: "test"}
	ev := skill.NewEvidenceSet(nil)
	result := ys.Match(context.Background(), tk, ev)
	if !result.Matched {
		t.Fatal("expected match with no trigger constraints")
	}
	if result.Confidence != 0.85 {
		t.Errorf("expected 0.85 confidence for HIGH, got %f", result.Confidence)
	}

	diag, err := ys.Diagnose(context.Background(), tk, ev)
	if err != nil {
		t.Fatal(err)
	}
	if diag.Confidence != ticket.ConfidenceHigh {
		t.Errorf("expected HIGH confidence in diagnosis, got %s", diag.Confidence)
	}
}

func TestLoaderLoadAll(t *testing.T) {
	// Create temp dir with YAML skills.
	dir := t.TempDir()

	yamlContent := `
name: test-skill-1
version: "1.0"
description: "Test skill"
trigger:
  log_patterns:
    - "test error"
diagnosis:
  template: "Found error in {{.Service}}"
  confidence: LOW
  fixable: false
`
	if err := os.WriteFile(filepath.Join(dir, "test1.yaml"), []byte(yamlContent), 0644); err != nil {
		t.Fatal(err)
	}

	yaml2 := `
name: test-skill-2
version: "2.0"
description: "Another test skill"
diagnosis:
  template: "Issue in {{.Service}}"
  confidence: MEDIUM
`
	if err := os.WriteFile(filepath.Join(dir, "test2.yml"), []byte(yaml2), 0644); err != nil {
		t.Fatal(err)
	}

	// Non-YAML file should be ignored.
	if err := os.WriteFile(filepath.Join(dir, "readme.txt"), []byte("ignored"), 0644); err != nil {
		t.Fatal(err)
	}

	reg := skill.NewRegistry()
	loader := NewLoader(dir, reg)

	count := loader.LoadAll()
	if count != 2 {
		t.Errorf("expected 2 skills loaded, got %d", count)
	}

	if loader.LoadedCount() != 2 {
		t.Errorf("expected loaded count 2, got %d", loader.LoadedCount())
	}

	// Skills should be in registry.
	if _, ok := reg.Get("test-skill-1"); !ok {
		t.Error("test-skill-1 not found in registry")
	}
	if _, ok := reg.Get("test-skill-2"); !ok {
		t.Error("test-skill-2 not found in registry")
	}
}

func TestLoaderNonExistentDir(t *testing.T) {
	reg := skill.NewRegistry()
	loader := NewLoader("/nonexistent/path", reg)
	count := loader.LoadAll()
	if count != 0 {
		t.Errorf("expected 0 from nonexistent dir, got %d", count)
	}
}

func TestInvalidYAMLSkill(t *testing.T) {
	dir := t.TempDir()

	// Invalid regex.
	bad := `
name: bad-skill
trigger:
  log_patterns:
    - "[invalid regex"
diagnosis:
  template: "nope"
  confidence: LOW
`
	if err := os.WriteFile(filepath.Join(dir, "bad.yaml"), []byte(bad), 0644); err != nil {
		t.Fatal(err)
	}

	reg := skill.NewRegistry()
	loader := NewLoader(dir, reg)
	count := loader.LoadAll()
	if count != 0 {
		t.Errorf("expected 0 loaded for invalid skill, got %d", count)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && containsHelper(s, sub)
}

func containsHelper(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
