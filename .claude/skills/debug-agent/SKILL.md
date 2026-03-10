# Debug mctl-agent Pipeline

## Overview
This skill helps you debug the mctl-agent pipeline — from alert reception to skill matching, diagnosis, fix generation, and PR creation.

## Architecture Flow

```
AlertManager → POST /api/v1/alerts → AlertHandler → ticket.Store.Create()
                                                          ↓
                                                   Pipeline.ProcessTicket()
                                                          ↓
                                              processTicketSync():
                                              1. collectEvidence()
                                              2. registry.Match()
                                              3. skill.Diagnose()
                                              4. skill.Fix()
                                              5. github.CreatePR()
                                              6. telegram.SendNotify()
```

## Key Files

| File | Purpose |
|------|---------|
| `internal/pipeline/pipeline.go` | Main orchestration: processTicketSync |
| `internal/pipeline/evidence.go` | Evidence collection from mctl-api |
| `internal/skill/registry.go` | Skill matching and ranking |
| `internal/skill/metrics.go` | Execution metrics, circuit breaker |
| `internal/monitor/alert.go` | AlertManager webhook handler |
| `internal/monitor/poller.go` | Periodic health check poller |
| `internal/api/router.go` | HTTP routes |
| `internal/ticket/store.go` | SQLite ticket persistence |

## Common Issues

### 1. Skill not matching
- Check if skill is registered in `builtin/register.go`
- Check if skill is disabled: `GET /api/v1/skills`
- Check circuit breaker: `GET /api/v1/skills/{name}/metrics` → `circuit_open`
- Verify evidence is being collected: add logging in `collectEvidence()`
- Check alert type matches what the skill expects

### 2. Evidence collection failing
- Evidence comes from `mctl-api` via `mctlclient.Client`
- Check `MCTL_API_URL` and `MCTL_API_TOKEN` env vars
- Check `internal/pipeline/evidence.go` for the evidence types collected
- Evidence types: status, config, logs, resources, audit

### 3. Fix not creating PR
- Check `DRY_RUN` env var (if true, no actual PRs)
- Check GitHub token permissions: `GITHUB_TOKEN`
- Check `handleHighConfidenceFix()` in pipeline.go
- Verify `fixer.DetectFilePath()` returns correct path
- Look at fix type switch: `bump_memory`, `rollback_image`, or LLM-generated

### 4. Pipeline paused
- Check: `GET /api/v1/skills` or telegram `/status`
- Resume: send `/resume` in Telegram or call `Pipeline.Resume()`

## Debugging Commands

```bash
# Build
go build ./...

# Run all tests
go test ./... -count=1

# Run specific package tests
go test ./internal/skill/... -v -count=1
go test ./internal/pipeline/... -v -count=1

# Check for compilation errors
go vet ./...

# Run with debug logging
MCTL_LOG_LEVEL=debug go run ./cmd/agent/

# Test a specific skill match
go test ./internal/skill/builtin/... -run TestOOMKilledSkillMatch -v
```

## Environment Variables

| Variable | Purpose | Required |
|----------|---------|----------|
| `MCTL_API_URL` | mctl-api base URL | Yes |
| `MCTL_API_TOKEN` | API authentication token | Yes |
| `GITHUB_TOKEN` | GitHub API token for PRs | Yes |
| `GITHUB_OWNER` | GitHub org/user | Yes |
| `GITHUB_REPO` | GitOps repo name | Yes |
| `TELEGRAM_BOT_TOKEN` | Telegram bot token | Yes |
| `TELEGRAM_CHAT_ID` | Telegram chat for notifications | Yes |
| `ANTHROPIC_API_KEY` | Claude API key (for LLM skill) | No |
| `DRY_RUN` | Disable actual PRs | No |
| `PORT` | HTTP listen port (default: 8080) | No |
| `DB_PATH` | SQLite database path | No |
| `POLL_INTERVAL` | Health check interval | No |

## SQLite Database

The agent uses a single SQLite database for:
- Tickets (`tickets` table)
- Skill metrics (`skill_executions` table)

Access via: `sqlite3 <DB_PATH>` or through `ticket.Store.DB()`
