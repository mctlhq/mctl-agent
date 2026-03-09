package ticket

import "time"

// Ticket types.
const (
	TypeArgoCDDegraded   = "argocd_app_degraded"
	TypeWorkflowFailed   = "workflow_failed"
	TypePodCrashloop     = "pod_crashloop"
	TypeResourceLimit    = "resource_limit"
	TypeDeploymentFailed = "deployment_failed"
)

// Ticket sources.
const (
	SourceAlertManager = "alertmanager"
	SourcePolling      = "polling"
	SourceGitHub       = "github"
	SourceManual       = "manual"
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
