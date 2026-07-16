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
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/mctlhq/mctl-agent/internal/ticket"
)

// AlertHandler receives AlertManager webhooks and creates tickets.
type AlertHandler struct {
	store    *ticket.Store
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

// ServeHTTP handles POST /api/v1/alerts.
func (h *AlertHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
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

	for _, a := range payload.Alerts {
		h.processAlert(a)
	}

	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"accepted"}`))
}

func (h *AlertHandler) processAlert(a alert) {
	alertName := a.Labels["alertname"]
	namespace := a.Labels["namespace"]
	pod := a.Labels["pod"]
	workflow := a.Labels["name"]

	tType, severity := classifyAlert(alertName)

	tenant := namespace
	service := extractService(pod)
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
		ids, err := h.store.ResolveByTenantService(tenant, service, tType)
		if err != nil {
			slog.Error("failed to resolve tickets", "error", err, "tenant", tenant, "service", service)
		}
		// Migration fallback: a ticket created before the tenant/service
		// fallbacks above existed may still sit on the old fully-empty
		// ("", "") key. Only tried when this resolve is for a labelless
		// alert (namespace and pod both empty — the case the fallbacks
		// rewrite) and the rewritten key found nothing; becomes a no-op
		// once pre-rollout tickets have aged out or resolved.
		if len(ids) == 0 && namespace == "" && pod == "" {
			legacyIDs, legacyErr := h.store.ResolveByTenantService("", "", tType)
			if legacyErr != nil {
				slog.Error("failed to resolve legacy empty-tenant tickets", "error", legacyErr)
			}
			ids = append(ids, legacyIDs...)
		}
		slog.Info("resolved tickets for alert",
			"alertname", alertName, "tenant", tenant, "service", service, "count", len(ids))
		if len(ids) > 0 && h.OnResolve != nil {
			h.OnResolve(ids)
		}
		return
	}

	// Service-name filter: drop firing alerts for demo/PR-preview services
	// (e.g. openclawpr4, hook-e2e-check) before they create tickets.
	if h.IgnoreService != nil && service != "" && h.IgnoreService.MatchString(service) {
		slog.Info("alert dropped by service filter",
			"alertname", alertName, "tenant", tenant, "service", service)
		return
	}

	// Dedup: check for existing open ticket.
	existing, err := h.store.FindDuplicate(tenant, service, tType)
	if err != nil {
		slog.Error("dedup check failed", "error", err)
	}
	if existing != nil {
		// Bump UpdatedAt so the stale-ticket GC can distinguish a still-
		// firing alert from one that has stopped firing.
		if err := h.store.TouchWithFingerprint(existing.ID, a.Fingerprint); err != nil {
			slog.Error("failed to touch ticket on duplicate alert", "error", err, "id", existing.ID)
		}
		slog.Debug("duplicate ticket exists", "id", existing.ID, "alertname", alertName)
		return
	}

	// Flap suppression: if the same alert was just resolved, skip creating
	// a fresh ticket. Prevents Telegram spam from alerts that toggle
	// above/below threshold (e.g. CPU throttling near the limit). The key
	// includes alertName so that two Prometheus alerts mapped to the same
	// ticket type (e.g. TenantCPUQuotaHigh and CPUThrottlingHigh both ->
	// TypeResourceLimit) do not suppress each other.
	if h.FlapCooldown > 0 {
		recent, err := h.store.FindRecentlyResolved(tenant, service, tType, alertName, h.FlapCooldown)
		if err != nil {
			slog.Error("flap cooldown check failed", "error", err)
		}
		if recent != nil {
			slog.Info("suppressing flap alert within cooldown",
				"alertname", alertName, "tenant", tenant, "service", service,
				"previous_ticket", recent.ID, "cooldown", h.FlapCooldown)
			return
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

	if err := h.store.Create(t); err != nil {
		slog.Error("failed to create ticket from alert", "error", err, "alertname", alertName)
		return
	}

	// Store the raw alert as evidence.
	alertJSON, _ := json.Marshal(a)
	_ = h.store.AddEvidence(t.ID, ticket.Evidence{
		Type:        "alert",
		Content:     string(alertJSON),
		CollectedAt: time.Now().UTC(),
	})

	slog.Info("ticket created from alert",
		"id", t.ID, "type", tType, "tenant", tenant, "service", service, "severity", severity)

	if h.onTicket != nil {
		h.onTicket(t)
	}
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
