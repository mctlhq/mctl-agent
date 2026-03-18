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

package ticket

import "time"

// Ticket types.
const (
	TypeArgoCDDegraded      = "argocd_app_degraded"
	TypeWorkflowFailed      = "workflow_failed"
	TypePodCrashloop        = "pod_crashloop"
	TypeResourceLimit       = "resource_limit"
	TypeDeploymentFailed    = "deployment_failed"
	TypeGitHubActionsFailed = "github_actions_failed"
	TypeGeneric             = "generic"
)

// Ticket sources.
const (
	SourceAlertManager  = "alertmanager"
	SourcePolling       = "polling"
	SourceGitHub        = "github"
	SourceGitHubWebhook = "github_webhook"
	SourceManual        = "manual"
)

// Ticket statuses.
const (
	StatusOpen         = "open"
	StatusAnalyzing    = "analyzing"
	StatusFixProposed  = "fix_proposed"
	StatusFixApplied   = "fix_applied"
	StatusResolved     = "resolved"
	StatusSuppressed   = "suppressed"
)

// Severity levels.
const (
	SeverityCritical = "critical"
	SeverityWarning  = "warning"
	SeverityInfo     = "info"
)

// Confidence levels.
const (
	ConfidenceHigh   = "HIGH"
	ConfidenceMedium = "MEDIUM"
	ConfidenceLow    = "LOW"
)

// Ticket is the central workflow object.
type Ticket struct {
	ID          string     `json:"id"`
	Source      string     `json:"source"`
	Type        string     `json:"type"`
	Tenant      string     `json:"tenant"`
	Service     string     `json:"service"`
	Summary     string     `json:"summary"`
	Severity    string     `json:"severity"`
	Status      string     `json:"status"`
	Evidence    []Evidence `json:"evidence"`
	Analysis    string     `json:"analysis,omitempty"`
	ProposedFix string     `json:"proposed_fix,omitempty"`
	PRURL       string     `json:"pr_url,omitempty"`
	PRNumber    int        `json:"pr_number,omitempty"`
	Confidence  string     `json:"confidence,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
	ResolvedAt  *time.Time `json:"resolved_at,omitempty"`
}

// Evidence attached to a ticket.
type Evidence struct {
	Type        string    `json:"type"`
	Content     string    `json:"content"`
	CollectedAt time.Time `json:"collected_at"`
}
