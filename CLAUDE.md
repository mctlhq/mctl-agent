# mctl-agent

Self-healing GitOps agent. Watches cluster for issues, diagnoses with Claude API, opens PRs.

## Stack
- Go 1.24, Claude API (Anthropic SDK)
- Storage: Postgres via `DATABASE_URL` (in-cluster deploy uses shared-pg); SQLite (modernc.org/sqlite) as local-dev fallback via `DB_PATH`
- Receives AlertManager webhooks + periodic polling
- Opens PRs to mctl-gitops with fixes
- Notifies via Telegram

## Architecture: Skills & Capabilities

The agent uses a modular **skills architecture**:

### Core
- `internal/skill/skill.go` — Skill interface, MatchResult, DiagnosisResult, FixResult, EvidenceSet
- `internal/skill/registry.go` — SkillRegistry: register, match (ranked by confidence), disable/enable
- `internal/skill/metrics.go` — DB-backed metrics + circuit breaker (auto-disables failing skills)
- `internal/capability/capability.go` — Provider + Context sandbox with per-skill access control
- `internal/pipeline/pipeline.go` — Orchestrates: ticket → evidence → skill match → diagnose → fix → PR → notify

### Skill Types
- `internal/skill/builtin/` — 12 compiled Go skills (OOMKilled, ImagePullBackOff, PostDeployRollback, ArgoCDDrift, ArgoCDSyncFailed, ProbeFix, CPUThrottle, QuotaAdjust, ScaleUp, GitHubActions, WorkflowFixer, LLMDiagnosis)
- `internal/skill/yaml/` — YAML-defined skills loaded from `skills/custom/` (hot-reload, no code)
- `internal/skill/remote/` — HTTP-delegating skills registered at runtime via REST API

### MCP & API
- `internal/mcp/` — MCP server (JSON-RPC 2.0) with 6 tools: list_skills, skill_status, disable/enable, trigger, all_metrics
- `internal/api/` — REST API: alerts webhook, telegram webhook, tickets, skills, skill metrics, remote skill registration

### Developer Workflow
- `.claude/skills/` — Instructions for AI coding agents: add-new-skill, debug-agent, test-skill

**Adding a new skill:** See `.claude/skills/add-new-skill/SKILL.md` for step-by-step guide.

## Conventions
- Go conventions: `go fmt`, `go vet`, error wrapping
- Structured logging with `slog`
- Context propagation for all I/O
- Table-driven tests for skill match/diagnose

## Releases
- Semantic version tags, NO `v` prefix (e.g. `1.9.0`). Pushing a matching tag
  triggers `.github/workflows/build.yml`, which builds/pushes the image and bumps
  the gitops manifest.
- Cut a release via a branch + PR (never directly on `main`): land a
  `chore: release x.y.z` commit that bumps the version, merge the PR, then tag
  the resulting merge commit with `x.y.z` and push the tag.

## Alert Types Handled
- PodCrashLooping, KubePodNotReady
- TenantCPUQuotaHigh, TenantMemoryQuotaHigh
- ArgoWorkflowFailed, ArgoWorkflowHighFailureRate

## API Endpoints
- `POST /api/v1/alerts` — AlertManager webhook
- `POST /api/v1/telegram` — Telegram bot webhook
- `GET /api/v1/tickets` — List tickets
- `GET /api/v1/skills` — List all skills
- `GET /api/v1/skills/{name}/metrics` — Skill metrics
- `POST /api/v1/skills/register` — Register remote skill
- `GET /api/v1/skills/remote` — List remote skills
- `POST /mcp` — MCP JSON-RPC endpoint
- `GET /healthz` / `GET /readyz` — Health checks
