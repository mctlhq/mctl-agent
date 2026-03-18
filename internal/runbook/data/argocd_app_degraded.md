# ArgoCD App Degraded Runbook

## Triage order

1. Check ArgoCD sync status: OutOfSync, Degraded, or Missing resources?
2. Check the last sync result — did it fail due to a manifest error or a timeout?
3. Check if the Helm values.yaml was recently changed (audit log)
4. Check if the underlying resources (Deployment, Service) are healthy in Kubernetes

## Fix strategies

| Root cause | Fix type | Confidence |
|---|---|---|
| Image tag not found in GHCR | rollback_image or fix CI pipeline | MEDIUM |
| Invalid values.yaml (parse error) | manual fix — LOW confidence | LOW |
| Resource quota exceeded | quota_adjust | MEDIUM |
| ArgoCD sync timeout (transient) | no fix needed — re-sync will self-heal | LOW |
| Missing CRD or dependency | NO AUTO-FIX — infra issue | — |

## Notes

- Most ArgoCD degraded states self-heal after fixing the root cause
- Check the ArgoCD app events for the specific error message
- A recent GitOps commit that introduced a bad manifest is the most common cause
