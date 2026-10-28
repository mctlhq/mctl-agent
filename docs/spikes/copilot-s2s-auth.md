# Spike: Copilot server-to-server (GitHub App) auth for mctl-agent

**Decision: no-go / not applicable for mctl-agent.** The code makes no Copilot
calls, and its GitHub credential is already a GitHub App installation token.
Adopting Copilot would mean adding a second LLM provider next to
`LLMDiagnosisSkill`, which issue #191 does not ask for.

## Credential inventory (verified against the repository at HEAD)

| Credential | Consumer | Destination | Type | Copilot? |
|---|---|---|---|---|
| `GITHUB_TOKEN` / `GITHUB_TOKEN_FILE` (`internal/config/config.go`) | `fixer.NewGitHubFixer`, `tokenSource`, `authTransport` (`internal/fixer/github.go`, `internal/fixer/token.go`); reused by `scoutspike` | `api.github.com`, `uploads.github.com` only (`defaultGitHubHosts`) | GitHub App installation token, rotated every 30 min (per code comments) | No |
| `ANTHROPIC_API_KEY` | `LLMDiagnosisSkill` (`internal/skill/builtin/llm_diagnosis.go`) | `api.anthropic.com/v1/messages` | Provider API key | No |
| `MCTL_API_TOKEN` (falls back to `GITHUB_TOKEN`) | `mctlclient.NewClient` | mctl-api (in-cluster) | Static service token | No |
| Telegram, webhook and AlertManager secrets | `internal/api`, `internal/notify` | Telegram / inbound | Static secrets | No |
| CI: mctl-agents App token (`.github/workflows/release-please.yml`) | release-please | GitHub | App installation token | No |

A case-insensitive search for `copilot` finds no match in Go code, config or
workflows.

## Feature summary (changelog, 2026-10-27)

All claims below are **unverified**: the changelog entry was not fetched in this
spike.

- Existence of Copilot server-to-server auth for GitHub Apps: unverified.
- Which Copilot APIs it covers (chat completions, review, agent): unverified.
- Required App permission: unverified.

Verify these before any "go" is given anywhere.

## Residual risk

The `MCTL_API_TOKEN` to `GITHUB_TOKEN` fallback in `internal/config/config.go`
(`Load`, see the comment above the fallback) is the only path where a
GitHub-issued token reaches a non-GitHub host (mctl-api). It is inert while
`MCTL_API_TOKEN` is set. Removing it is a separate change.

## Unverified deployed state

Not observed in this spike, so not asserted as absent: the pod env, the Vault key
names, and the job that mints the GitHub App token. The App-token claim rests on
code comments in `internal/fixer/token.go`.

## Follow-ups (outside this repo)

Where Copilot may actually be used, each a candidate for its own spike: the org
Copilot reviewer, IDE seats, and other mctlhq repos (mctl-agents, mctl-telegram,
mctl-api).

## Revisit trigger

Any future Copilot integration must use the App path: reuse `tokenSource` with a
separate host allowlist, never a personal access token. The guard test
`internal/fixer/copilot_guard_test.go` fails if a Copilot API host appears in
non-test Go source.
