# Contributing to mctl-agent

## Prerequisites

- Go 1.25+
- Docker (for building images)

## Local Development

```bash
# Run with dry-run mode
DRY_RUN=true go run main.go

# Run tests
go test ./...
```

## Making Changes

1. Fork the repository
2. Create a feature branch: `git checkout -b feat/my-feature`
3. Make your changes
4. Run tests: `go test ./...`
5. Commit using conventional commits
6. Push and open a Pull Request

## How It Works

mctl-agent receives AlertManager webhooks, diagnoses issues using Claude API, and opens pull requests to the mctl-gitops repository with targeted fixes. All changes go through the standard GitOps review pipeline.
