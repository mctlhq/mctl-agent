# Correlating incidents, workflows, traces and logs

This page defines how to get from an incident or a workflow run to its trace, and
from there to the failed span and its log lines (mctlhq/mctl-agent#97). It covers
both `mctl-agent` (incidents) and the `mctl-agents` DevLoop (workflows). The
producer side of the DevLoop is documented in mctl-agents
`docs/observability/execution-traces.md` and is not repeated here.

## The contract is backend-neutral

Everything below is a W3C trace-context / OpenTelemetry identifier, carried as
plain data:

- `trace_id`: 32 lowercase hex characters.
- `span_id`: 16 lowercase hex characters.
- `traceparent`: `00-<trace_id>-<span_id>-<flags>`.

No identifier, field name or query step depends on a particular trace backend.
Spans leave every producer over OTLP and go to the platform OpenTelemetry Collector
(mctlhq/mctl-gitops#902), which decides where they are stored. If the backend
changes, the identifiers and the navigation stay the same; only the UI used to
open a trace id changes.

The IDs are **log fields, never Loki labels**. A label per trace would create a
stream per ticket or run. To find a trace's lines, search for the bare 32-hex
value. That one filter matches both producers' formats:

| Producer | How the IDs appear in a log line |
|---|---|
| `mctl-agent` | JSON fields `"trace_id":"…","span_id":"…"` on every line the pipeline (`internal/pipeline`) logs for a ticket while processing it, including the store-failure and lost-race lines. Here `span_id` is always the **root** processing span (`mctl_agent.process_ticket`), not the stage span a line was emitted in. Lines from other packages (fixer, skills, mctl-api client) carry no IDs. |
| `mctl-agents` worker | a ` trace_id=… span_id=…` suffix on lines logged inside a recording span |
| `mctl-agents` agent pods | one anchor line: `[trace] trace_id=… span_id=<pod root> argo_workflow=<name>` |

`span_id` is not interchangeable between the two producers. The `mctl-agents`
worker logs the span that is current when the line is written; `mctl-agent` logs
its root span for the whole run. Navigate by `trace_id`, and use the span tree
plus the time window to narrow down to a stage.

In every case a line names a trace only if the span was **sampled**. An unsampled
span is never exported, and a line pointing at it would send the reader to a trace
that no backend has.

## Identifiers by entry point

| Entry point | Durable identity | Where the trace id lives |
|---|---|---|
| Incident (`mctl-agent` ticket) | ticket `id` (= the mctl-api incident id) | the ticket's `trace_id`, stored in the agent's database and sent as `trace_id` on the incident publish and on each status update to mctl-api. Locally it belongs to the latest processing run; whether mctl-api keeps it is up to mctl-api, which ignores the field today. |
| DevLoop run | Temporal `workflow_id` + `run_id` | derived from the run (`sha256(workflow_id, run_id)`). It is passed to each Argo step as the `traceparent` operation parameter. |
| Argo step | Argo workflow name | the `traceparent` in the mctl-api audit record for that submit |
| Execution evidence | `evidence_id` | `execution.trace_id`; mctl-api can query by it (`GET /api/v1/evidence?trace_id=`) |

The ticket stores **no `span_id`**. A span id names one transient processing
attempt, not the incident. To get from a trace to its failing step, use the span
tree, not a stored span id.

## Navigation

### Incident → trace → logs

1. Read the incident's `trace_id`. An empty value means the run was not traced:
   tracing was off, or the span was unsampled. That is an expected state, not an
   error.
2. Open the trace id in the trace backend.
3. Find the incident's lines:
   `{namespace="admins", pod=~"admins-mctl-agent-base-service-.*"} |= "<trace_id>"`.
   The root span's `span_id` is on every line, and the ticket id is the `ticket`
   field.

### Workflow → trace

`mctl_get_workflow_status <argo workflow name>` returns the trace in one of two
places:

- **While the Argo object exists:** `live.trace.trace_id`.
- **After Argo has garbage-collected it:** the `live` block is gone, and the
  response says `Live status unavailable`. The trace identity is recovered from
  the durable audit record that mctl-api keeps for the submit:
  `audit.parameters.traceparent` (take the second `-`-separated field).
  `audit.parameters.temporal_workflow_id` and `temporal_run_id` name the DevLoop
  run.

For the Argo steps of one DevLoop run, `mctl_list_agent_executions
workflow_id=<temporal workflow id>` lists the steps' Argo workflow names. These
execution records do not themselves carry a trace id, so resolve it per step
through the audit record above.

### Failed span → Loki lines

1. In the trace, find the span with status `ERROR` and read its `error.type`.
2. Take the span's resource attribute `k8s.pod.name` and its start and end time.
3. Query that pod's whole stream over the span's time window, for example
   `{pod="<k8s.pod.name>"}` from `start - 5s` to `end + 5s`.

Agent pods print one `[trace]` anchor and do not stamp a span id on every line.
Inside a pod, the join from a child span to its lines is therefore
**pod + time window**, not an id match. When several spans of the same kind fall
in one window, their order in the trace matches the order of their lines in the
pod log.

Two lines that print the same 32-hex shape are not the run's trace:

- The `[identity] execution_context=` line's `trace_id` (ADR 011
  `ExecutionContext`).
- The `trace_id` on a context minted locally (`assertions.asserted_by: local`).

The trace is the value on the `[trace]` anchor, in `traceparent`, or in
`live.trace.trace_id` (see the follow-ups below).

## Degradation

Correlation is enrichment. It never gates incident or workflow processing:

- With no OTLP endpoint configured, `mctl-agent` installs no tracer provider. No
  IDs are logged or stored, and tickets are processed exactly as before.
- An exporter that fails or a Collector that is down does not change a ticket's
  outcome. Spans are dropped by the SDK, and the ticket still records its trace id,
  because whether a backend received the span is not the ticket's concern.
- The write that stores the trace id is best effort. If it fails, the result is a
  warning line, not a failed ticket.
- Stored IDs (ticket `trace_id`, audit `traceparent`, evidence `trace_id`) remain
  readable when the trace backend or Loki is down, because none of the read paths
  above calls either of them.

Tests: `internal/telemetry/logattrs_test.go`, `internal/pipeline/correlation_test.go`,
`internal/ticket/store_trace_test.go`, and `TestPublishAlertTraceID`.

## Privacy

Only identifiers are added: trace id, span id, ticket id. No prompt, completion,
MCP argument, tool output or credential is logged or stored by this correlation.
Redaction of span attributes stays with the producers and the Collector.

## Worked example: the navigation mechanics

This example shows that the mechanics work on a real DevLoop trace. It is **not**
evidence of a failed workflow or incident. The run succeeded and opened
mctlhq/mctl-api#566, and its one failed GitHub span is an expected probe (see the
follow-ups).

