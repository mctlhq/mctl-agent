// Copyright 2025 MCTL Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/mctlhq/mctl-agent/internal/ctxutil"
	"github.com/mctlhq/mctl-agent/internal/ticket"
)

// alertStore is the slice of *ticket.Store that AlertHandler actually uses.
// The narrow interface exists so tests can inject a store that fails on a
// chosen call: the batch-drain guarantee this handler rests on — one alert's
// store error must not stop the rest of the batch from being processed —
// cannot be exercised against a real *ticket.Store, where the only way to
// provoke an error (closing the database) fails every alert at once.
type alertStore interface {
	ResolveByTenantService(ctx context.Context, tenant, service, ticketType, fingerprint string, notAfter time.Time) ([]string, error)
	FindDuplicate(ctx context.Context, tenant, service, ticketType string) (*ticket.Ticket, error)
	TouchWithFingerprint(ctx context.Context, id, fingerprint string) error
	FindRecentlyResolved(ctx context.Context, tenant, service, ticketType, alertName string, window time.Duration) (*ticket.Ticket, error)
	Create(ctx context.Context, t *ticket.Ticket) error
	AddEvidence(ctx context.Context, ticketID string, e ticket.Evidence) error
}

// AlertHandler receives AlertManager webhooks and creates tickets.
type AlertHandler struct {
	store    alertStore
	onTicket func(*ticket.Ticket)
	// FlapCooldown suppresses creation of a new ticket for the same
	// (tenant, service, type) if a previous ticket was resolved within
	// this window. Zero disables the cooldown.
	FlapCooldown time.Duration
	// IgnoreService, when non-nil, drops firing alerts whose extracted
	// service name matches the pattern. Resolved alerts still flow through
	// so pre-filter tickets can close normally. Nil means no filter.
	IgnoreService *regexp.Regexp
	// OnResolve, when non-nil, is invoked with the IDs of tickets that
	// transitioned to resolved as a result of an AlertManager `resolved`
	// webhook. Used to fan out the resolution to external incident
	// stores (mctl-api's `alerts` table) that would otherwise remain
	// `open` forever — a publish-only feed from PublishAlert with no
	// counterpart resolve channel was the root cause of the 198 stale
	// incidents accumulated by 2026-05-12.
	OnResolve func(ids []string)
}

// NewAlertHandler creates a new AlertManager webhook handler.
func NewAlertHandler(store *ticket.Store, onTicket func(*ticket.Ticket)) *AlertHandler {
	return &AlertHandler{store: store, onTicket: onTicket}
}

// alertManagerPayload is the AlertManager webhook JSON structure.
type alertManagerPayload struct {
	Status string  `json:"status"`
	Alerts []alert `json:"alerts"`
}

type alert struct {
	Fingerprint string            `json:"fingerprint"`
	Status      string            `json:"status"`
	Labels      map[string]string `json:"labels"`
	Annotations map[string]string `json:"annotations"`
	StartsAt    time.Time         `json:"startsAt"`
	EndsAt      time.Time         `json:"endsAt"`
}

// maxAlertBodyBytes bounds inbound AlertManager payloads. Firing-alert
// batches are a few KB at most; 1MB leaves generous headroom while
// stopping an oversized body from being read fully into memory.
const maxAlertBodyBytes = 1 << 20 // 1MB

