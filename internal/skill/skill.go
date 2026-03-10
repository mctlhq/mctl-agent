package skill

import (
	"context"

	"github.com/mctlhq/mctl-agent/internal/ticket"
)

// CapabilityID identifies a platform capability that a skill may require.
type CapabilityID string

const (
	CapReadLogs      CapabilityID = "read_logs"
	CapReadConfig    CapabilityID = "read_config"
	CapReadStatus    CapabilityID = "read_status"
	CapReadResources CapabilityID = "read_resources"
	CapReadAudit     CapabilityID = "read_audit"
	CapModifyGitOps  CapabilityID = "modify_gitops"
	CapCreatePR      CapabilityID = "create_pr"
	CapMergePR       CapabilityID = "merge_pr"
	CapSendNotify    CapabilityID = "send_notification"
	CapCallLLM       CapabilityID = "call_llm"
	CapExecWorkflow  CapabilityID = "exec_workflow"
)

// Skill is the core abstraction — a modular, self-contained unit of agent behavior
// that can detect, diagnose, and optionally fix a specific class of problems.
type Skill interface {
	// Name returns a unique identifier for this skill (e.g. "oomkilled").
	Name() string

	// Version returns the skill version (e.g. "1.0.0").
	Version() string

	// Description returns a human-readable description.
	Description() string

	// Match determines whether this skill can handle the given ticket.
	// Called for every new ticket; should be fast and side-effect free.
	Match(ctx context.Context, t *ticket.Ticket, ev EvidenceSet) MatchResult

	// Diagnose analyzes the problem and returns a diagnosis.
	// Only called if Match returned Matched=true.
	Diagnose(ctx context.Context, t *ticket.Ticket, ev EvidenceSet) (*DiagnosisResult, error)

	// Fix generates a remediation for the diagnosed issue.
	// Only called if Diagnose returned Fixable=true.
	Fix(ctx context.Context, t *ticket.Ticket, diag *DiagnosisResult) (*FixResult, error)

	// RequiredCapabilities declares what platform capabilities this skill needs.
	RequiredCapabilities() []CapabilityID
}

// MatchResult is returned by Skill.Match.
type MatchResult struct {
	// Matched indicates the skill can handle this ticket.
	Matched bool

	// Confidence from 0.0 to 1.0 — how sure the skill is about the match.
	Confidence float64

	// Priority among skills with equal confidence (higher wins).
	Priority int

	// Reason is a short explanation of why this skill matched.
	Reason string
}

// DiagnosisResult contains the analysis output from a skill.
type DiagnosisResult struct {
	// Diagnosis is a human-readable explanation of the problem.
	Diagnosis string `json:"diagnosis"`

	// Confidence level: HIGH, MEDIUM, or LOW.
	Confidence string `json:"confidence"`

	// Fixable indicates whether the skill can generate a fix.
	Fixable bool `json:"fixable"`

	// YAMLPath is the GitOps file to modify (if fixable).
	YAMLPath string `json:"yaml_path,omitempty"`

	// YAMLField is the specific field to change (e.g. "resources.limits.memory").
	YAMLField string `json:"yaml_field,omitempty"`

	// CurrentValue is the current value of the field being changed.
	CurrentValue string `json:"current_value,omitempty"`

	// SuggestedValue is the recommended new value.
	SuggestedValue string `json:"suggested_value,omitempty"`

	// Reasoning explains why this fix is appropriate.
	Reasoning string `json:"reasoning,omitempty"`

	// FixType categorizes the fix (e.g. "bump_memory", "rollback_image").
	FixType string `json:"fix_type,omitempty"`

	// SkillName records which skill produced this diagnosis.
	SkillName string `json:"skill_name"`
}

// FixResult describes a generated remediation.
type FixResult struct {
	// FilePath in the GitOps repo that was patched.
	FilePath string

	// NewContent is the full file content after the patch.
	NewContent string

	// Summary is a one-line description of the change.
	Summary string

	// NextSkills suggests skills to run after this fix is applied.
	NextSkills []string
}

// EvidenceSet holds collected evidence for a ticket, keyed by evidence type.
type EvidenceSet struct {
	entries map[string]string
}

// NewEvidenceSet creates an EvidenceSet from ticket evidence.
func NewEvidenceSet(evs []ticket.Evidence) EvidenceSet {
	es := EvidenceSet{entries: make(map[string]string, len(evs))}
	for _, ev := range evs {
		es.entries[ev.Type] = ev.Content
	}
	return es
}

// Get returns the evidence content for the given type, or empty string.
func (es EvidenceSet) Get(evidenceType string) string {
	return es.entries[evidenceType]
}

// Has checks if evidence of the given type exists.
func (es EvidenceSet) Has(evidenceType string) bool {
	_, ok := es.entries[evidenceType]
	return ok
}

// All returns all evidence entries.
func (es EvidenceSet) All() map[string]string {
	cp := make(map[string]string, len(es.entries))
	for k, v := range es.entries {
		cp[k] = v
	}
	return cp
}

// Info contains metadata about a registered skill (for listing/discovery).
type Info struct {
	Name         string         `json:"name"`
	Version      string         `json:"version"`
	Description  string         `json:"description"`
	Capabilities []CapabilityID `json:"capabilities"`
	Enabled      bool           `json:"enabled"`
}
