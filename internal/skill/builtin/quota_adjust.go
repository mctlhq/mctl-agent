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
	"encoding/json"
	"fmt"
	"strconv"
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
	// Both branches below are quota-specific and must never fire for a
	// ticket type unrelated to resource pressure. collectEvidence attaches
	// "resources" evidence to every ticket regardless of type, so without
	// this gate the fallback branch used to match ArgoCD/generic tickets
	// too (isHighUtilization was also a no-op — see below), stamping every
	// incident with a misleading "approaching resource quota limits"
	// diagnosis that beat the real skill for that ticket in the ranked
	// match (higher confidence than llm_diagnosis's fixed 0.50).
	if t.Type != ticket.TypeResourceLimit {
		return skill.MatchResult{}
	}

	// The per-service summary-keyword match needs a specific Service to
	// name in the diagnosis. Tenant-wide quota alerts (TenantCPUQuotaHigh /
	// TenantMemoryQuotaHigh use `sum by (namespace)`, no pod label) arrive
	// with Service == "" and must fall through to the evidence-based
	// check below instead of being rejected outright — they're exactly
	// the alerts this skill exists to diagnose (Codex P1 on the first
	// version of this fix, which added Service == "" to this same early
	// return and made those quota alerts unmatchable everywhere).
	if t.Service != "" {
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

	// Fall back to evidence-based detection for resource-limit alerts
	// whose summary text doesn't include the usual keywords, or that
	// have no specific Service at all (tenant-wide quota alerts).
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
	diag += " The mctl optimizer right-sizes over-provisioned requests in this tenant on its daily pass; see GET /api/v1/optimizer/candidates for per-workload status."

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

// isHighUtilization reports whether any resource key present in both maps
// has used/allocated >= 0.8. Previously this only checked that both maps
// were non-empty ("if the JSON contains usage data, it was flagged for a
// reason") regardless of the actual numbers — since collectEvidence
// attaches resource evidence to every ticket, that made this true for
// virtually any tenant and defeated the documented 80% threshold entirely.
func isHighUtilization(resourceJSON string) bool {
	var data struct {
		Used      map[string]string `json:"used"`
		Allocated map[string]string `json:"allocated"`
	}
	if err := json.Unmarshal([]byte(resourceJSON), &data); err != nil {
		return false
	}
	for key, usedStr := range data.Used {
		allocStr, ok := data.Allocated[key]
		if !ok {
			continue
		}
		used, err := parseQuantity(usedStr)
		if err != nil {
			continue
		}
		allocated, err := parseQuantity(allocStr)
		if err != nil || allocated <= 0 {
			continue
		}
		if used/allocated >= 0.8 {
			return true
		}
	}
	return false
}

// parseQuantity parses a Kubernetes-style resource quantity ("500m", "2",
// "256Mi", "3Gi") into a float64 base unit (cores or bytes). Supports the
// decimal SI suffixes (m, k, M, G, T) and binary suffixes (Ki, Mi, Gi, Ti)
// used by mctl-api's resource usage responses. Pi and Ei are not handled —
// no tenant quota in this platform reaches petabyte scale.
func parseQuantity(s string) (float64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty quantity")
	}
	suffixes := []struct {
		suffix string
		mult   float64
	}{
		{"Ki", 1024},
		{"Mi", 1024 * 1024},
		{"Gi", 1024 * 1024 * 1024},
		{"Ti", 1024 * 1024 * 1024 * 1024},
		{"m", 0.001},
		{"k", 1000},
		{"M", 1_000_000},
		{"G", 1_000_000_000},
		{"T", 1_000_000_000_000},
	}
	for _, sfx := range suffixes {
		if strings.HasSuffix(s, sfx.suffix) {
			num, err := strconv.ParseFloat(strings.TrimSuffix(s, sfx.suffix), 64)
			if err != nil {
				return 0, err
			}
			return num * sfx.mult, nil
		}
	}
	return strconv.ParseFloat(s, 64)
}
