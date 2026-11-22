# Capability audit: declared vs used

Status: audit only. Nothing here is enforced; enforcement is a later step
(mctl-agent#115, owner decision). The audit table lives in
`internal/capability/audit_test.go` (`TestCapabilityDeclaredVsUsedAudit`).

Definitions:

- Declared: a `CapabilityID` constant in `internal/skill/skill.go`.
- Used: a `capability.Context` method in `internal/capability/capability.go`
  guards an operation with `c.check(skill.CapX)`.

The test reads both sources with `go/parser`, so it fails on drift in either
direction:

- a capability declared without an audit row (new, unclassified);
- a table row that disagrees with the source (used vs unused);
- a `check()` on something that is not a declared constant;
- a declared constant missing from `skill.AllCapabilityIDs()`, or the reverse.

When it fails, classify the capability, then update the table and this file.

## Result

Used (8): `read_logs`, `read_config`, `read_status`, `read_resources`,
`read_audit`, `modify_gitops` (guards `GetFileContent`), `create_pr`,
`send_notification`.

Declared but never used (3):

| Capability | Note |
|---|---|
| `merge_pr` | No `Context` method. No builtin declares it. PR merges bypass the capability package (`GitHubFixer.MergePR` called from `internal/pipeline` and `internal/api`). |
| `call_llm` | No `Context` method. Only `llm_diagnosis` declares it. |
| `exec_workflow` | No `Context` method and no builtin declares it. |

Used but undeclared: none.

## Related observations (not covered by the test)

- `capability.NewContext` has no production caller; only tests build a
  `Context`. The checks above are therefore not yet on any live path.
- `llm_diagnosis` declares `call_llm` plus three read capabilities but writes
  fixes; its declaration must be extended explicitly, with a test, before
  enforcement lands (see the constraints in #115).
- Notify-only skills must not gain `modify_gitops` / `create_pr`.
