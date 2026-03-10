package builtin

import (
	"context"
	"strings"

	"github.com/mctlhq/mctl-agent/internal/skill"
	"github.com/mctlhq/mctl-agent/internal/ticket"
)

// ArgoCDDriftSkill detects ArgoCD apps that are OutOfSync but Healthy.
type ArgoCDDriftSkill struct{}

func NewArgoCDDriftSkill() *ArgoCDDriftSkill { return &ArgoCDDriftSkill{} }

func (s *ArgoCDDriftSkill) Name() string    { return "argocd_drift" }
func (s *ArgoCDDriftSkill) Version() string { return "1.0.0" }

func (s *ArgoCDDriftSkill) Description() string {
	return "Detects ArgoCD apps that are OutOfSync but Healthy (benign drift)"
}

func (s *ArgoCDDriftSkill) RequiredCapabilities() []skill.CapabilityID {
	return []skill.CapabilityID{skill.CapReadStatus, skill.CapSendNotify}
}

func (s *ArgoCDDriftSkill) Match(_ context.Context, _ *ticket.Ticket, ev skill.EvidenceSet) skill.MatchResult {
	status := ev.Get("argocd_status")
	if status == "" {
		return skill.MatchResult{}
	}

	if strings.Contains(status, `"syncStatus":"OutOfSync"`) &&
		strings.Contains(status, `"health":"Healthy"`) {
		return skill.MatchResult{
			Matched:    true,
			Confidence: 0.80,
			Priority:   50,
			Reason:     "ArgoCD app is OutOfSync but Healthy",
		}
	}
	return skill.MatchResult{}
}

func (s *ArgoCDDriftSkill) Diagnose(_ context.Context, _ *ticket.Ticket, _ skill.EvidenceSet) (*skill.DiagnosisResult, error) {
	return &skill.DiagnosisResult{
		Diagnosis:  "ArgoCD app is OutOfSync but healthy. Sync will happen automatically or may be intentional drift.",
		Confidence: ticket.ConfidenceLow,
		Fixable:    false,
		SkillName:  s.Name(),
	}, nil
}

func (s *ArgoCDDriftSkill) Fix(_ context.Context, _ *ticket.Ticket, _ *skill.DiagnosisResult) (*skill.FixResult, error) {
	return nil, nil // Not auto-fixable.
}
