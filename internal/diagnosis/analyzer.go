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

package diagnosis

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/mctlhq/mctl-agent/internal/mctlclient"
	"github.com/mctlhq/mctl-agent/internal/ticket"
)

// Analyzer collects evidence and diagnoses tickets.
type Analyzer struct {
	apiClient   *mctlclient.Client
	store       *ticket.Store
	anthropicKey string
}

// NewAnalyzer creates a new diagnosis analyzer.
func NewAnalyzer(apiClient *mctlclient.Client, store *ticket.Store, anthropicKey string) *Analyzer {
	return &Analyzer{
		apiClient:    apiClient,
		store:        store,
		anthropicKey: anthropicKey,
	}
}

// DiagnosisResult contains the analysis output.
type DiagnosisResult struct {
	Diagnosis      string `json:"diagnosis"`
	Confidence     string `json:"confidence"`
	Fixable        bool   `json:"fixable"`
	YAMLPath       string `json:"yaml_path"`
	YAMLField      string `json:"yaml_field"`
	CurrentValue   string `json:"current_value"`
	SuggestedValue string `json:"suggested_value"`
	Reasoning      string `json:"reasoning"`
	FromPattern    bool   `json:"-"`
	PatternFixType string `json:"-"`
}

// Analyze collects evidence and diagnoses the issue for a ticket.
func (a *Analyzer) Analyze(ctx context.Context, t *ticket.Ticket) (*DiagnosisResult, error) {
	// Collect evidence from mctl-api.
	a.collectEvidence(ctx, t)

	// Reload ticket with evidence.
	t, err := a.store.Get(t.ID)
	if err != nil {
		return nil, fmt.Errorf("reloading ticket: %w", err)
	}

	// Check for recent deploys.
	auditEntries, _ := a.apiClient.ListAudit()
	var diagAudit []AuditEntry
	for _, e := range auditEntries {
		diagAudit = append(diagAudit, AuditEntry{
			User:      e.User,
			Action:    e.Action,
			Target:    e.Target,
			Timestamp: e.Timestamp,
		})
	}
	recentDeploy := HasRecentDeploy(t.Tenant, t.Service, diagAudit)

	// Try known patterns first (zero LLM cost).
	pattern := MatchKnownPattern(t, recentDeploy)
	if pattern.Matched {
		slog.Info("known pattern matched", "ticket", t.ID, "diagnosis", pattern.Diagnosis)
		return &DiagnosisResult{
			Diagnosis:      pattern.Diagnosis,
			Confidence:     pattern.Confidence,
			Fixable:        pattern.Fixable,
			FromPattern:    true,
			PatternFixType: pattern.FixType,
		}, nil
	}

	// Fall back to Claude API.
	if a.anthropicKey == "" {
		return &DiagnosisResult{
			Diagnosis:  "Unable to diagnose — no Anthropic API key configured. Evidence collected for manual review.",
			Confidence: ticket.ConfidenceLow,
			Fixable:    false,
		}, nil
	}

	return a.callClaude(ctx, t)
}

func (a *Analyzer) collectEvidence(ctx context.Context, t *ticket.Ticket) {
	now := time.Now().UTC()

	// Workflow specific evidence.
	if t.Type == ticket.TypeWorkflowFailed && t.Service != "" {
		if wf, err := a.apiClient.GetWorkflow(t.Service); err == nil {
			_ = a.store.AddEvidence(t.ID, ticket.Evidence{
				Type:        "workflow_live_status",
				Content:     ticket.EvidenceJSON(wf),
				CollectedAt: now,
			})
		}
	}

	// Status from ArgoCD.
	if t.Service != "" && t.Type != ticket.TypeWorkflowFailed {
		if status, err := a.apiClient.GetServiceStatus(t.Tenant, t.Service); err == nil {
			_ = a.store.AddEvidence(t.ID, ticket.Evidence{
				Type:        "argocd_status",
				Content:     ticket.EvidenceJSON(status),
				CollectedAt: now,
			})
		}
	}

	// Service config.
	if config, err := a.apiClient.GetServiceConfig(t.Tenant, t.Service); err == nil {
		_ = a.store.AddEvidence(t.ID, ticket.Evidence{
			Type:        "config",
			Content:     ticket.EvidenceJSON(config),
			CollectedAt: now,
		})
	}

	// Logs (100 lines, 1 hour).
	if logs, err := a.apiClient.GetServiceLogs(t.Tenant, t.Service, 100, time.Hour); err == nil {
		_ = a.store.AddEvidence(t.ID, ticket.Evidence{
			Type:        "logs",
			Content:     ticket.EvidenceJSON(logs),
			CollectedAt: now,
		})
	}

	// Resource usage.
	if resources, err := a.apiClient.GetResourceUsage(t.Tenant); err == nil {
		_ = a.store.AddEvidence(t.ID, ticket.Evidence{
			Type:        "resources",
			Content:     ticket.EvidenceJSON(resources),
			CollectedAt: now,
		})
	}

	// Audit log.
	if audit, err := a.apiClient.ListAudit(); err == nil {
		_ = a.store.AddEvidence(t.ID, ticket.Evidence{
			Type:        "audit",
			Content:     ticket.EvidenceJSON(audit),
			CollectedAt: now,
		})
	}
}

// callClaude sends the ticket + evidence to the Anthropic Messages API.
func (a *Analyzer) callClaude(ctx context.Context, t *ticket.Ticket) (*DiagnosisResult, error) {
	// Route to model based on ticket type.
	model := "claude-sonnet-4-20250514"
	if t.Type == ticket.TypePodCrashloop || t.Type == ticket.TypeResourceLimit {
		model = "claude-haiku-4-5-20251001"
	}

	// Build user message with ticket + evidence.
	userMsg := buildUserMessage(t)

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
	req.Header.Set("x-api-key", a.anthropicKey)
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

	// Parse response.
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

	var result DiagnosisResult
	if err := json.Unmarshal([]byte(text), &result); err != nil {
		// If Claude didn't return valid JSON, use the text as diagnosis.
		return &DiagnosisResult{
			Diagnosis:  text,
			Confidence: ticket.ConfidenceLow,
			Fixable:    false,
		}, nil
	}

	return &result, nil
}

func buildUserMessage(t *ticket.Ticket) string {
	var sb bytes.Buffer
	fmt.Fprintf(&sb, "## Incident Ticket\n")
	fmt.Fprintf(&sb, "- ID: %s\n", t.ID)
	fmt.Fprintf(&sb, "- Type: %s\n", t.Type)
	fmt.Fprintf(&sb, "- Tenant: %s\n", t.Tenant)
	fmt.Fprintf(&sb, "- Service: %s\n", t.Service)
	fmt.Fprintf(&sb, "- Severity: %s\n", t.Severity)
	fmt.Fprintf(&sb, "- Summary: %s\n", t.Summary)
	fmt.Fprintf(&sb, "- Source: %s\n\n", t.Source)

	for _, ev := range t.Evidence {
		fmt.Fprintf(&sb, "## Evidence: %s (collected %s)\n", ev.Type, ev.CollectedAt.Format(time.RFC3339))
		fmt.Fprintf(&sb, "```json\n%s\n```\n\n", ev.Content)
	}

	return sb.String()
}
