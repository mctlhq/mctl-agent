# Resource Limit / Quota Runbook

## Triage order

1. Check which resource is exhausted: CPU, memory, or namespace quota
2. For a single service: check resources.requests vs resources.limits in its values.yaml
3. For namespace quota: compare ResourceQuota used vs hard limits
4. Check if the problem is a spike (temporary) or a steady-state leak (permanent)

## Fix strategies

| Root cause | Fix type | Confidence |
|---|---|---|
| Service OOMKilled / memory near limit | bump_memory — +50% on limits.memory | HIGH |
| Service CPU throttled consistently | bump_cpu — double limits.cpu | MEDIUM |
| Namespace quota exhausted | quota_adjust — requires admin approval | MEDIUM |
| Node pressure (NodeHighCPU, NodeHighMemory, NodeDiskPressure) | NO AUTO-FIX — infra alert, escalate | — |
| VaultSealed | NO AUTO-FIX — infra alert, escalate immediately | — |

## Safety rules

- NEVER auto-fix node-level or Vault alerts — these are infrastructure, not tenant issues
- For namespace quota: always notify, never auto-apply (requires admin review)
- CPU bumps: double is usually safe; memory bumps: +50% per iteration
