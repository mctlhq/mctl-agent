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
	if len(infos) != 5 {
		t.Fatalf("expected 5 skills, got %d", len(infos))
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

	expected := []string{"oomkilled", "imagepull", "post_deploy_rollback", "argocd_drift", "llm_diagnosis"}
	for _, name := range expected {
		if !names[name] {
			t.Errorf("missing skill: %s", name)
		}
	}
}
