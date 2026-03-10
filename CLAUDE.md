# mctl-agent

Self-healing GitOps agent. Watches cluster for issues, diagnoses with Claude API, opens PRs.

## Stack
- Go, Claude API (Anthropic SDK)
- Receives AlertManager webhooks
- Opens PRs to mctl-gitops with fixes

## Architecture: Skills & Capabilities

The agent uses a modular **skills architecture**:

- `internal/skill/` — Skill interface, Registry, EvidenceSet
- `internal/skill/builtin/` — Built-in skills (OOMKilled, ImagePull, Rollback, ArgoCDDrift, LLMDiagnosis)
- `internal/pipeline/` — Orchestrates: ticket → evidence → skill match → diagnose → fix → PR → notify

**Adding a new skill:** Create `internal/skill/builtin/my_skill.go` implementing `skill.Skill`, register in `builtin/register.go`.

## Conventions
- Go conventions: `go fmt`, `go vet`, error wrapping
- Structured logging with `slog`
- Context propagation for all I/O

## Alert Types Handled
- PodCrashLooping, KubePodNotReady
- TenantCPUQuotaHigh, TenantMemoryQuotaHigh
- ArgoWorkflowFailed, ArgoWorkflowHighFailureRate
