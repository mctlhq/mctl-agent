# Pod CrashLoop Runbook

## Triage order

1. Check container exit code in logs
   - Exit 137 (OOMKilled) → increase resources.limits.memory
   - Exit 1/2 after a recent deploy → rollback image.tag
   - Exit 1/2 on a stable service → check env vars, Vault secret sync, probe config
2. Check recent deploys in audit log — if crash started right after a deploy, rollback first
3. Check resource usage — if memory is at limit, bump before diagnosing further

## Fix strategies

| Root cause | Fix type | Confidence |
|---|---|---|
| OOMKilled (exit 137 in logs) | bump_memory — +50% on limits.memory | HIGH |
| Crash after recent deploy | rollback_image — revert image.tag | HIGH |
| Probe misconfiguration | adjust_probe — fix initialDelaySeconds or path | MEDIUM |
| Unknown / missing evidence | no fix — LOW confidence, manual review | LOW |

## Safety rules

- Never rollback unless audit log confirms a recent deploy preceded the crash
- Never bump memory above 4x the current value in a single PR
- If both OOMKilled AND recent deploy: prefer rollback first (faster to verify)