- Issue mctlhq/mctl-api#454 → DevLoop `dev-loop-mctlhq-mctl-api-454`, run
  `01a12c25-900b-73cf-bbca-c8bf22dd926e`, with steps
  `mctl-agents-investigate-3917e7bc`, `mctl-agents-approve-635afb50` and
  `mctl-agents-implement-c85f3135`.
- `mctl_get_workflow_status mctl-agents-implement-c85f3135`, after Argo GC, returns
  `audit.parameters.traceparent = 00-164ac97d8d7264326c406240d263dcc1-3a08536289d0606b-01`.
- Trace `164ac97d8d7264326c406240d263dcc1` has 56 spans. The span with status
  `ERROR` is `github.api.read`, service `mctl-agents-implementer`, span id
  `8e6d7a44b1735a35`, `error.type=exit_1`, 2026-10-11T18:13:46.730–47.220Z, on pod
  `mctl-agents-implement-c85f3135-run-implementer-2431022302`.
- `{namespace=~"admins|argo-workflows"} |= "164ac97d8d7264326c406240d263dcc1"`
  returns the exec and implement worker lines and the pod's `[trace]` anchor.
- Over the span's window, `{pod="mctl-agents-implement-c85f3135-run-implementer-2431022302"}`
  shows the second of three `gh` commands, `gh api repos/mctlhq/mctl-api/commits/feat%2F…`,
  which matches the second GitHub span in the trace.

## Follow-ups outside #97

- **mctl-agents (#196 ownership):** the `ExecutionContext.trace_id` naming
  collision. A locally minted context logs a random `trace_id` that is not the
  run's OTel trace.
- **mctl-agents:** the branch-existence probe's expected 404 is recorded as an
  `ERROR` span.
- **mctl-api:** storing the incident `trace_id` and returning it from
  `mctl_get_incident`, if it is still useful once `mctl-agent` sends it. mctl-api
  ignores the field today.
- **mctl-agents:** agent pods already write tool arguments to Loki
  (`AUDIT tool=… cmd=…`, `ToolUseBlock(input=…)`). This predates #97 and is not
  changed by it.