// ServeHTTP handles POST /api/v1/alerts.
func (h *AlertHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxAlertBodyBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "failed to read body", http.StatusBadRequest)
		return
	}

	var payload alertManagerPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}

	// A store failure must reach AlertManager as 5xx. AM retries a webhook
	// only on 5xx; a 200 tells it the batch is delivered and it never sends
	// that notification again. For a `resolved` webhook that is permanent:
	// the ticket stays open forever, and the poller's AM reconcile is the
	// only thing left that might close it. Answering 500 costs a redelivery
	// of the whole batch, which is safe — every path here is idempotent by
	// (tenant, service, type): a re-sent firing alert dedups onto the
	// existing ticket, a re-sent resolve finds nothing left open.
	//
	// Processing continues past the first failure so one bad alert does not
	// hide the rest of the batch; the first error is what gets reported.
	// Two nested bounds, per ctxutil.DetachedBatch: the batch context caps the
	// total detached work this one delivery can cause, and each alert still
	// gets its own ceiling underneath it so a slow early alert cannot spend
	// the whole budget and starve the tail on every redelivery.
	batchCtx, batchCancel := ctxutil.DetachedBatch(r.Context(), len(payload.Alerts))
	defer batchCancel()

	var firstErr error
	processed := 0
	for _, a := range payload.Alerts {
		// Stop rather than hand the remaining alerts a context that is
		// already spent. Under sustained store latency the batch budget runs
		// out partway; continuing would issue store calls that cannot
		// succeed, adding load to the database that is the reason they are
		// slow. The 500 below asks AlertManager to redeliver, which is the
		// backpressure this situation actually calls for.
		//
		// The tail of a long batch therefore waits for the store to recover
		// before it is processed. That is a deliberate trade, not an
		// oversight: bounding total detached work and giving every alert in an
		// unbounded batch a full ceiling cannot both hold in a sequential
		// loop, and unbounded work during a database outage is the worse of
		// the two. Normal batches never reach this branch — the budget covers
		// MaxBatchWrite/WriteTimeout alerts at full latency, and far more when
		// the store is healthy, because a fast alert returns its ceiling
		// unused.
		if err := batchCtx.Err(); err != nil {
			slog.Error("alert batch budget exhausted, deferring the rest to redelivery",
				"processed", processed, "total", len(payload.Alerts), "error", err)
			if firstErr == nil {
				firstErr = fmt.Errorf("batch budget exhausted after %d/%d alerts: %w",
					processed, len(payload.Alerts), err)
			}
			break
		}

		// Derived from batchCtx, not from r.Context(): the request's
		// cancellation is already stripped upstream, and nesting here is what
		// keeps the two bounds composed rather than competing.
		err := func() error {
			ctx, cancel := context.WithTimeout(batchCtx, ctxutil.WriteTimeout)
			defer cancel()
			return h.processAlert(ctx, a)
		}()
		processed++
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if firstErr != nil {
		slog.Error("alert batch not fully processed, asking AlertManager to retry",
			"error", firstErr, "alerts", len(payload.Alerts))
		http.Error(w, "failed to process alerts", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"accepted"}`))
}

// processAlert turns one AlertManager alert into ticket state. It returns
// an error only for failures that a retry could fix — i.e. store failures —
// so ServeHTTP can ask AlertManager to redeliver. Everything else is handled
// in place.
func (h *AlertHandler) processAlert(ctx context.Context, a alert) error {
	alertName := a.Labels["alertname"]
	namespace := a.Labels["namespace"]
	pod := a.Labels["pod"]
	workflow := a.Labels["name"]

	tType, severity := classifyAlert(alertName)

	tenant := namespace
	service := extractService(pod)
	// Workload-object alerts (KubeDeploymentReplicasMismatch,
	// KubeStatefulSetReplicasMismatch, KubeDeploymentRolloutStuck, ...)
	// are computed from kube-state-metrics series like
	// kube_deployment_spec_replicas, which name the object in a
	// `deployment`/`statefulset`/`daemonset` label and carry NO `pod`
	// label of their own. The scrape target's own labels then survive, so
	// `pod` is kube-state-metrics' pod and extractService() yields
	// "monitoring-kube-state-metrics" for every such alert, in every
	// namespace — the actual failing object is lost.
	//
	// Pod-scoped alerts are unaffected: kube_pod_* series carry their own
	// `pod` label, the ServiceMonitor honors metric labels over target
	// ones, and none of them set a workload label — so the branch below
	// never fires for them and KubePodCrashLooping keeps resolving to the
	// real service.
	//
	// INVARIANT this relies on: no pod-scoped alert carries a workload
	// label. It holds because the workload labels only exist on
	// kube_<workload>_* series, which have no `pod` of their own. A future
	// VMRule that enriches a pod-scoped series with the owning workload
	// (the `* on(...) group_left(deployment)` join pattern) would break it
	// and silently send that alert here instead of to the pod-derived
	// name. If such a rule is ever added, gate this branch on the alert
	// types it is meant for, the way the branches below gate on tType.
	//
	// Observed 2026-06-21..2026-08-28: 20 incidents recorded against
	// service="monitoring-kube-state-metrics" across tenants admins, labs,
	// nfc, backstage, minio, vault and monitoring — six namespaces that
	// never ran a kube-state-metrics pod. No skill can match those
	// tickets, and the reconciler auto-resolves several as "service does
	// not exist (likely synthetic / orphaned alert)".
	//
	// ROLLOUT NOTE: an alert already firing when this ships keeps its
	// pre-existing ticket under the old pod-derived key, so it gets one
	// new ticket (and one notification) under the corrected name. The old
	// one is closed by reconcileWithAlertManager (poller.go) once its
	// alerts clear. That is deliberate, and it is why no key-migration
	// fallback lives here: those legacy tickets are AGGREGATES. The
	// collapsed key merged every workload in the tenant into one ticket
	// and TouchWithFingerprint accumulated all their fingerprints, so no
	// single webhook can say whether the ticket as a whole is resolved —
	// attaching or closing it on one alert misattributes or prematurely
	// closes the others. AM reconcile is the only place that can decide
	// correctly, because it alone requires ALL of a ticket's fingerprints
	// to be absent from the active set. Earlier revisions of this PR tried
	// the fallback three ways; each one reintroduced a cross-workload
	// resolve.
	for _, key := range []string{"deployment", "statefulset", "daemonset"} {
		if obj := a.Labels[key]; obj != "" {
			service = obj
			break
		}
	}
	if tType == ticket.TypeWorkflowFailed && workflow != "" {
		service = workflow
	}
	if tType == ticket.TypeArgoCDDegraded {
		// ArgoCD app health metrics carry the Application identity in
		// `name` (with optional `dest_namespace` / `project`), not in
		// `pod`. Without this branch every Degraded app collapses onto
		// the same (tenant="", service="") dedup key and
		// collectEvidence skips argocd_status (it gates on service !=
		// ""), so the argocd_sync_failed skill never sees the data it
		// needs to diagnose.
		if app := a.Labels["name"]; app != "" {
			service = app
		}
		if dest := a.Labels["dest_namespace"]; dest != "" {
			tenant = dest
		}
	}

	// Some VMRules (absent() checks with no label matcher, e.g.
	// MctlAgentMetricsAbsent, OpenclawLlmMetricsAbsent) produce alerts with
	// no `namespace` label at all — PromQL's absent() has nothing to
	// inherit labels from when the series doesn't exist. mctl-api's
	// POST /api/v1/incidents rejects an empty tenant as a missing required
	// field (400), so PublishAlert silently drops these tickets (error is
	// logged and swallowed, fire-and-forget) even though they're created
	// fine locally and notified via Telegram. We don't know the real owning
	// tenant here (it varies per alert: mctl-agent's own metrics belong to
	// admins, openclaw's to labs, etc. — see mctl-gitops
	// vm-rules/mctl-agent-cleanup-alerts.yaml and openclaw-llm-alerts.yaml),
	// so fall back to a generic non-empty placeholder rather than guessing
	// wrong. Applied before the resolved-alert branch too, so a later
	// "resolved" webhook for the same alert still matches on the same
	// (tenant, service, type) dedup key.
	if tenant == "" {
		tenant = "platform"
	}

	// Alerts with neither a namespace nor a pod label (the same absent()
	// alerts the tenant fallback above handles) also have service="" here.
	// classifyAlert's default case maps most alertnames to TypeGeneric, so
	// without this, MctlAgentMetricsAbsent and OpenclawLlmMetricsAbsent
	// would both collapse onto the identical dedup/resolve key
	// (tenant="platform", service="", type=generic) — FindDuplicate/
	// ResolveByTenantService key on (tenant, service, type) only, not
	// alertName, so whichever fires first would "win" the ticket and a
	// resolved webhook for either would incorrectly resolve the other's.
	// Falling back service to alertName keeps distinct label-less alerts on
	// distinct keys without touching alerts that already have a real
	// service (e.g. ScrapePoolHasNoTargets, which has namespace set but no
	// pod — untouched since namespace != "" here).
	//
	// Excluded: TypeResourceLimit. NodeHighCPU/NodeHighMemory/
	// NodeDiskPressure/VaultSealed are node- or cluster-level alerts with
	// no namespace/pod either, but isInfraAlert() (pipeline.go) treats
	// "TypeResourceLimit + empty Service" as its signal to route the
	// ticket manual-only instead of auto-fixing it. Giving them a
	// non-empty service here would silently opt them into cpu_throttle's
	// Match() (any TypeResourceLimit ticket whose summary contains "cpu"),
	// which is meant for pod-level CPU limits, not node hardware — found
	// by Codex review on this PR.
	if service == "" && namespace == "" && pod == "" && tType != ticket.TypeResourceLimit {
		service = alertName
	}

	// Resolved alerts → close matching tickets.
	if a.Status == "resolved" {
		// A resolve with no fingerprint would disable the scope below
		// entirely and close every open ticket under (tenant, service,
		// type) — the exact blast radius the fingerprint scope exists to
		// prevent, reachable from an unauthenticated caller because the
		// webhook's bearer token is optional. AlertManager always sets a
		// fingerprint on a resolved alert, so this drops nothing real.
		//
		// Dropped, not failed: an error here would be a 500, and a 500 asks
		// AlertManager to redeliver a payload that no retry can improve.
		// Leaving the ticket open is the safe direction — it is what
		// reconcileWithAlertManager closes on its next pass.
		if a.Fingerprint == "" {
			slog.Warn("resolved alert carries no fingerprint, refusing to resolve by key alone",
				"alertname", alertName, "tenant", tenant, "service", service)
			return nil
		}
		// Scoped by fingerprint AND by the alert's own end time: ServeHTTP
		// can now ask AlertManager to replay a batch, and (tenant, service,
		// type) is coarse enough — several alertnames map to one ticket
		// type — that a replayed resolve could otherwise close a different
		// incident that opened under the same key in between. The
		// fingerprint alone does not close that hole, because it names the
		// alert's label set rather than one occurrence of it: the same alert
		// flapping back to firing carries the identical fingerprint. EndsAt
		// adds the occurrence boundary — a ticket opened after this alert
		// already ended belongs to a later occurrence and is not ours to
		// close. AlertManager always sets endsAt on a resolved alert; a zero
		// value falls through to fingerprint-only scoping rather than
		// resolving nothing.
		ids, err := h.store.ResolveByTenantService(ctx, tenant, service, tType, a.Fingerprint, a.EndsAt)
		if err != nil {
			slog.Error("failed to resolve tickets", "error", err, "tenant", tenant, "service", service)
			return fmt.Errorf("resolve tickets for %s/%s: %w", tenant, service, err)
		}
		// The ("", "") migration fallback that used to live here is gone.
		// It resolved the legacy fully-empty key whenever the rewritten key
		// matched nothing — which, on a replayed batch, is exactly what a
		// successful first attempt looks like. That legacy ticket is an
		// AGGREGATE of every labelless alert (they all collapsed onto one
		// key before the tenant/service fallbacks existed), so closing it
		// on one alert's replay resolves unrelated labelless alerts that
		// are still firing. Same reasoning that removed the workload-key
		// migration in #105: an aggregate can only be closed by
		// reconcileWithAlertManager, which requires ALL of its fingerprints
		// to be absent from the active set.
		slog.Info("resolved tickets for alert",
			"alertname", alertName, "tenant", tenant, "service", service, "count", len(ids))
		if len(ids) > 0 && h.OnResolve != nil {
			h.OnResolve(ids)
		}
		return nil
	}

	// Service-name filter: drop firing alerts for demo/PR-preview services
	// (e.g. openclawpr4, hook-e2e-check) before they create tickets.
	if h.IgnoreService != nil && service != "" && h.IgnoreService.MatchString(service) {
		slog.Info("alert dropped by service filter",
			"alertname", alertName, "tenant", tenant, "service", service)
		return nil
	}

	// Dedup: check for existing open ticket.
	existing, err := h.store.FindDuplicate(ctx, tenant, service, tType)
	if err != nil {
		slog.Error("dedup check failed", "error", err)
		return fmt.Errorf("dedup check for %s/%s: %w", tenant, service, err)
	}
	if existing != nil {
		// Bump UpdatedAt so the stale-ticket GC can distinguish a still-
		// firing alert from one that has stopped firing.
		if err := h.store.TouchWithFingerprint(ctx, existing.ID, a.Fingerprint); err != nil {
			slog.Error("failed to touch ticket on duplicate alert", "error", err, "id", existing.ID)
			return fmt.Errorf("touch ticket %s: %w", existing.ID, err)
		}
		slog.Debug("duplicate ticket exists", "id", existing.ID, "alertname", alertName)
		return nil
	}

	// Flap suppression: if the same alert was just resolved, skip creating
	// a fresh ticket. Prevents Telegram spam from alerts that toggle
	// above/below threshold (e.g. CPU throttling near the limit). The key
	// includes alertName so that two Prometheus alerts mapped to the same
	// ticket type (e.g. TenantCPUQuotaHigh and CPUThrottlingHigh both ->
	// TypeResourceLimit) do not suppress each other.
	if h.FlapCooldown > 0 {
		recent, err := h.store.FindRecentlyResolved(ctx, tenant, service, tType, alertName, h.FlapCooldown)
		if err != nil {
			slog.Error("flap cooldown check failed", "error", err)
			return fmt.Errorf("flap cooldown check for %s/%s: %w", tenant, service, err)
		}
		if recent != nil {
			slog.Info("suppressing flap alert within cooldown",
				"alertname", alertName, "tenant", tenant, "service", service,
				"previous_ticket", recent.ID, "cooldown", h.FlapCooldown)
			return nil
		}
	}

	summary := a.Annotations["summary"]
	if summary == "" {
		summary = alertName + " in " + namespace
	}

	t := &ticket.Ticket{
		Source:           ticket.SourceAlertManager,
		AlertName:        alertName,
		Type:             tType,
		Tenant:           tenant,
		Service:          service,
		Summary:          summary,
		Severity:         severity,
		AlertFingerprint: a.Fingerprint,
	}

	if err := h.store.Create(ctx, t); err != nil {
		slog.Error("failed to create ticket from alert", "error", err, "alertname", alertName)
		return fmt.Errorf("create ticket for %s: %w", alertName, err)
	}

	// Store the raw alert as evidence.
	alertJSON, _ := json.Marshal(a)
	_ = h.store.AddEvidence(ctx, t.ID, ticket.Evidence{
		Type:        "alert",
		Content:     string(alertJSON),
		CollectedAt: time.Now().UTC(),
	})

	slog.Info("ticket created from alert",
		"id", t.ID, "type", tType, "tenant", tenant, "service", service, "severity", severity)

	if h.onTicket != nil {
		h.onTicket(t)
	}
	return nil
}

// classifyAlert maps AlertManager alertname to ticket type and severity.
func classifyAlert(alertName string) (ticketType, severity string) {
	switch alertName {
	case "PodCrashLooping", "KubePodCrashLooping":
		return ticket.TypePodCrashloop, ticket.SeverityCritical
	case "KubePodNotReady", "PodNotReady":
		return ticket.TypePodCrashloop, ticket.SeverityWarning
	case "TenantCPUQuotaHigh", "TenantMemoryQuotaHigh", "CPUThrottlingHigh":
		return ticket.TypeResourceLimit, ticket.SeverityWarning
	case "ArgoWorkflowFailed", "ArgoWorkflowHighFailureRate", "KubeJobNotCompleted":
		return ticket.TypeWorkflowFailed, ticket.SeverityWarning
	case "KubePersistentVolumeFillingUp", "KubeStatefulSetReplicasMismatch":
		return ticket.TypeGeneric, ticket.SeverityWarning
	case "NodeHighCPU", "NodeHighMemory", "NodeDiskPressure":
		return ticket.TypeResourceLimit, ticket.SeverityWarning
	case "VaultSealed":
		return ticket.TypeResourceLimit, ticket.SeverityCritical
	case "ArgoCDApplicationDegraded",
		"ArgoCDApplicationOutOfSyncLong",
		// ArgoCDApplicationSyncFailed is the original (mis-)name from
		// mctl-gitops PR #142 first commit; it was renamed to
		// ArgoCDApplicationOutOfSyncLong after Codex P2 (the alert
		// fires on prolonged OutOfSync drift, not a sync-failure
		// signal). Keep the old name in this switch so the two PRs
		// stay merge-order-independent — drop after both have
		// landed and the chart has rolled out.
		"ArgoCDApplicationSyncFailed":
		return ticket.TypeArgoCDDegraded, ticket.SeverityWarning
	default:
		return ticket.TypeGeneric, ticket.SeverityWarning
	}
}

// extractService extracts the service name from a pod name by stripping
// the ReplicaSet hash and pod suffix (e.g. "myapp-6d4b5c7f8-abc12" → "myapp").
func extractService(pod string) string {
	if pod == "" {
		return ""
	}
	parts := strings.Split(pod, "-")
	if len(parts) <= 2 {
		return pod
	}
	// Strip last two segments (RS hash + pod ID).
	return strings.Join(parts[:len(parts)-2], "-")
}
