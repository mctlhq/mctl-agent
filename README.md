# mctl-agent

Self-healing GitOps agent for the [mctl](https://mctl.ai) platform. Receives alerts from Prometheus AlertManager, diagnoses root causes using Claude API, and opens pull requests to [mctl-gitops](https://github.com/mctlhq/mctl-gitops) with targeted fixes.

## How It Works

```
AlertManager (Prometheus rules)
    |
    v
mctl-agent (webhook receiver)
    |
    v
Claude API (diagnose root cause)
    |
    v
Pull Request to mctl-gitops (fix)
    |
    v
Review -> Merge -> ArgoCD sync -> Fixed
```

1. **Receive** — AlertManager sends webhook notifications for cluster issues
2. **Diagnose** — Agent analyzes the alert context and queries Claude API for root cause analysis
3. **Fix** — Agent generates a targeted fix and opens a PR to the GitOps repository
4. **Review** — Human reviews the PR, merges it, ArgoCD deploys the fix

## Alert Types

| Alert | Trigger |
|---|---|
| PodCrashLooping | Pod restart count exceeds threshold |
| KubePodNotReady | Pod not ready for extended period |
| TenantCPUQuotaHigh | CPU usage approaching quota limit |
| TenantMemoryQuotaHigh | Memory usage approaching quota limit |
| ArgoWorkflowFailed | Argo Workflow execution failure |
| ArgoWorkflowHighFailureRate | High workflow failure rate |

## Architecture

```
cmd/agent/          — entrypoint
internal/
  api/              — HTTP server, webhook handler
  config/           — configuration
  diagnosis/        — Claude API integration, root cause analysis
  fixer/            — fix generation, PR creation
  mctlclient/       — mctl API client
  monitor/          — cluster monitoring
  notify/           — Telegram notifications
  pipeline/         — alert processing pipeline
  ticket/           — ticket/issue tracking
```

## Configuration

| Variable | Description | Default |
|---|---|---|
| `MCTL_API_URL` | mctl API endpoint | `https://api.mctl.ai` |
| `MCTL_API_TOKEN` | API authentication token | required |
| `ANTHROPIC_API_KEY` | Claude API key | required |
| `GITHUB_TOKEN` | GitHub token for PR creation | required |
| `GITHUB_REPO` | GitOps repository name | `mctl-gitops` |
| `TELEGRAM_BOT_TOKEN` | Telegram notifications | optional |
| `TELEGRAM_CHAT_ID` | Telegram chat for alerts | optional |
| `DRY_RUN` | Disable actual PR creation | `true` |
| `PORT` | HTTP server port | `8081` |

## Development

```bash
# Run in dry-run mode
DRY_RUN=true go run cmd/agent/main.go

# Run tests
go test ./...

# Build
go build -o bin/mctl-agent cmd/agent/main.go
```

## Deployment

The agent runs as a Kubernetes deployment in the `admins` namespace. GitHub App tokens are auto-rotated every 45 minutes via a CronWorkflow.

## License

Apache License 2.0 — see [LICENSE](LICENSE).
