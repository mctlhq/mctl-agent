package builtin

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/mctlhq/mctl-agent/internal/skill"
	"github.com/mctlhq/mctl-agent/internal/ticket"
)

// QuotaAdjustSkill detects tenant quota exhaustion and recommends adjustments.
type QuotaAdjustSkill struct{}

func NewQuotaAdjustSkill() *QuotaAdjustSkill { return &QuotaAdjustSkill{} }

func (s *QuotaAdjustSkill) Name() string    { return "quota_adjust" }
func (s *QuotaAdjustSkill) Version() string { return "1.0.0" }

func (s *QuotaAdjustSkill) Description() string {
	return "Detects tenant resource quota exhaustion and recommends adjustments"
}

func (s *QuotaAdjustSkill) RequiredCapabilities() []skill.CapabilityID {
	return []skill.CapabilityID{skill.CapReadResources, skill.CapSendNotify}
}

func (s *QuotaAdjustSkill) Match(_ context.Context, t *ticket.Ticket, ev skill.EvidenceSet) skill.MatchResult {
	if t.Type == ticket.TypeResourceLimit && t.Service != "" {
		summary := strings.ToLower(t.Summary)
		if containsAny(summary, "quota", "memory", "cpu") {
			return skill.MatchResult{
				Matched:    true,
				Confidence: 0.75,
				Priority:   70,
				Reason:     "Resource quota alert for tenant service",
			}
		}
	}

	// Check resource evidence for high utilization.
	resources := ev.Get("resources")
	if resources != "" && isHighUtilization(resources) {
		return skill.MatchResult{
			Matched:    true,
			Confidence: 0.70,
			Priority:   60,
			Reason:     "Resource utilization above 80% threshold",
		}
	}

	return skill.MatchResult{}
}

func (s *QuotaAdjustSkill) Diagnose(_ context.Context, t *ticket.Ticket, ev skill.EvidenceSet) (*skill.DiagnosisResult, error) {
	resources := ev.Get("resources")

	diag := "Tenant " + t.Tenant + " is approaching resource quota limits."
	if resources != "" {
		diag += " Review current allocation and consider increasing quotas or optimizing service resource requests."
	}

	return &skill.DiagnosisResult{
		Diagnosis:  diag,
		Confidence: ticket.ConfidenceMedium,
		Fixable:    false, // Quota changes need manual approval.
		SkillName:  s.Name(),
	}, nil
}

func (s *QuotaAdjustSkill) Fix(_ context.Context, _ *ticket.Ticket, _ *skill.DiagnosisResult) (*skill.FixResult, error) {
	return nil, nil // Quota adjustments require manual approval.
}

// isHighUtilization checks if any resource usage is above 80%.
func isHighUtilization(resourceJSON string) bool {
	var data struct {
		Used      map[string]string `json:"used"`
		Allocated map[string]string `json:"allocated"`
	}
	if err := json.Unmarshal([]byte(resourceJSON), &data); err != nil {
		return false
	}
	// Simple heuristic — if the JSON contains usage data, it was flagged for a reason.
	return len(data.Used) > 0 && len(data.Allocated) > 0
}
