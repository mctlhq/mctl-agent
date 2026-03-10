# Test a Skill in Isolation

## Overview
This skill helps you write and run tests for mctl-agent skills without needing a real Kubernetes cluster, mctl-api, or GitHub.

## Test Pattern

All skill tests follow the same structure:

1. **Create skill instance** (no dependencies needed)
2. **Create mock ticket** with relevant type, tenant, service
3. **Create mock evidence** with relevant log/status content
4. **Call Match()** and assert matched/not-matched
5. **Call Diagnose()** and check diagnosis text, confidence, fixable
6. **Call Fix()** if skill supports it

## Template

```go
package builtin

import (
	"context"
	"testing"

	"github.com/mctlhq/mctl-agent/internal/skill"
	"github.com/mctlhq/mctl-agent/internal/ticket"
)

func TestMySkillMatch(t *testing.T) {
	s := NewMySkill()
	ctx := context.Background()

	tests := []struct {
		name       string
		ticketType string
		logs       string
		wantMatch  bool
		wantConf   float64
	}{
		{
			name:       "positive match",
			ticketType: "pod_crashloop",
			logs:       "ERROR: the thing happened",
			wantMatch:  true,
			wantConf:   0.85,
		},
		{
			name:       "no match - wrong logs",
			ticketType: "pod_crashloop",
			logs:       "INFO: everything fine",
			wantMatch:  false,
		},
		{
			name:       "no match - empty evidence",
			ticketType: "pod_crashloop",
			logs:       "",
			wantMatch:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tk := &ticket.Ticket{
				Type:    tt.ticketType,
				Tenant:  "test-team",
				Service: "test-svc",
			}
			ev := skill.NewEvidenceSet([]ticket.Evidence{
				{Type: "logs", Content: tt.logs},
			})

			result := s.Match(ctx, tk, ev)
			if result.Matched != tt.wantMatch {
				t.Errorf("Match() = %v, want %v", result.Matched, tt.wantMatch)
			}
			if tt.wantMatch && result.Confidence != tt.wantConf {
				t.Errorf("Confidence = %f, want %f", result.Confidence, tt.wantConf)
			}
		})
	}
}

func TestMySkillDiagnose(t *testing.T) {
	s := NewMySkill()
	ctx := context.Background()

	tk := &ticket.Ticket{
		Type:    "pod_crashloop",
		Tenant:  "billing",
		Service: "payment-api",
		Summary: "Pod crashlooping",
	}
	ev := skill.NewEvidenceSet([]ticket.Evidence{
		{Type: "logs", Content: "ERROR: the thing happened with details"},
	})

	diag, err := s.Diagnose(ctx, tk, ev)
	if err != nil {
		t.Fatal(err)
	}

	if diag.Diagnosis == "" {
		t.Error("expected non-empty diagnosis")
	}
	if diag.Confidence != ticket.ConfidenceHigh {
		t.Errorf("expected HIGH confidence, got %s", diag.Confidence)
	}
}
```

## Running Tests

```bash
# Run tests for a specific skill
go test ./internal/skill/builtin/... -run TestMySkill -v -count=1

# Run all builtin skill tests
go test ./internal/skill/builtin/... -v -count=1

# Run all skill tests (includes registry, metrics)
go test ./internal/skill/... -v -count=1

# Run with race detection
go test ./internal/skill/... -race -count=1

# Run full test suite
go test ./... -count=1
```

## Mock Evidence Cheat Sheet

```go
// Logs with OOM kill
ev := skill.NewEvidenceSet([]ticket.Evidence{
	{Type: "logs", Content: "OOMKilled: container xyz was killed due to OOM"},
})

// ArgoCD status — OutOfSync
ev := skill.NewEvidenceSet([]ticket.Evidence{
	{Type: "argocd_status", Content: `{"sync":"OutOfSync","health":"Healthy"}`},
})

// Multiple evidence types
ev := skill.NewEvidenceSet([]ticket.Evidence{
	{Type: "logs", Content: "Error logs here"},
	{Type: "config", Content: `{"image":{"tag":"v1.2.3"}}`},
	{Type: "resources", Content: `{"cpu_usage":"95%","memory_usage":"80%"}`},
	{Type: "audit", Content: `[{"action":"deploy","timestamp":"2025-01-01T00:00:00Z"}]`},
})

// Empty evidence (for testing no-match cases)
ev := skill.NewEvidenceSet(nil)
```

## Ticket Types
- `"pod_crashloop"` — CrashLoopBackOff, OOMKilled, etc.
- `"argocd_app_degraded"` — ArgoCD sync/health issues
- `"workflow_failed"` — Argo Workflow failures
- `"resource_limit"` — CPU/memory/quota limits
- `"deployment_failed"` — Deployment rollout failures

## Tips
- Match() must be **fast** and **side-effect free** — no API calls
- Use `strings.Contains()` or `regexp.MatchString()` for pattern matching
- Set confidence based on match specificity (see add-new-skill SKILL.md)
- Test edge cases: empty evidence, wrong ticket type, partial matches
- Use table-driven tests for Match() (multiple patterns to check)
