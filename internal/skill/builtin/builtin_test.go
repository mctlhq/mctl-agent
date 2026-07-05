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
	"testing"

	"github.com/mctlhq/mctl-agent/internal/skill"
	"github.com/mctlhq/mctl-agent/internal/ticket"
)

func TestOOMKilledSkillMatch(t *testing.T) {
	s := NewOOMKilledSkill()
	ctx := context.Background()
	tk := &ticket.Ticket{Type: ticket.TypePodCrashloop}

	tests := []struct {
		name    string
		logs    string
		matched bool
	}{
		{"OOMKilled in logs", `{"lines":[{"line":"OOMKilled"}]}`, true},
		{"oom-kill in logs", `container oom-kill event`, true},
		{"Out of memory", `Out of memory: Kill process`, true},
		{"no OOM", `normal log output`, false},
		{"empty evidence", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ev := skill.NewEvidenceSet([]ticket.Evidence{
				{Type: "logs", Content: tt.logs},
			})
			result := s.Match(ctx, tk, ev)
			if result.Matched != tt.matched {
				t.Errorf("Match() = %v, want %v", result.Matched, tt.matched)
			}
			if tt.matched && result.Confidence < 0.9 {
				t.Errorf("expected high confidence, got %f", result.Confidence)
			}
		})
	}
}

func TestOOMKilledSkillDiagnose(t *testing.T) {
	s := NewOOMKilledSkill()
	diag, err := s.Diagnose(context.Background(), &ticket.Ticket{}, skill.NewEvidenceSet(nil))
	if err != nil {
		t.Fatal(err)
	}
	if diag.Confidence != ticket.ConfidenceHigh {
		t.Errorf("expected HIGH confidence, got %s", diag.Confidence)
	}
	if !diag.Fixable {
		t.Error("expected fixable=true")
	}
	if diag.FixType != "bump_memory" {
		t.Errorf("expected bump_memory fix type, got %s", diag.FixType)
	}
}

func TestImagePullBackOffSkillMatch(t *testing.T) {
	s := NewImagePullBackOffSkill()
	ctx := context.Background()
	tk := &ticket.Ticket{}

	tests := []struct {
		name    string
		logs    string
		matched bool
	}{
		{"ImagePullBackOff", `ImagePullBackOff for image`, true},
		{"ErrImagePull", `ErrImagePull: unauthorized`, true},
		{"no image error", `healthy pod output`, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ev := skill.NewEvidenceSet([]ticket.Evidence{
				{Type: "logs", Content: tt.logs},
			})
			result := s.Match(ctx, tk, ev)
			if result.Matched != tt.matched {
				t.Errorf("Match() = %v, want %v", result.Matched, tt.matched)
			}
		})
	}
}

func TestImagePullBackOffSkillDiagnose(t *testing.T) {
	s := NewImagePullBackOffSkill()
	diag, err := s.Diagnose(context.Background(), &ticket.Ticket{}, skill.NewEvidenceSet(nil))
	if err != nil {
		t.Fatal(err)
	}
	if diag.Fixable {
		t.Error("expected fixable=false for image pull issues")
	}
}

func TestPostDeployRollbackSkillMatch(t *testing.T) {
	s := NewPostDeployRollbackSkill()
	ctx := context.Background()

	// Non-crashloop ticket should not match.
	tk := &ticket.Ticket{Type: ticket.TypeResourceLimit, Tenant: "team-a", Service: "app-1"}
	ev := skill.NewEvidenceSet(nil)
	result := s.Match(ctx, tk, ev)
	if result.Matched {
		t.Error("should not match non-crashloop ticket")
	}

	// Crashloop ticket without audit → no match.
	tk.Type = ticket.TypePodCrashloop
	result = s.Match(ctx, tk, ev)
	if result.Matched {
		t.Error("should not match without audit evidence")
	}
}

