package builtin

import (
	"context"
	"strings"

	"github.com/mctlhq/mctl-agent/internal/skill"
	"github.com/mctlhq/mctl-agent/internal/ticket"
)

// ProbeFixSkill detects liveness/readiness probe failures and suggests timeout adjustments.
type ProbeFixSkill struct{}

func NewProbeFixSkill() *ProbeFixSkill { return &ProbeFixSkill{} }

func (s *ProbeFixSkill) Name() string    { return "probe_fix" }
func (s *ProbeFixSkill) Version() string { return "1.0.0" }

func (s *ProbeFixSkill) Description() string {
	return "Detects liveness/readiness probe failures and suggests timeout adjustments"
}

func (s *ProbeFixSkill) RequiredCapabilities() []skill.CapabilityID {
	return []skill.CapabilityID{skill.CapReadLogs, skill.CapReadConfig, skill.CapModifyGitOps, skill.CapCreatePR}
}

func (s *ProbeFixSkill) Match(_ context.Context, _ *ticket.Ticket, ev skill.EvidenceSet) skill.MatchResult {
	logs := ev.Get("logs") + "\n" + ev.Get("alert")
	if containsAny(logs, "Liveness probe failed", "Readiness probe failed", "probe failed") {
		return skill.MatchResult{
			Matched:    true,
			Confidence: 0.80,
			Priority:   80,
			Reason:     "Health probe failure detected in logs",
		}
	}
	return skill.MatchResult{}
}

func (s *ProbeFixSkill) Diagnose(_ context.Context, _ *ticket.Ticket, ev skill.EvidenceSet) (*skill.DiagnosisResult, error) {
	logs := ev.Get("logs") + "\n" + ev.Get("alert")

	probeType := "liveness/readiness"
	if strings.Contains(logs, "Liveness probe failed") {
		probeType = "liveness"
	} else if strings.Contains(logs, "Readiness probe failed") {
		probeType = "readiness"
	}

	return &skill.DiagnosisResult{
		Diagnosis:      "Container " + probeType + " probe is failing. The application may need more startup time or the probe configuration needs adjustment.",
		Confidence:     ticket.ConfidenceMedium,
		Fixable:        true,
		FixType:        "adjust_probe",
		YAMLField:      probeType + "Probe.initialDelaySeconds",
		SuggestedValue: "30",
		SkillName:      s.Name(),
	}, nil
}

func (s *ProbeFixSkill) Fix(_ context.Context, t *ticket.Ticket, diag *skill.DiagnosisResult) (*skill.FixResult, error) {
	return &skill.FixResult{
		FilePath: detectFilePath(t.Tenant, t.Service),
		Summary:  "Increase probe initialDelaySeconds to 30s",
	}, nil
}
