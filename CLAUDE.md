# mctl-agent

Self-healing GitOps agent. Watches cluster for issues, diagnoses with Claude API, opens PRs.

## Stack
- Go, Claude API (Anthropic SDK)
- Receives AlertManager webhooks
- Opens PRs to mctl-gitops with fixes

## Conventions
- Go conventions: `go fmt`, `go vet`, error wrapping
- Structured logging with `slog`
- Context propagation for all I/O

## Alert Types Handled
- PodCrashLooping, KubePodNotReady
- TenantCPUQuotaHigh, TenantMemoryQuotaHigh
- ArgoWorkflowFailed, ArgoWorkflowHighFailureRate