func TestArgoCDDriftSkillMatch(t *testing.T) {
	s := NewArgoCDDriftSkill()
	ctx := context.Background()
	tk := &ticket.Ticket{}

	tests := []struct {
		name    string
		status  string
		matched bool
	}{
		{
			"OutOfSync + Healthy",
			`{"syncStatus":"OutOfSync","health":"Healthy"}`,
			true,
		},
		{
			"Synced + Healthy",
			`{"syncStatus":"Synced","health":"Healthy"}`,
			false,
		},
		{
			"OutOfSync + Degraded",
			`{"syncStatus":"OutOfSync","health":"Degraded"}`,
			false,
		},
		{"no status", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ev := skill.NewEvidenceSet([]ticket.Evidence{
				{Type: "argocd_status", Content: tt.status},
			})
			result := s.Match(ctx, tk, ev)
			if result.Matched != tt.matched {
				t.Errorf("Match() = %v, want %v", result.Matched, tt.matched)
			}
		})
	}
}

func TestArgoCDSyncFailedSkillMatch(t *testing.T) {
	s := NewArgoCDSyncFailedSkill()
	ctx := context.Background()
	tk := &ticket.Ticket{}

	tests := []struct {
		name             string
		status           string
		wantMatched      bool
		wantMinPriority  int
		wantHighConfDiag bool
	}{
		{
			name:        "OutOfSync + Healthy (drift, leave to ArgoCDDriftSkill)",
			status:      `{"syncStatus":"OutOfSync","health":"Healthy"}`,
			wantMatched: false,
		},
		{
			name:        "Synced + Healthy (nothing to do)",
			status:      `{"syncStatus":"Synced","health":"Healthy"}`,
			wantMatched: false,
		},
		{
			name:            "OutOfSync + Degraded (sync stuck)",
			status:          `{"syncStatus":"OutOfSync","health":"Degraded"}`,
			wantMatched:     true,
			wantMinPriority: 60,
		},
		{
			name:             "Degraded with storedVersion conflict signature",
			status:           `{"syncStatus":"OutOfSync","health":"Degraded","message":"CustomResourceDefinition externalsecrets is invalid: status.storedVersions[1]: Invalid value: \"v1\": missing from spec.versions"}`,
			wantMatched:      true,
			wantMinPriority:  80,
			wantHighConfDiag: true,
		},
		{
			name:             "Degraded with invalid group/version signature",
			status:           `{"syncStatus":"OutOfSync","health":"Degraded","message":"request to convert CR from an invalid group/version: external-secrets.io/v1"}`,
			wantMatched:      true,
			wantMinPriority:  80,
			wantHighConfDiag: true,
		},
		{
			name:            "Degraded only (no sync info)",
			status:          `{"health":"Degraded"}`,
			wantMatched:     true,
			wantMinPriority: 55,
		},
		{name: "no status", status: "", wantMatched: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ev := skill.NewEvidenceSet([]ticket.Evidence{
				{Type: "argocd_status", Content: tt.status},
			})
			result := s.Match(ctx, tk, ev)
			if result.Matched != tt.wantMatched {
				t.Fatalf("Match() = %v, want %v (reason=%q)", result.Matched, tt.wantMatched, result.Reason)
			}
			if !tt.wantMatched {
				return
			}
			if result.Priority < tt.wantMinPriority {
				t.Errorf("Match() priority = %d, want >= %d", result.Priority, tt.wantMinPriority)
			}
			diag, err := s.Diagnose(ctx, tk, ev)
			if err != nil {
				t.Fatalf("Diagnose() error = %v", err)
			}
			if diag.SkillName != s.Name() {
				t.Errorf("Diagnose() skill name = %q, want %q", diag.SkillName, s.Name())
			}
			if diag.Fixable {
				t.Error("Diagnose() should not advertise auto-fixable for cluster-side recovery")
			}
			if tt.wantHighConfDiag && diag.Confidence != ticket.ConfidenceHigh {
				t.Errorf("Diagnose() confidence = %q, want HIGH", diag.Confidence)
			}
			fix, err := s.Fix(ctx, tk, diag)
			if err != nil || fix != nil {
				t.Errorf("Fix() = (%v, %v), want (nil, nil)", fix, err)
			}
		})
	}
}

func TestLLMDiagnosisSkillMatchNoKey(t *testing.T) {
	s := NewLLMDiagnosisSkill("")
	result := s.Match(context.Background(), &ticket.Ticket{}, skill.NewEvidenceSet(nil))
	if result.Matched {
		t.Error("should not match without API key")
	}
}

