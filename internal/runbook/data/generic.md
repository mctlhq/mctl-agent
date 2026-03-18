# Unknown / Generic Alert Runbook

## This alert type is not in the known classification list.

## Triage order

1. Read the raw alert labels carefully — alertname, namespace, pod, severity
2. Check service logs for error patterns around the alert timestamp
3. Check ArgoCD sync status — is the service degraded or out of sync?
4. Check recent deploys in the audit log — did something change recently?
5. Check resource usage — is the service under memory or CPU pressure?

## Fix strategies

- Only propose a fix if evidence clearly and unambiguously points to a cause
- If the evidence is ambiguous: set confidence=LOW, fixable=false
- If logs show OOMKilled: treat as resource_limit (bump_memory)
- If logs show crash after deploy: treat as pod_crashloop (rollback)
- For anything else: provide a clear diagnosis summary for manual review

## Response guidelines

- Be explicit about what you do NOT know
- Always include the raw alertname in the diagnosis
- Suggest what a human operator should check next
- Avoid speculation — if you cannot diagnose with evidence, say so
