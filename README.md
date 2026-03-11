# mctl-agent

Self-healing GitOps agent for Kubernetes that turns Prometheus alerts into automated pull requests.

## What It Does

mctl-agent receives alerts from Prometheus AlertManager (via webhook or periodic polling), diagnoses root causes using the Claude API and a library of builtin skills, and opens targeted fix PRs against the [mctl-gitops](https://github.com/mctlhq/mctl-gitops) repository. It supports six alert types — PodCrashLooping, KubePodNotReady, TenantCPUQuotaHigh, TenantMemoryQuotaHigh, ArgoWorkflowFailed, and ArgoWorkflowHighFailureRate — and notifies operators via Telegram throughout the lifecycle.

## Architecture

```
AlertManager Webhook / Periodic Polling
    → AlertHandler / Poller
    → Ticket Creation (SQLite)
    → Skill Matching (ranked by confidence)
    → Diagnosis (Claude API + builtin skills)
    → Fix Generation (targeted remediation)
    → PR Creation (GitHub)
    → Telegram Notification
```

Skills are resolved in three tiers:

1. **Builtin** — 9 compiled Go skills: OOMKilled, ImagePull, Rollback, ArgoCDDrift, ProbeFix, CPUThrottle, QuotaAdjust, ScaleUp, LLMDiagnosis
2. **YAML-defined** — hot-reloadable from `skills/custom/` (e.g. `high-restart-count.yaml`, `redis-connection-timeout.yaml`)
3. **Remote** — HTTP-delegating skills registered at runtime via REST API

## Tech Stack

| Category | Details |
|----------|---------|
| Language | Go 1.24 |
| Router | go-chi/chi v5.2.1 |
| GitHub client | google/go-github v68 |
| Database | SQLite via modernc.org/sqlite (pure Go, CGO-free) |
| AI | Anthropic Claude API |
| Container | Multi-stage Alpine 3.20 |
| CI/CD | GitHub Actions → GHCR |

## Project Structure

```
mctl-agent/
├── cmd/agent/main.go            # Entry point
├── internal/
│   ├── api/                     # HTTP REST & MCP server
│   ├── capability/              # Provider + context sandbox
│   ├── config/                  # Configuration loading
│   ├── diagnosis/               # Root cause analysis (Claude)
│   ├── evidence/                # Evidence collection
│   ├── fixer/                   # PR generation & GitHub integration
│   ├── mcp/                     # Model Context Protocol server
│   ├── mctlclient/              # mctl API client
│   ├── monitor/                 # Alert handling & polling
│   ├── notify/                  # Telegram notifications
│   ├── pipeline/                # Core orchestration pipeline
│   ├── skill/                   # Skill registry & management
│   │   ├── builtin/             # 9 compiled Go skills
│   │   ├── yaml/                # YAML-defined skills
│   │   └── remote/              # HTTP-delegating skills
│   └── ticket/                  # Ticket tracking (SQLite)
├── skills/custom/               # Custom YAML skill definitions
├── examples/                    # AlertManager config examples
├── Dockerfile
├── Makefile
├── go.mod / go.sum
└── .env.example
```

## Getting Started

### Prerequisites

- Go 1.24+
- An Anthropic API key (Claude)
- A GitHub token with repo scope
- (Optional) Telegram bot token and chat ID

### Local Development

```bash
cp .env.example .env          # fill in required variables
make build                    # CGO_ENABLED=0 static binary
make run                      # starts with DRY_RUN=true
make test                     # go test ./...
make fmt                      # gofmt
```

Or run directly:

```bash
DRY_RUN=true go run cmd/agent/main.go
```

### Docker

```bash
docker build -t mctl-agent .
docker run --env-file .env -p 8081:8081 -v agent-data:/data mctl-agent
```

The image uses a non-root user (`app`, uid 1000) and persists the SQLite database at `/data`.

## Configuration

### Environment Variables

| Variable | Description | Default | Required |
|----------|-------------|---------|----------|
| `PORT` | HTTP server port | `8081` | No |
| `DRY_RUN` | Disable actual PR creation | `true` | No |
| `MCTL_API_URL` | mctl API endpoint | `http://mctl-api.mctl-api.svc:8080` | No |
| `MCTL_API_TOKEN` | API authentication token | — | Yes |
| `ANTHROPIC_API_KEY` | Claude API key | — | Yes |
| `GITHUB_TOKEN` | GitHub token for PR creation | — | Yes |
| `GITHUB_OWNER` | GitHub organization | `mctlhq` | No |
| `GITHUB_REPO` | GitOps repository name | `mctl-gitops` | No |
| `TELEGRAM_BOT_TOKEN` | Telegram bot token | — | No |
| `TELEGRAM_CHAT_ID` | Telegram chat ID | — | No |
| `POLL_INTERVAL` | Polling frequency | `5m` | No |
| `DB_PATH` | SQLite database path | `/data/mctl-agent.db` | No |
| `MAX_PR_PER_HOUR` | Rate limit — PRs per hour | `5` | No |
| `MAX_PR_PER_DAY` | Rate limit — PRs per day | `20` | No |

## API / Endpoints

The agent exposes an HTTP API (chi router) for:

- **POST /api/v1/webhook** — AlertManager webhook receiver
- **GET /api/v1/tickets** — list tracked incidents
- **POST /api/v1/skills** — register a remote skill at runtime
- **GET /api/v1/health** — liveness probe

An MCP (Model Context Protocol) server is also available for tool-based AI integrations.

## Testing

12 test files using table-driven tests, mock HTTP servers, and structured logging verification:

```bash
go test ./...
```

## CI/CD

GitHub Actions workflow (`.github/workflows/build.yml`):

- **Triggers:** semver tags (`v*.*.*`) and PRs to `main`
- **Steps:** checkout → Go 1.24 → golangci-lint → build + test → Docker buildx → GHCR push → Trivy scan → GitOps auto-update → Telegram notify on failure
- **Registry:** `ghcr.io/mctlhq/mctl-agent`

## Deployment

The agent runs as a Kubernetes Deployment in the `admins` namespace. ArgoCD syncs its manifests from the mctl-gitops repository. GitHub App tokens are auto-rotated every 45 minutes via a CronWorkflow.

AlertManager configuration examples are in [`examples/`](examples/).

## Release Process

1. Tag a semver release: `git tag v1.2.3 && git push --tags`
2. CI builds, scans, and pushes the image to GHCR
3. The workflow updates the image tag in mctl-gitops
4. ArgoCD detects the change and rolls out the new version

## Related Projects

- [mctl-api](https://github.com/mctlhq/mctl-api) — REST API + MCP server (Go)
- [mctl-gitops](https://github.com/mctlhq/mctl-gitops) — GitOps source of truth + CLI (Helm, ArgoCD, Go)
- [mctl-portal](https://github.com/mctlhq/mctl-portal) — Developer portal (TypeScript, Backstage)
- [mctl-web](https://github.com/mctlhq/mctl-web) — Landing page + MCP connector (HTML, Cloudflare)

## License

Apache License 2.0 — see [LICENSE](LICENSE).