func TestLLMDiagnosisSkillMatchWithKey(t *testing.T) {
	s := NewLLMDiagnosisSkill("sk-test")
	result := s.Match(context.Background(), &ticket.Ticket{}, skill.NewEvidenceSet(nil))
	if !result.Matched {
		t.Error("should match with API key as fallback")
	}
	if result.Confidence > 0.6 {
		t.Errorf("fallback confidence should be low, got %f", result.Confidence)
	}
}

func TestDetectFilePath(t *testing.T) {
	tests := []struct {
		tenant, service, want string
	}{
		{"billing", "payment-api", "platform-gitops/services/billing/payment-api/values.yaml"},
		{"", "mctl-api", "platform-gitops/apps/templates/mctl-api.yaml"},
		{"", "mctl-agent", "platform-gitops/apps/templates/mctl-agent.yaml"},
	}
	for _, tt := range tests {
		got := detectFilePath(tt.tenant, tt.service)
		if got != tt.want {
			t.Errorf("detectFilePath(%q, %q) = %q, want %q", tt.tenant, tt.service, got, tt.want)
		}
	}
}

func TestAllSkillsRegistered(t *testing.T) {
	reg := skill.NewRegistry()
	RegisterAll(reg, "test-key")

	infos := reg.List()
	if len(infos) != 12 {
		t.Fatalf("expected 12 skills, got %d", len(infos))
	}

	names := map[string]bool{}
	for _, info := range infos {
		names[info.Name] = true
		if info.Version == "" {
			t.Errorf("skill %s has empty version", info.Name)
		}
		if info.Description == "" {
			t.Errorf("skill %s has empty description", info.Name)
		}
	}

	expected := []string{
		"oomkilled", "imagepull", "post_deploy_rollback", "argocd_drift",
		"argocd_sync_failed", "llm_diagnosis", "probe_fix", "cpu_throttle",
		"quota_adjust", "scale_up", "github_actions", "workflow_fixer",
	}
	for _, name := range expected {
		if !names[name] {
			t.Errorf("missing skill: %s", name)
		}
	}
}

func TestProbeFixSkillMatch(t *testing.T) {
	s := NewProbeFixSkill()
	ctx := context.Background()

	tests := []struct {
		name    string
		logs    string
		matched bool
	}{
		{"liveness probe failed", "Liveness probe failed: connection refused", true},
		{"readiness probe failed", "Readiness probe failed: HTTP 503", true},
		{"generic probe failed", "probe failed with status 500", true},
		{"no probe issue", "normal pod running fine", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ev := skill.NewEvidenceSet([]ticket.Evidence{
				{Type: "logs", Content: tt.logs},
			})
			result := s.Match(ctx, &ticket.Ticket{}, ev)
			if result.Matched != tt.matched {
				t.Errorf("Match() = %v, want %v", result.Matched, tt.matched)
			}
		})
	}
}

func TestCPUThrottleSkillMatch(t *testing.T) {
	s := NewCPUThrottleSkill()
	ctx := context.Background()

	// Match on resource_limit ticket with CPU in summary.
	tk := &ticket.Ticket{Type: ticket.TypeResourceLimit, Summary: "TenantCPUQuotaHigh in billing"}
	result := s.Match(ctx, tk, skill.NewEvidenceSet(nil))
	if !result.Matched {
		t.Error("should match resource_limit + CPU summary")
	}

	// Match on throttling in logs.
	tk2 := &ticket.Ticket{Type: ticket.TypePodCrashloop}
	ev := skill.NewEvidenceSet([]ticket.Evidence{
		{Type: "logs", Content: "CPUThrottlingHigh detected on pod"},
	})
	result = s.Match(ctx, tk2, ev)
	if !result.Matched {
		t.Error("should match CPUThrottlingHigh in logs")
	}

	// No match on unrelated ticket.
	tk3 := &ticket.Ticket{Type: ticket.TypePodCrashloop, Summary: "random crash"}
	result = s.Match(ctx, tk3, skill.NewEvidenceSet(nil))
	if result.Matched {
		t.Error("should not match unrelated ticket")
	}
}

