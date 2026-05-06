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

	// Resolved alerts → close matching tickets.
	if a.Status == "resolved" {
		if err := h.store.ResolveByTenantService(tenant, service, tType); err != nil {
			slog.Error("failed to resolve tickets", "error", err, "tenant", tenant, "service", service)
		}
		slog.Info("resolved tickets for alert", "alertname", alertName, "tenant", tenant, "service", service)
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
	case "ArgoCDApplicationDegraded", "ArgoCDApplicationSyncFailed":
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
