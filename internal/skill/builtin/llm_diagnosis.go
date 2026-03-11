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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/mctlhq/mctl-agent/internal/skill"
	"github.com/mctlhq/mctl-agent/internal/ticket"
)

const systemPrompt = `You are a platform engineer diagnosing issues in a Kubernetes-based multi-tenant platform (mctlhq).

## Platform Architecture
- GitOps: ArgoCD syncs from mctl-gitops repo (source of truth)
- Services deployed via base-service Helm chart with per-service values.yaml
- Tenant namespaces with ResourceQuotas
- CI: GitHub Actions → GHCR → GitOps image tag update → ArgoCD sync
- Monitoring: Prometheus + AlertManager → Telegram notifications

## GitOps File Structure
- Standard services: platform-gitops/services/{team}/{service}/values.yaml
- Platform services (mctl-api): platform-gitops/apps/templates/{service}.yaml (inline helm values)
- Helm chart schema: image.repository, image.tag, resources.requests/limits, env, ingress, probes

## Common Issues & Fixes
1. OOMKilled → increase resources.limits.memory (typically 50% bump)
2. CrashLoopBackOff after deploy → rollback image.tag to previous version
3. ImagePullBackOff → check image tag exists, check imagePullSecrets
4. Degraded ArgoCD app → check sync status, resource health, events
5. ResourceQuota exceeded → adjust tenant quotas or service resource requests
6. Workflow failures → check workflow templates, input parameters, permissions

## Safety Rules
- NEVER suggest fixes for infrastructure alerts (Node*, VaultSealed)
- NEVER modify anything outside platform-gitops/ directory
- ONLY modify values.yaml files (resources, image tags, env vars)
- When unsure, set confidence to LOW and fixable to false

## Response Format
Respond ONLY with valid JSON:
{
  "diagnosis": "Clear explanation of what went wrong and why",
  "confidence": "HIGH|MEDIUM|LOW",
  "fixable": true/false,
  "yaml_path": "platform-gitops/services/{team}/{service}/values.yaml",
  "yaml_field": "resources.limits.memory",
  "current_value": "256Mi",
  "suggested_value": "384Mi",
  "reasoning": "Brief explanation of why this fix is appropriate"
}`

// LLMDiagnosisSkill uses the Claude API as a fallback for tickets
// that no pattern-based skill could handle.
type LLMDiagnosisSkill struct {
	anthropicKey string
}

func NewLLMDiagnosisSkill(anthropicKey string) *LLMDiagnosisSkill {
	return &LLMDiagnosisSkill{anthropicKey: anthropicKey}
}

func (s *LLMDiagnosisSkill) Name() string    { return "llm_diagnosis" }
func (s *LLMDiagnosisSkill) Version() string { return "1.0.0" }

func (s *LLMDiagnosisSkill) Description() string {
	return "Fallback skill: sends ticket + evidence to Claude API for diagnosis"
}

func (s *LLMDiagnosisSkill) RequiredCapabilities() []skill.CapabilityID {
	return []skill.CapabilityID{skill.CapCallLLM, skill.CapReadLogs, skill.CapReadConfig, skill.CapReadStatus}
}

// Match always returns true with low confidence — this is the fallback skill.
func (s *LLMDiagnosisSkill) Match(_ context.Context, _ *ticket.Ticket, _ skill.EvidenceSet) skill.MatchResult {
	if s.anthropicKey == "" {
		return skill.MatchResult{}
	}
	return skill.MatchResult{
		Matched:    true,
		Confidence: 0.50,
		Priority:   1, // Lowest priority — only used when no other skill matches better.
		Reason:     "LLM fallback — no pattern-based skill matched with high confidence",
	}
}

func (s *LLMDiagnosisSkill) Diagnose(ctx context.Context, t *ticket.Ticket, ev skill.EvidenceSet) (*skill.DiagnosisResult, error) {
	if s.anthropicKey == "" {
		return &skill.DiagnosisResult{
			Diagnosis:  "Unable to diagnose — no Anthropic API key configured. Evidence collected for manual review.",
			Confidence: ticket.ConfidenceLow,
			Fixable:    false,
			SkillName:  s.Name(),
		}, nil
	}

	// Route to model based on ticket type.
	model := "claude-sonnet-4-20250514"
	if t.Type == ticket.TypePodCrashloop || t.Type == ticket.TypeResourceLimit {
		model = "claude-haiku-4-5-20251001"
	}

	userMsg := buildUserMessage(t, ev)

	reqBody := map[string]interface{}{
		"model":      model,
		"max_tokens": 1024,
		"system":     systemPrompt,
		"messages": []map[string]string{
			{"role": "user", "content": userMsg},
		},
	}

	jsonBody, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshaling request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", "https://api.anthropic.com/v1/messages", bytes.NewReader(jsonBody))
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", s.anthropicKey)
	req.Header.Set("anthropic-version", "2023-06-01")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("claude API request: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("claude API returned %d: %s", resp.StatusCode, string(body))
	}

	var apiResp struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(body, &apiResp); err != nil {
		return nil, fmt.Errorf("parsing claude response: %w", err)
	}

	if len(apiResp.Content) == 0 {
		return nil, fmt.Errorf("empty claude response")
	}

	text := apiResp.Content[0].Text

	var result skill.DiagnosisResult
	if err := json.Unmarshal([]byte(text), &result); err != nil {
		return &skill.DiagnosisResult{
			Diagnosis:  text,
			Confidence: ticket.ConfidenceLow,
			Fixable:    false,
			SkillName:  s.Name(),
		}, nil
	}

	result.SkillName = s.Name()
	return &result, nil
}

func (s *LLMDiagnosisSkill) Fix(_ context.Context, t *ticket.Ticket, diag *skill.DiagnosisResult) (*skill.FixResult, error) {
	if diag.YAMLField == "" || diag.SuggestedValue == "" {
		return nil, fmt.Errorf("diagnosis missing yaml_field or suggested_value")
	}

	return &skill.FixResult{
		Applied:  true,
		FilePath: detectFilePath(t.Tenant, t.Service),
		Summary:  fmt.Sprintf("Update %s from %s to %s", diag.YAMLField, diag.CurrentValue, diag.SuggestedValue),
	}, nil
}

func buildUserMessage(t *ticket.Ticket, ev skill.EvidenceSet) string {
	var sb bytes.Buffer
	fmt.Fprintf(&sb, "## Incident Ticket\n")
	fmt.Fprintf(&sb, "- ID: %s\n", t.ID)
	fmt.Fprintf(&sb, "- Type: %s\n", t.Type)
	fmt.Fprintf(&sb, "- Tenant: %s\n", t.Tenant)
	fmt.Fprintf(&sb, "- Service: %s\n", t.Service)
	fmt.Fprintf(&sb, "- Severity: %s\n", t.Severity)
	fmt.Fprintf(&sb, "- Summary: %s\n", t.Summary)
	fmt.Fprintf(&sb, "- Source: %s\n\n", t.Source)

	for k, v := range ev.All() {
		fmt.Fprintf(&sb, "## Evidence: %s\n```json\n%s\n```\n\n", k, v)
	}

	return sb.String()
}