func TestQuotaAdjustSkillMatch(t *testing.T) {
	s := NewQuotaAdjustSkill()
	ctx := context.Background()

	// Match on resource_limit with service and quota in summary.
	tk := &ticket.Ticket{Type: ticket.TypeResourceLimit, Service: "app-1", Summary: "Memory quota high"}
	result := s.Match(ctx, tk, skill.NewEvidenceSet(nil))
	if !result.Matched {
		t.Error("should match resource_limit with quota in summary")
	}

	// Should not auto-fix.
	diag, err := s.Diagnose(ctx, tk, skill.NewEvidenceSet(nil))
	if err != nil {
		t.Fatal(err)
	}
	if diag.Fixable {
		t.Error("quota adjustments should not be auto-fixable")
	}
}

// TestQuotaAdjustSkillIgnoresNonResourceLimitTickets guards the production
// bug where quota_adjust matched ANY ticket type (ArgoCD-degraded, generic,
// ...) whenever "resources" evidence was present, because (a) the fallback
// branch had no Type gate and (b) isHighUtilization only checked that the
// used/allocated maps were non-empty rather than computing a real ratio.
// collectEvidence attaches "resources" evidence to every ticket regardless
// of type, so this made quota_adjust win the ranked-skill sort (confidence
// 0.70) over the real diagnosis for unrelated incidents, stamping them with
// a misleading "approaching resource quota limits" analysis.
func TestQuotaAdjustSkillIgnoresNonResourceLimitTickets(t *testing.T) {
	s := NewQuotaAdjustSkill()
	ctx := context.Background()

	highUtilEvidence := skill.NewEvidenceSet([]ticket.Evidence{
		{Type: "resources", Content: `{"used":{"cpu":"1800m"},"allocated":{"cpu":"2"}}`},
	})

	for _, tk := range []*ticket.Ticket{
		{Type: ticket.TypeArgoCDDegraded, Tenant: "admins", Service: "admins-mctl-agent", Summary: "ArgoCD OutOfSync"},
		{Type: ticket.TypeGeneric, Tenant: "monitoring", Service: "", Summary: "Vmagent has scrape_pool with 0 targets"},
		{Type: ticket.TypeGeneric, Tenant: "labs", Service: "labs-mctl-telegram-base-service", Summary: "no tool invocations for 15 minutes"},
	} {
		result := s.Match(ctx, tk, highUtilEvidence)
		if result.Matched {
			t.Errorf("type=%s: quota_adjust must not match a non-resource-limit ticket even with high-utilization evidence, got %+v", tk.Type, result)
		}
	}
}

// TestQuotaAdjustSkillMatchesServiceLessTenantQuotaAlert guards a regression
// caught by Codex review on the first version of this fix: the shared Type
// gate must not also require Service != "", because tenant-wide quota
// alerts (TenantCPUQuotaHigh / TenantMemoryQuotaHigh use `sum by (namespace)`
// with no pod label) are legitimately service-less and rely on the
// evidence-based fallback to be diagnosed at all.
func TestQuotaAdjustSkillMatchesServiceLessTenantQuotaAlert(t *testing.T) {
	s := NewQuotaAdjustSkill()
	ctx := context.Background()

	tk := &ticket.Ticket{
		Type:    ticket.TypeResourceLimit,
		Tenant:  "labs",
		Service: "",
		Summary: "TenantCPUQuotaHigh: labs is at 92% of CPU quota",
	}
	highUtilEvidence := skill.NewEvidenceSet([]ticket.Evidence{
		{Type: "resources", Content: `{"used":{"cpu":"1900m"},"allocated":{"cpu":"2"}}`},
	})

	result := s.Match(ctx, tk, highUtilEvidence)
	if !result.Matched {
		t.Error("must match a service-less TypeResourceLimit ticket via the evidence-based fallback")
	}
}

