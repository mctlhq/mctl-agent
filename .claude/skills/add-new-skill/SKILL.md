# Add New Skill to mctl-agent

## Overview
This skill guides you through creating a new builtin Go skill for the mctl-agent self-healing pipeline.

## Architecture
- Skills implement the `skill.Skill` interface defined in `internal/skill/skill.go`
- Builtin skills live in `internal/skill/builtin/`
- Each skill has 3 phases: **Match** (fast pattern check) → **Diagnose** (analysis) → **Fix** (generate patch)

## Step-by-Step

### 1. Create the skill file

Create `internal/skill/builtin/<skill_name>.go`:

```go
package builtin

import (
	"context"
	"strings"

	"github.com/mctlhq/mctl-agent/internal/skill"
	"github.com/mctlhq/mctl-agent/internal/ticket"
)

type MyNewSkill struct{}

func NewMyNewSkill() *MyNewSkill { return &MyNewSkill{} }

func (s *MyNewSkill) Name() string        { return "my_new_skill" }
func (s *MyNewSkill) Version() string      { return "1.0.0" }
func (s *MyNewSkill) Description() string  { return "Detects and fixes <problem description>" }

func (s *MyNewSkill) RequiredCapabilities() []skill.CapabilityID {
	return []skill.CapabilityID{
		skill.CapReadLogs,
		skill.CapReadConfig,
		// Add only what's needed
	}
}

func (s *MyNewSkill) Match(_ context.Context, t *ticket.Ticket, ev skill.EvidenceSet) skill.MatchResult {
	// MUST be fast, side-effect free.
	// Check logs, alert type, or argocd_status evidence.
	logs := ev.Get("logs")
	if strings.Contains(logs, "my_error_pattern") {
		return skill.MatchResult{
			Matched:    true,
			Confidence: 0.85, // 0.0-1.0
			Priority:   100,
			Reason:     "Found my_error_pattern in logs",
		}
	}
	return skill.MatchResult{}
}

func (s *MyNewSkill) Diagnose(_ context.Context, t *ticket.Ticket, ev skill.EvidenceSet) (*skill.DiagnosisResult, error) {
	return &skill.DiagnosisResult{
		Diagnosis:  "Detailed explanation of what went wrong",
		Confidence: ticket.ConfidenceHigh, // "HIGH", "MEDIUM", "LOW"
		Fixable:    true,
		FixType:    "my_fix_type",
		// Optional fields for YAML patching:
		// YAMLPath, YAMLField, CurrentValue, SuggestedValue, Reasoning
	}, nil
}

func (s *MyNewSkill) Fix(_ context.Context, t *ticket.Ticket, diag *skill.DiagnosisResult) (*skill.FixResult, error) {
	// Return nil, error if not fixable.
	// Otherwise generate patch:
	return &skill.FixResult{
		FilePath:   "tenants/<tenant>/<service>/values.yaml",
		NewContent: "... patched content ...",
		Summary:    "One-line description of the fix",
	}, nil
}
```

### 2. Register the skill

In `internal/skill/builtin/register.go`, add to `RegisterAll()`:

```go
func RegisterAll(registry *skill.Registry, anthropicKey string) {
	// ... existing registrations ...
	registry.Register(NewMyNewSkill())
}
```

### 3. Create tests

Create `internal/skill/builtin/<skill_name>_test.go`:

```go
package builtin

import (
	"context"
	"testing"

	"github.com/mctlhq/mctl-agent/internal/skill"
	"github.com/mctlhq/mctl-agent/internal/ticket"
)

func TestMyNewSkillMatch(t *testing.T) {
	s := NewMyNewSkill()
	ctx := context.Background()

	tests := []struct {
		name    string
		logs    string
		want    bool
	}{
		{"matching pattern", "ERROR: my_error_pattern occurred", true},
		{"no match", "INFO: all good", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tk := &ticket.Ticket{Type: "pod_crashloop", Tenant: "test", Service: "svc"}
			ev := skill.NewEvidenceSet([]ticket.Evidence{
				{Type: "logs", Content: tt.logs},
			})
			result := s.Match(ctx, tk, ev)
			if result.Matched != tt.want {
				t.Errorf("Match() = %v, want %v", result.Matched, tt.want)
			}
		})
	}
}
```

### 4. Build and test

```bash
go build ./...
go test ./internal/skill/builtin/... -v -count=1
go test ./... -count=1
```

## Key Types Reference

| Type | Location | Purpose |
|------|----------|---------|
| `skill.Skill` | `internal/skill/skill.go` | Interface all skills implement |
| `skill.MatchResult` | `internal/skill/skill.go` | Match response with confidence |
| `skill.DiagnosisResult` | `internal/skill/skill.go` | Analysis output |
| `skill.FixResult` | `internal/skill/skill.go` | Patch/fix output |
| `skill.EvidenceSet` | `internal/skill/skill.go` | Evidence accessor |
| `skill.CapabilityID` | `internal/skill/skill.go` | Capability identifiers |
| `ticket.Ticket` | `internal/ticket/ticket.go` | Ticket with tenant, service, type |
| `ticket.Evidence` | `internal/ticket/ticket.go` | Type + Content string |

## Evidence Types
- `"logs"` — service logs from Loki
- `"alert"` — raw alert data
- `"argocd_status"` — sync/health status JSON
- `"config"` — service configuration (values.yaml, etc.)
- `"resources"` — Kubernetes resource status
- `"audit"` — recent operations audit log

## Confidence Guidelines
- **0.90-1.0**: Very specific pattern match (e.g., exact error string)
- **0.80-0.89**: Strong indicator (e.g., OOMKilled + memory data)
- **0.60-0.79**: Likely match (e.g., general crash pattern + recent deploy)
- **0.40-0.59**: Possible match, low confidence (e.g., LLM-based analysis)
- **Below 0.40**: Avoid — too many false positives

## Existing Skills for Reference
Look at these files for examples:
- `internal/skill/builtin/oomkilled.go` — simple pattern match + auto-fix
- `internal/skill/builtin/imagepull.go` — diagnostic only (no fix)
- `internal/skill/builtin/rollback.go` — multi-condition match
- `internal/skill/builtin/argocd_drift.go` — JSON status parsing
- `internal/skill/builtin/llm_diagnosis.go` — Claude API integration
