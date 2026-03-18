# Argo Workflow Failure Runbook

## Triage order

1. Check workflow name and template — is it a deploy, build, or platform workflow?
2. Check the failing step: git commit, kubectl apply, Vault auth, or image build?
3. Check if this is a recurring failure (see historical incidents) or one-off
4. High failure rate (ArgoWorkflowHighFailureRate): check if multiple workflows fail or one template is broken

## Fix strategies

| Root cause | Fix type | Confidence |
|---|---|---|
| Git commit step fails (GITOPS_TOKEN expired) | Token rotation — manual action needed | LOW |
| Vault auth step fails | Check vault-backend ClusterSecretStore, token expiry | LOW |
| Image build fails (GitHub Actions) | Check repo CI logs, likely a code issue | LOW |
| Workflow template misconfiguration | Review template YAML, LOW confidence fix | LOW |

## Notes

- Most workflow failures require human investigation — avoid auto-fixes
- The audit log entry for the workflow is the most useful evidence
- Link to Argo UI: https://workflows.mctl.ai/workflows/{namespace}/{workflow_name}
- If ArgoWorkflowHighFailureRate: suppress individual tickets, escalate once