func TestQuotaAdjustSkillFallbackRequiresRealThreshold(t *testing.T) {
	s := NewQuotaAdjustSkill()
	ctx := context.Background()
	tk := &ticket.Ticket{Type: ticket.TypeResourceLimit, Service: "app-1", Summary: "resource pressure detected"}

	low := skill.NewEvidenceSet([]ticket.Evidence{
		{Type: "resources", Content: `{"used":{"cpu":"200m","memory":"128Mi"},"allocated":{"cpu":"2","memory":"1Gi"}}`},
	})
	if result := s.Match(ctx, tk, low); result.Matched {
		t.Errorf("must not match at 10%% cpu / 12%% memory utilization, got %+v", result)
	}

	high := skill.NewEvidenceSet([]ticket.Evidence{
		{Type: "resources", Content: `{"used":{"cpu":"200m","memory":"900Mi"},"allocated":{"cpu":"2","memory":"1Gi"}}`},
	})
	if result := s.Match(ctx, tk, high); !result.Matched {
		t.Error("must match when memory utilization is 90% (>= 80% threshold)")
	}
}

func TestIsHighUtilization(t *testing.T) {
	tests := []struct {
		name string
		json string
		want bool
	}{
		{"below threshold", `{"used":{"cpu":"500m"},"allocated":{"cpu":"2"}}`, false},
		{"at threshold", `{"used":{"cpu":"1600m"},"allocated":{"cpu":"2"}}`, true},
		{"binary suffix above threshold", `{"used":{"memory":"3800Mi"},"allocated":{"memory":"4Gi"}}`, true},
		{"malformed json", `not-json`, false},
		{"empty maps", `{"used":{},"allocated":{}}`, false},
		{"key missing from allocated", `{"used":{"cpu":"1900m"},"allocated":{"memory":"1Gi"}}`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isHighUtilization(tt.json); got != tt.want {
				t.Errorf("isHighUtilization(%s) = %v, want %v", tt.json, got, tt.want)
			}
		})
	}
}

func TestAutoMergeSafe(t *testing.T) {
	tests := []struct {
		name  string
		skill skill.Skill
		safe  bool
	}{
		{"oomkilled", NewOOMKilledSkill(), true},
		{"cpu_throttle", NewCPUThrottleSkill(), true},
		{"probe_fix", NewProbeFixSkill(), true},
		{"scale_up", NewScaleUpSkill(), false},
		{"post_deploy_rollback", NewPostDeployRollbackSkill(), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			am, ok := tt.skill.(skill.AutoMerger)
			if tt.safe {
				if !ok {
					t.Fatalf("expected %s to implement AutoMerger", tt.name)
				}
				if !am.AutoMergeSafe() {
					t.Errorf("expected AutoMergeSafe()=true for %s", tt.name)
				}
			} else {
				if ok && am.AutoMergeSafe() {
					t.Errorf("expected %s NOT to be auto-merge safe", tt.name)
				}
			}
		})
	}
}

func TestScaleUpSkillMatch(t *testing.T) {
	s := NewScaleUpSkill()
	ctx := context.Background()

	tests := []struct {
		name    string
		logs    string
		status  string
		matched bool
	}{
		{"max replicas reached", "max replicas reached for deployment", "", true},
		{"ScaleUpLimited", "ScaleUpLimited: desired=10, max=5", "", true},
		{"degraded + timeouts", "upstream connect error", `{"health":"Degraded"}`, true},
		{"healthy no issues", "all pods running", `{"health":"Healthy"}`, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			evs := []ticket.Evidence{{Type: "logs", Content: tt.logs}}
			if tt.status != "" {
				evs = append(evs, ticket.Evidence{Type: "argocd_status", Content: tt.status})
			}
			ev := skill.NewEvidenceSet(evs)
			result := s.Match(ctx, &ticket.Ticket{}, ev)
			if result.Matched != tt.matched {
				t.Errorf("Match() = %v, want %v", result.Matched, tt.matched)
			}
		})
	}

	// Should suggest chaining to quota_adjust.
	fix, err := s.Fix(ctx, &ticket.Ticket{Tenant: "t", Service: "s"}, &skill.DiagnosisResult{})
	if err != nil {
		t.Fatal(err)
	}
	if len(fix.NextSkills) == 0 || fix.NextSkills[0] != "quota_adjust" {
		t.Error("expected NextSkills to include quota_adjust")
	}
}
