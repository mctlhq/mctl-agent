package monitor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/mctlhq/mctl-agent/internal/ticket"
	_ "modernc.org/sqlite"
)

// ctx is the context the store calls in this package's tests run under. The
// store API takes one now; nothing here exercises cancellation, so a single
// background context keeps the call sites readable. Tests that need their own
// context shadow this with a local one.
var ctx = context.Background()

func newTestStore(t *testing.T) *ticket.Store {
	t.Helper()
	store, err := ticket.NewStore(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func TestClassifyAlert(t *testing.T) {
	tests := []struct {
		alertName    string
		wantType     string
		wantSeverity string
	}{
		{"PodCrashLooping", ticket.TypePodCrashloop, ticket.SeverityCritical},
		{"KubePodCrashLooping", ticket.TypePodCrashloop, ticket.SeverityCritical},
		{"KubePodNotReady", ticket.TypePodCrashloop, ticket.SeverityWarning},
		{"PodNotReady", ticket.TypePodCrashloop, ticket.SeverityWarning},
		{"TenantCPUQuotaHigh", ticket.TypeResourceLimit, ticket.SeverityWarning},
		{"TenantMemoryQuotaHigh", ticket.TypeResourceLimit, ticket.SeverityWarning},
		{"CPUThrottlingHigh", ticket.TypeResourceLimit, ticket.SeverityWarning},
		{"ArgoWorkflowFailed", ticket.TypeWorkflowFailed, ticket.SeverityWarning},
		{"KubeJobNotCompleted", ticket.TypeWorkflowFailed, ticket.SeverityWarning},
		{"KubePersistentVolumeFillingUp", ticket.TypeGeneric, ticket.SeverityWarning},
		{"KubeStatefulSetReplicasMismatch", ticket.TypeGeneric, ticket.SeverityWarning},
		{"VaultSealed", ticket.TypeResourceLimit, ticket.SeverityCritical},
		{"NodeHighCPU", ticket.TypeResourceLimit, ticket.SeverityWarning},
		{"ArgoCDApplicationDegraded", ticket.TypeArgoCDDegraded, ticket.SeverityWarning},
		{"ArgoCDApplicationOutOfSyncLong", ticket.TypeArgoCDDegraded, ticket.SeverityWarning},
		{"ArgoCDApplicationSyncFailed", ticket.TypeArgoCDDegraded, ticket.SeverityWarning}, // legacy name pre-rename
		{"UnknownAlert", ticket.TypeGeneric, ticket.SeverityWarning},
	}

	for _, tt := range tests {
		t.Run(tt.alertName, func(t *testing.T) {
			gotType, gotSeverity := classifyAlert(tt.alertName)
			if gotType != tt.wantType {
				t.Errorf("classifyAlert(%q) type = %q, want %q", tt.alertName, gotType, tt.wantType)
			}
			if gotSeverity != tt.wantSeverity {
				t.Errorf("classifyAlert(%q) severity = %q, want %q", tt.alertName, gotSeverity, tt.wantSeverity)
			}
		})
	}
}

func TestExtractService(t *testing.T) {
	tests := []struct {
		pod  string
		want string
	}{
		{"myapp-6d4b5c7f8-abc12", "myapp"},
		{"payment-api-7f8d9e-xyz99", "payment-api"},
		{"single", "single"},
		{"two-parts", "two-parts"},
		{"", ""},
		{"a-b-c-d-e", "a-b-c"},
	}

	for _, tt := range tests {
		t.Run(tt.pod, func(t *testing.T) {
			got := extractService(tt.pod)
			if got != tt.want {
				t.Errorf("extractService(%q) = %q, want %q", tt.pod, got, tt.want)
			}
		})
	}
}

func TestAlertHandlerServeHTTP(t *testing.T) {
	store := newTestStore(t)

	var received []*ticket.Ticket
	handler := NewAlertHandler(store, func(tk *ticket.Ticket) {
		received = append(received, tk)
	})

	payload := alertManagerPayload{
		Status: "firing",
		Alerts: []alert{
			{
				Status: "firing",
				Labels: map[string]string{
					"alertname": "PodCrashLooping",
					"namespace": "billing",
					"pod":       "api-6d4b5c7f8-abc12",
				},
				Annotations: map[string]string{
					"summary": "Pod billing/api-6d4b5c7f8-abc12 is crash looping",
				},
			},
		},
	}

	body, _ := json.Marshal(payload)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/alerts", bytes.NewReader(body))
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
	if len(received) != 1 {
		t.Fatalf("expected 1 ticket callback, got %d", len(received))
	}
	if received[0].Tenant != "billing" {
		t.Errorf("expected tenant billing, got %s", received[0].Tenant)
	}
	if received[0].Service != "api" {
		t.Errorf("expected service api, got %s", received[0].Service)
	}
	if received[0].Type != ticket.TypePodCrashloop {
		t.Errorf("expected type %s, got %s", ticket.TypePodCrashloop, received[0].Type)
	}
	if received[0].Severity != ticket.SeverityCritical {
		t.Errorf("expected severity critical, got %s", received[0].Severity)
	}
	if received[0].AlertName != "PodCrashLooping" {
		t.Errorf("expected alert name PodCrashLooping, got %s", received[0].AlertName)
	}
}

func TestAlertHandlerEmptyNamespaceTenantFallback(t *testing.T) {
	// absent()-style VMRules (MctlAgentMetricsAbsent, OpenclawLlmMetricsAbsent)
	// fire with no `namespace` label at all — there's no series to inherit
	// one from. mctl-api rejects an empty tenant as a missing required
	// field, so these tickets must get a non-empty placeholder tenant to
	// survive PublishAlert.
	store := newTestStore(t)

	var received []*ticket.Ticket
	handler := NewAlertHandler(store, func(tk *ticket.Ticket) {
		received = append(received, tk)
	})

	payload := alertManagerPayload{
		Status: "firing",
		Alerts: []alert{
			{
				Status: "firing",
				Labels: map[string]string{
					"alertname": "MctlAgentMetricsAbsent",
				},
				Annotations: map[string]string{
					"summary": "mctl_agent_open_tickets gauge series missing for 30m",
				},
			},
		},
	}

	body, _ := json.Marshal(payload)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/alerts", bytes.NewReader(body))
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if len(received) != 1 {
		t.Fatalf("expected 1 ticket callback, got %d", len(received))
	}
	if received[0].Tenant != "platform" {
		t.Errorf("expected tenant fallback to %q, got %q", "platform", received[0].Tenant)
	}
	if received[0].Service != "MctlAgentMetricsAbsent" {
		t.Errorf("expected service fallback to alertName %q, got %q", "MctlAgentMetricsAbsent", received[0].Service)
	}
}

func TestAlertHandlerLeavesLegacyEmptyTenantTicketToReconcile(t *testing.T) {
	// Was TestAlertHandlerResolvesLegacyEmptyTenantTicket, which asserted
	// the opposite: a resolved webhook that matched nothing under the
	// rewritten ("platform", alertName) key fell back to the legacy
	// fully-empty ("", "") key and closed whatever sat there.
	//
	// That fallback is removed, because ServeHTTP can now ask AlertManager
	// to replay a batch and "the rewritten key matched nothing" is exactly
	// what a successful first attempt looks like on replay. The legacy
	// ticket is an AGGREGATE — every labelless alert collapsed onto that
	// one key before the tenant/service fallbacks existed — so closing it
	// on one alert's replay resolves unrelated labelless alerts that are
	// still firing (agy P1 on PR #106; same aggregate reasoning that
	// removed the workload-key migration in #105).
	//
	// Aggregates are reconcileWithAlertManager's job: it closes a ticket
	// only when ALL of its fingerprints are absent from the active set.
	store := newTestStore(t)
	legacy := &ticket.Ticket{
		Source:    ticket.SourceAlertManager,
		AlertName: "MctlAgentMetricsAbsent",
		Type:      ticket.TypeGeneric,
		Tenant:    "",
		Service:   "",
		Summary:   "mctl_agent_open_tickets gauge series missing for 30m",
		Severity:  ticket.SeverityWarning,
	}
	if err := store.Create(ctx, legacy); err != nil {
		t.Fatalf("failed to seed legacy ticket: %v", err)
	}

	var resolvedIDs []string
	handler := NewAlertHandler(store, nil)
	handler.OnResolve = func(ids []string) { resolvedIDs = append(resolvedIDs, ids...) }

	payload := alertManagerPayload{
		Status: "resolved",
		Alerts: []alert{
			{
				Status:      "resolved",
				Fingerprint: "aaaa1111",
				Labels:      map[string]string{"alertname": "MctlAgentMetricsAbsent"},
				Annotations: map[string]string{"summary": "mctl_agent_open_tickets gauge series missing for 30m"},
			},
		},
	}

	body, _ := json.Marshal(payload)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/alerts", bytes.NewReader(body))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status: got %d, want %d", w.Code, http.StatusOK)
	}
	if len(resolvedIDs) != 0 {
		t.Errorf("expected the aggregate legacy ticket to be left alone, resolved %v", resolvedIDs)
	}
	got, err := store.Get(ctx, legacy.ID)
	if err != nil {
		t.Fatalf("failed to reload ticket: %v", err)
	}
	if got.Status == ticket.StatusResolved {
		t.Error("aggregate legacy ticket was closed by one alert's resolve")
	}
}
func TestAlertHandlerLabellessAlertsDoNotCollide(t *testing.T) {
	// Two distinct absent()-style alerts with no namespace/pod label both
	// fall through classifyAlert's default (TypeGeneric). Without the
	// service=alertName fallback they'd share the same
	// (tenant="platform", service="", type=generic) dedup key and the
	// second would be silently treated as a duplicate of the first.
	store := newTestStore(t)

	var received []*ticket.Ticket
	handler := NewAlertHandler(store, func(tk *ticket.Ticket) {
		received = append(received, tk)
	})

	payload := alertManagerPayload{
		Status: "firing",
		Alerts: []alert{
			{
				Status:      "firing",
				Labels:      map[string]string{"alertname": "MctlAgentMetricsAbsent"},
				Annotations: map[string]string{"summary": "mctl_agent_open_tickets gauge series missing for 30m"},
			},
			{
				Status:      "firing",
				Labels:      map[string]string{"alertname": "OpenclawLlmMetricsAbsent"},
				Annotations: map[string]string{"summary": "openclaw_llm_* metric series missing for 30m"},
			},
		},
	}

	body, _ := json.Marshal(payload)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/alerts", bytes.NewReader(body))
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if len(received) != 2 {
		t.Fatalf("expected 2 distinct tickets, got %d", len(received))
	}
	if received[0].Service == received[1].Service {
		t.Errorf("expected distinct services to avoid dedup collision, both got %q", received[0].Service)
	}
}

func TestAlertHandlerNodeInfraAlertsStayServiceLess(t *testing.T) {
	// NodeHighCPU (and its siblings NodeHighMemory/NodeDiskPressure/
	// VaultSealed) have no namespace/pod label either, but they classify
	// as TypeResourceLimit, where isInfraAlert() (pipeline.go) treats an
	// empty Service as the signal to route the ticket manual-only instead
	// of matching it to cpu_throttle's auto-fix path. The alertName
	// fallback must not override that.
	store := newTestStore(t)

	var received []*ticket.Ticket
	handler := NewAlertHandler(store, func(tk *ticket.Ticket) {
		received = append(received, tk)
	})

	payload := alertManagerPayload{
		Status: "firing",
		Alerts: []alert{
			{
				Status:      "firing",
				Labels:      map[string]string{"alertname": "NodeHighCPU"},
				Annotations: map[string]string{"summary": "node cpu high"},
			},
		},
	}

	body, _ := json.Marshal(payload)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/alerts", bytes.NewReader(body))
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if len(received) != 1 {
		t.Fatalf("expected 1 ticket, got %d", len(received))
	}
	if received[0].Service != "" {
		t.Errorf("expected NodeHighCPU to stay service-less (isInfraAlert gate), got %q", received[0].Service)
	}
	if received[0].Type != ticket.TypeResourceLimit {
		t.Fatalf("test setup: expected TypeResourceLimit, got %q", received[0].Type)
	}
}

func TestAlertHandlerArgoCDLabels(t *testing.T) {
	// ArgoCD app health alerts must extract the application name from
	// `name` (and namespace from `dest_namespace`) instead of the
	// generic `pod` path. Without this each Degraded application
	// collapses onto a single (tenant="", service="") dedup key and
	// pipeline.collectEvidence skips argocd_status (gated on
	// service != ""), preventing the argocd_sync_failed skill from
	// diagnosing the actual failing app.
	store := newTestStore(t)
	var received []*ticket.Ticket
	handler := NewAlertHandler(store, func(tk *ticket.Ticket) {
		received = append(received, tk)
	})

	payload := alertManagerPayload{
		Status: "firing",
		Alerts: []alert{
			{
				Status: "firing",
				Labels: map[string]string{
					"alertname":      "ArgoCDApplicationDegraded",
					"namespace":      "argocd",
					"name":           "tenant-labs",
					"dest_namespace": "labs",
					"project":        "platform",
				},
				Annotations: map[string]string{"summary": "tenant-labs Degraded"},
			},
		},
	}
	body, _ := json.Marshal(payload)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/alerts", bytes.NewReader(body))
	handler.ServeHTTP(httptest.NewRecorder(), req)

	if len(received) != 1 {
		t.Fatalf("expected 1 ticket, got %d", len(received))
	}
	if received[0].Service != "tenant-labs" {
		t.Errorf("service: got %q, want %q", received[0].Service, "tenant-labs")
	}
	if received[0].Tenant != "labs" {
		t.Errorf("tenant: got %q, want %q (dest_namespace label)", received[0].Tenant, "labs")
	}
	if received[0].Type != ticket.TypeArgoCDDegraded {
		t.Errorf("type: got %q, want %q", received[0].Type, ticket.TypeArgoCDDegraded)
	}
}

func TestAlertHandlerDedup(t *testing.T) {
	store := newTestStore(t)

	callCount := 0
	handler := NewAlertHandler(store, func(tk *ticket.Ticket) {
		callCount++
	})

	payload := alertManagerPayload{
		Alerts: []alert{
			{
				Status: "firing",
				Labels: map[string]string{
					"alertname": "PodCrashLooping",
					"namespace": "billing",
					"pod":       "api-6d4b5c7f8-abc12",
				},
			},
		},
	}
	body, _ := json.Marshal(payload)

	// Send twice.
	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/alerts", bytes.NewReader(body))
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
	}

	if callCount != 1 {
		t.Errorf("expected 1 callback (dedup), got %d", callCount)
	}
}

func TestAlertHandlerResolvedAlert(t *testing.T) {
	store := newTestStore(t)

	handler := NewAlertHandler(store, func(tk *ticket.Ticket) {})

	// First: fire alert.
	fire := alertManagerPayload{
		Alerts: []alert{
			{
				Status:      "firing",
				Fingerprint: "fp-podcrash-1111",
				Labels: map[string]string{
					"alertname": "PodCrashLooping",
					"namespace": "billing",
					"pod":       "api-6d4b5c7f8-abc12",
				},
			},
		},
	}
	body, _ := json.Marshal(fire)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/alerts", bytes.NewReader(body))
	handler.ServeHTTP(httptest.NewRecorder(), req)

	// Verify ticket exists.
	open, _ := store.ListOpen(ctx)
	if len(open) != 1 {
		t.Fatalf("expected 1 open ticket, got %d", len(open))
	}

	// Then: resolve alert.
	resolve := alertManagerPayload{
		Alerts: []alert{
			{
				Status:      "resolved",
				Fingerprint: "fp-podcrash-1111",
				Labels: map[string]string{
					"alertname": "PodCrashLooping",
					"namespace": "billing",
					"pod":       "api-6d4b5c7f8-abc12",
				},
			},
		},
	}
	body, _ = json.Marshal(resolve)
	req = httptest.NewRequest(http.MethodPost, "/api/v1/alerts", bytes.NewReader(body))
	handler.ServeHTTP(httptest.NewRecorder(), req)

	// Verify ticket is resolved.
	open, _ = store.ListOpen(ctx)
	if len(open) != 0 {
		t.Errorf("expected 0 open tickets after resolve, got %d", len(open))
	}
}

func TestAlertHandlerResolveFiresOnResolve(t *testing.T) {
	store := newTestStore(t)
	handler := NewAlertHandler(store, func(tk *ticket.Ticket) {})

	var resolvedIDs []string
	handler.OnResolve = func(ids []string) {
		resolvedIDs = append(resolvedIDs, ids...)
	}

	// Pre-existing open ticket matching the resolve key.
	tk := &ticket.Ticket{
		Source:  ticket.SourceAlertManager,
		Type:    ticket.TypePodCrashloop,
		Tenant:  "billing",
		Service: "api",
	}
	if err := store.Create(ctx, tk); err != nil {
		t.Fatal(err)
	}

	resolve := alertManagerPayload{
		Alerts: []alert{
			{
				Status:      "resolved",
				Fingerprint: "fp-podcrash-1111",
				Labels: map[string]string{
					"alertname": "PodCrashLooping",
					"namespace": "billing",
					"pod":       "api-6d4b5c7f8-abc12",
				},
			},
		},
	}
	body, _ := json.Marshal(resolve)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/alerts", bytes.NewReader(body))
	handler.ServeHTTP(httptest.NewRecorder(), req)

	if len(resolvedIDs) != 1 || resolvedIDs[0] != tk.ID {
		t.Errorf("expected OnResolve called with [%s], got %v", tk.ID, resolvedIDs)
	}
}

func TestAlertHandlerFiringDoesNotFireOnResolve(t *testing.T) {
	store := newTestStore(t)
	handler := NewAlertHandler(store, func(tk *ticket.Ticket) {})

	called := false
	handler.OnResolve = func(ids []string) { called = true }

	fire := alertManagerPayload{
		Alerts: []alert{
			{
				Status: "firing",
				Labels: map[string]string{
					"alertname": "PodCrashLooping",
					"namespace": "billing",
					"pod":       "api-6d4b5c7f8-abc12",
				},
			},
		},
	}
	body, _ := json.Marshal(fire)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/alerts", bytes.NewReader(body))
	handler.ServeHTTP(httptest.NewRecorder(), req)

	if called {
		t.Error("OnResolve must not fire on a firing alert")
	}
}

func TestAlertHandlerResolveWithNoMatchSkipsOnResolve(t *testing.T) {
	store := newTestStore(t)
	handler := NewAlertHandler(store, func(tk *ticket.Ticket) {})

	called := false
	handler.OnResolve = func(ids []string) { called = true }

	// No pre-existing tickets; resolve hits an empty match set.
	resolve := alertManagerPayload{
		Alerts: []alert{
			{
				Status: "resolved",
				Labels: map[string]string{
					"alertname": "PodCrashLooping",
					"namespace": "ghost",
					"pod":       "missing-6d4b5c7f8-xxxxx",
				},
			},
		},
	}
	body, _ := json.Marshal(resolve)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/alerts", bytes.NewReader(body))
	handler.ServeHTTP(httptest.NewRecorder(), req)

	if called {
		t.Error("OnResolve must not fire when no tickets match the resolve key")
	}
}

func TestAlertHandlerInvalidJSON(t *testing.T) {
	store := newTestStore(t)
	handler := NewAlertHandler(store, nil)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/alerts", bytes.NewReader([]byte("not json")))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

func TestAlertHandlerFlapCooldown(t *testing.T) {
	store := newTestStore(t)

	callCount := 0
	handler := NewAlertHandler(store, func(tk *ticket.Ticket) {
		callCount++
	})
	handler.FlapCooldown = 10 * time.Minute

	fire := alertManagerPayload{
		Alerts: []alert{
			{
				Status:      "firing",
				Fingerprint: "fp-cpu-throttle-2222",
				Labels: map[string]string{
					"alertname": "CPUThrottlingHigh",
					"namespace": "platform-db",
					"pod":       "shared-pg-1",
				},
			},
		},
	}
	body, _ := json.Marshal(fire)
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/api/v1/alerts", bytes.NewReader(body)))

	resolve := alertManagerPayload{
		Alerts: []alert{
			{
				Status:      "resolved",
				Fingerprint: "fp-cpu-throttle-2222",
				Labels: map[string]string{
					"alertname": "CPUThrottlingHigh",
					"namespace": "platform-db",
					"pod":       "shared-pg-1",
				},
			},
		},
	}
	body, _ = json.Marshal(resolve)
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/api/v1/alerts", bytes.NewReader(body)))

	// Re-fire immediately — should be suppressed by cooldown.
	body, _ = json.Marshal(fire)
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/api/v1/alerts", bytes.NewReader(body)))

	if callCount != 1 {
		t.Errorf("expected 1 callback with flap cooldown active, got %d", callCount)
	}
}

func TestAlertHandlerFlapCooldownKeyedByAlertName(t *testing.T) {
	// Two distinct Prometheus alerts (CPUThrottlingHigh and
	// TenantCPUQuotaHigh) both classify as TypeResourceLimit. When one
	// resolves within the cooldown window, the other must still be able
	// to open a ticket — they are independent incidents even though
	// they share a ticket type.
	store := newTestStore(t)

	callCount := 0
	handler := NewAlertHandler(store, func(tk *ticket.Ticket) {
		callCount++
	})
	handler.FlapCooldown = 10 * time.Minute

	fireThrottling := alertManagerPayload{
		Alerts: []alert{
			{
				Status:      "firing",
				Fingerprint: "fp-cpu-throttle-2222",
				Labels: map[string]string{
					"alertname": "CPUThrottlingHigh",
					"namespace": "platform-db",
					"pod":       "shared-pg-1",
				},
			},
		},
	}
	resolveThrottling := alertManagerPayload{
		Alerts: []alert{
			{
				Status:      "resolved",
				Fingerprint: "fp-cpu-throttle-2222",
				Labels: map[string]string{
					"alertname": "CPUThrottlingHigh",
					"namespace": "platform-db",
					"pod":       "shared-pg-1",
				},
			},
		},
	}
	fireQuota := alertManagerPayload{
		Alerts: []alert{
			{
				Status:      "firing",
				Fingerprint: "fp-tenant-quota-3333",
				Labels: map[string]string{
					"alertname": "TenantCPUQuotaHigh",
					"namespace": "platform-db",
					"pod":       "shared-pg-1",
				},
			},
		},
	}

	body, _ := json.Marshal(fireThrottling)
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/api/v1/alerts", bytes.NewReader(body)))
	body, _ = json.Marshal(resolveThrottling)
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/api/v1/alerts", bytes.NewReader(body)))
	body, _ = json.Marshal(fireQuota)
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/api/v1/alerts", bytes.NewReader(body)))

	if callCount != 2 {
		t.Errorf("expected 2 callbacks (different alertnames are independent), got %d", callCount)
	}
}

func TestAlertHandlerFlapCooldownDisabled(t *testing.T) {
	store := newTestStore(t)

	callCount := 0
	handler := NewAlertHandler(store, func(tk *ticket.Ticket) {
		callCount++
	})
	// FlapCooldown defaults to zero — disabled.

	fire := alertManagerPayload{
		Alerts: []alert{
			{
				Status:      "firing",
				Fingerprint: "fp-cpu-throttle-2222",
				Labels: map[string]string{
					"alertname": "CPUThrottlingHigh",
					"namespace": "platform-db",
					"pod":       "shared-pg-1",
				},
			},
		},
	}
	resolve := alertManagerPayload{
		Alerts: []alert{
			{
				Status:      "resolved",
				Fingerprint: "fp-cpu-throttle-2222",
				Labels: map[string]string{
					"alertname": "CPUThrottlingHigh",
					"namespace": "platform-db",
					"pod":       "shared-pg-1",
				},
			},
		},
	}

	body, _ := json.Marshal(fire)
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/api/v1/alerts", bytes.NewReader(body)))
	body, _ = json.Marshal(resolve)
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/api/v1/alerts", bytes.NewReader(body)))
	body, _ = json.Marshal(fire)
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/api/v1/alerts", bytes.NewReader(body)))

	if callCount != 2 {
		t.Errorf("expected 2 callbacks without cooldown, got %d", callCount)
	}
}

func TestAlertHandlerUnknownAlert(t *testing.T) {
	store := newTestStore(t)

	callCount := 0
	handler := NewAlertHandler(store, func(tk *ticket.Ticket) {
		callCount++
	})

	payload := alertManagerPayload{
		Alerts: []alert{
			{
				Status: "firing",
				Labels: map[string]string{
					"alertname": "SomeCustomAlert",
					"namespace": "test",
				},
			},
		},
	}
	body, _ := json.Marshal(payload)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/alerts", bytes.NewReader(body))
	handler.ServeHTTP(httptest.NewRecorder(), req)

	// Unknown alerts are now routed as TypeGeneric — callback must fire.
	if callCount != 1 {
		t.Errorf("expected 1 callback for unknown alert (generic routing), got %d", callCount)
	}
}

func TestAlertHandlerIgnoreServiceFilter(t *testing.T) {
	store := newTestStore(t)

	callCount := 0
	handler := NewAlertHandler(store, func(tk *ticket.Ticket) { callCount++ })
	handler.IgnoreService = regexp.MustCompile(`^(openclawpr\d+|hooktest-.*)$`)

	cases := []struct {
		name        string
		pod         string
		wantDropped bool
	}{
		{"matches openclawprN", "openclawpr7-6d4b5c7f8-abc12", true},
		{"matches hooktest", "hooktest-service-6d4b5c7f8-abc12", true},
		{"non-matching service creates ticket", "api-6d4b5c7f8-abc12", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			callCount = 0
			// Use a unique tenant per case so dedup does not hide drops
			// between sub-tests (dedup keys on tenant+service+type).
			ns := "labs-" + tc.name[:4]
			payload := alertManagerPayload{
				Alerts: []alert{
					{
						Status: "firing",
						Labels: map[string]string{
							"alertname": "PodCrashLooping",
							"namespace": ns,
							"pod":       tc.pod,
						},
					},
				},
			}
			body, _ := json.Marshal(payload)
			req := httptest.NewRequest(http.MethodPost, "/api/v1/alerts", bytes.NewReader(body))
			handler.ServeHTTP(httptest.NewRecorder(), req)

			if tc.wantDropped && callCount != 0 {
				t.Errorf("expected ticket to be dropped by filter, got %d callbacks", callCount)
			}
			if !tc.wantDropped && callCount != 1 {
				t.Errorf("expected 1 ticket callback, got %d", callCount)
			}
		})
	}
}

func TestAlertHandlerIgnoreFilterSkipsResolve(t *testing.T) {
	// A resolved alert must still close existing tickets even if the
	// service name matches the ignore filter — otherwise any ticket that
	// was created before the filter was added would be stuck forever.
	store := newTestStore(t)
	handler := NewAlertHandler(store, func(tk *ticket.Ticket) {})

	// Create a ticket directly (simulating one created before filter existed).
	t0 := &ticket.Ticket{
		Source:   ticket.SourceAlertManager,
		Type:     ticket.TypePodCrashloop,
		Tenant:   "labs",
		Service:  "hooktest-service",
		Summary:  "legacy",
		Severity: ticket.SeverityCritical,
	}
	if err := store.Create(ctx, t0); err != nil {
		t.Fatal(err)
	}

	// Enable filter.
	handler.IgnoreService = regexp.MustCompile(`^hooktest-.*$`)

	// Send a resolved alert for the filtered service.
	resolve := alertManagerPayload{
		Alerts: []alert{
			{
				Status:      "resolved",
				Fingerprint: "fp-podcrash-1111",
				Labels: map[string]string{
					"alertname": "PodCrashLooping",
					"namespace": "labs",
					"pod":       "hooktest-service-6d4b5c7f8-abc12",
				},
			},
		},
	}
	body, _ := json.Marshal(resolve)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/alerts", bytes.NewReader(body))
	handler.ServeHTTP(httptest.NewRecorder(), req)

	open, _ := store.ListOpen(ctx)
	if len(open) != 0 {
		t.Errorf("expected legacy ticket resolved despite filter, still open: %d", len(open))
	}
}

func TestAlertHandlerDedupBumpsUpdatedAt(t *testing.T) {
	store := newTestStore(t)
	handler := NewAlertHandler(store, func(tk *ticket.Ticket) {})

	payload := alertManagerPayload{
		Alerts: []alert{
			{
				Status: "firing",
				Labels: map[string]string{
					"alertname": "PodCrashLooping",
					"namespace": "billing",
					"pod":       "api-6d4b5c7f8-abc12",
				},
			},
		},
	}
	body, _ := json.Marshal(payload)

	// First fire: creates ticket.
	req := httptest.NewRequest(http.MethodPost, "/api/v1/alerts", bytes.NewReader(body))
	handler.ServeHTTP(httptest.NewRecorder(), req)

	open, _ := store.ListOpen(ctx)
	if len(open) != 1 {
		t.Fatalf("expected 1 ticket, got %d", len(open))
	}
	firstUpdated := open[0].UpdatedAt

	// Force a gap, then fire the duplicate. Touch should advance UpdatedAt.
	time.Sleep(20 * time.Millisecond)
	req = httptest.NewRequest(http.MethodPost, "/api/v1/alerts", bytes.NewReader(body))
	handler.ServeHTTP(httptest.NewRecorder(), req)

	open, _ = store.ListOpen(ctx)
	if len(open) != 1 {
		t.Fatalf("expected 1 ticket after dup, got %d", len(open))
	}
	if !open[0].UpdatedAt.After(firstUpdated) {
		t.Errorf("expected UpdatedAt to advance on duplicate alert; was %v, is %v",
			firstUpdated, open[0].UpdatedAt)
	}
}

func TestAlertHandlerPersistsFingerprint(t *testing.T) {
	store := newTestStore(t)
	h := NewAlertHandler(store, nil)

	payload := `{
		"status": "firing",
		"alerts": [{
			"fingerprint": "deadbeef12345678",
			"status": "firing",
			"labels": {"alertname": "PodCrashLooping", "namespace": "labs", "pod": "myapp-abc-xyz"},
			"annotations": {},
			"startsAt": "2026-01-01T00:00:00Z",
			"endsAt": "0001-01-01T00:00:00Z"
		}]
	}`

	req := httptest.NewRequest(http.MethodPost, "/api/v1/alerts", bytes.NewBufferString(payload))
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, req)

	if rw.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rw.Code)
	}

	tickets, err := store.ListOpen(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(tickets) != 1 {
		t.Fatalf("want 1 ticket, got %d", len(tickets))
	}
	if tickets[0].AlertFingerprint != "deadbeef12345678" {
		t.Errorf("want fingerprint %q, got %q", "deadbeef12345678", tickets[0].AlertFingerprint)
	}
}

func TestAlertHandlerWorkloadLabelBeatsScrapeTargetPod(t *testing.T) {
	// Workload alerts derived from kube-state-metrics carry the scrape
	// target's own `pod` label (the kube-state-metrics pod), because the
	// kube_deployment_* / kube_statefulset_* series have no `pod` label of
	// their own. Extracting the service from `pod` therefore files every
	// such alert under "monitoring-kube-state-metrics" and loses the
	// object that actually broke — 20 incidents between 2026-06-21 and
	// 2026-08-28, in namespaces that never ran a kube-state-metrics pod.
	tests := []struct {
		name        string
		labels      map[string]string
		wantService string
		wantTenant  string
	}{
		{
			name: "deployment label wins over scrape target pod",
			labels: map[string]string{
				"alertname":  "KubeDeploymentReplicasMismatch",
				"namespace":  "admins",
				"deployment": "admins-mctl-agents-worker-base-service",
				"pod":        "monitoring-kube-state-metrics-7c9d4f8b6-abc12",
				"service":    "monitoring-kube-state-metrics",
			},
			wantService: "admins-mctl-agents-worker-base-service",
			wantTenant:  "admins",
		},
		{
			name: "statefulset label wins over scrape target pod",
			labels: map[string]string{
				"alertname":   "KubeStatefulSetReplicasMismatch",
				"namespace":   "vault",
				"statefulset": "vault",
				"pod":         "monitoring-kube-state-metrics-7c9d4f8b6-abc12",
			},
			wantService: "vault",
			wantTenant:  "vault",
		},
		{
			name: "daemonset label wins over scrape target pod",
			labels: map[string]string{
				"alertname": "KubeDaemonSetNotScheduled",
				"namespace": "monitoring",
				"daemonset": "node-exporter",
				"pod":       "monitoring-kube-state-metrics-7c9d4f8b6-abc12",
			},
			wantService: "node-exporter",
			wantTenant:  "monitoring",
		},
		{
			name: "pod-scoped alert keeps its own pod-derived service",
			labels: map[string]string{
				"alertname": "KubePodCrashLooping",
				"namespace": "admins",
				"pod":       "admins-mctl-agents-worker-base-service-6d4b5c7f8-abc12",
			},
			wantService: "admins-mctl-agents-worker-base-service",
			wantTenant:  "admins",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := newTestStore(t)
			var received []*ticket.Ticket
			handler := NewAlertHandler(store, func(tk *ticket.Ticket) {
				received = append(received, tk)
			})

			payload := alertManagerPayload{
				Status: "firing",
				Alerts: []alert{{
					Status:      "firing",
					Labels:      tt.labels,
					Annotations: map[string]string{"summary": "replicas mismatch"},
				}},
			}
			body, _ := json.Marshal(payload)
			req := httptest.NewRequest(http.MethodPost, "/api/v1/alerts", bytes.NewReader(body))
			handler.ServeHTTP(httptest.NewRecorder(), req)

			if len(received) != 1 {
				t.Fatalf("expected 1 ticket, got %d", len(received))
			}
			if received[0].Service != tt.wantService {
				t.Errorf("service: got %q, want %q", received[0].Service, tt.wantService)
			}
			if received[0].Tenant != tt.wantTenant {
				t.Errorf("tenant: got %q, want %q", received[0].Tenant, tt.wantTenant)
			}
		})
	}
}

func TestAlertHandlerRolloutWindowOpensOneNewTicket(t *testing.T) {
	// Documents the accepted bound of this change. An alert already
	// firing when it ships keeps its pre-existing ticket under the old
	// pod-derived key and gets ONE new ticket under the corrected name;
	// the old one is left for reconcileWithAlertManager to close once all
	// of its fingerprints clear.
	//
	// No key-migration fallback is attempted precisely because those
	// legacy tickets are aggregates: the collapsed key merged several
	// workloads into one ticket and TouchWithFingerprint accumulated all
	// of their fingerprints, so resolving on one alert's webhook would
	// close incidents that are still firing. This test pins the aggregate
	// case — two workloads on one legacy ticket — so a future migration
	// attempt has to confront it. See PR #105 review rounds 3 and 4.
	ksmPod := "monitoring-kube-state-metrics-7c9d4f8b6-abc12"
	post := func(t *testing.T, h *AlertHandler, status, fp string, labels map[string]string) {
		t.Helper()
		body, _ := json.Marshal(alertManagerPayload{
			Status: status,
			Alerts: []alert{{
				Status:      status,
				Fingerprint: fp,
				Labels:      labels,
				Annotations: map[string]string{"summary": "replicas mismatch"},
			}},
		})
		req := httptest.NewRequest(http.MethodPost, "/api/v1/alerts", bytes.NewReader(body))
		h.ServeHTTP(httptest.NewRecorder(), req)
	}

	store := newTestStore(t)
	var received []*ticket.Ticket
	handler := NewAlertHandler(store, func(tk *ticket.Ticket) {
		received = append(received, tk)
	})

	// Pre-rollout: workloads A and B collapse onto ONE ticket under the
	// shared kube-state-metrics key, which accumulates both fingerprints.
	preLabels := map[string]string{
		"alertname": "KubeDeploymentReplicasMismatch",
		"namespace": "admins",
		"pod":       ksmPod,
	}
	post(t, handler, "firing", "aaaa1111", preLabels)
	post(t, handler, "firing", "bbbb2222", preLabels)
	if len(received) != 1 {
		t.Fatalf("setup: expected the collapsed key to yield 1 ticket, got %d", len(received))
	}
	legacyID := received[0].ID
	stored, err := store.Get(ctx, legacyID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !strings.Contains(stored.AlertFingerprint, "aaaa1111") || !strings.Contains(stored.AlertFingerprint, "bbbb2222") {
		t.Fatalf("setup: expected an aggregate ticket, fingerprints=%q", stored.AlertFingerprint)
	}

	// Post-rollout: A fires again, now correctly named.
	post(t, handler, "firing", "aaaa1111", map[string]string{
		"alertname":  "KubeDeploymentReplicasMismatch",
		"namespace":  "admins",
		"deployment": "admins-mctl-agents-worker-base-service",
		"pod":        ksmPod,
	})
	if len(received) != 2 {
		t.Fatalf("expected one new ticket under the corrected name, got %d total", len(received))
	}
	if received[1].Service != "admins-mctl-agents-worker-base-service" {
		t.Errorf("new ticket service: got %q, want the workload name", received[1].Service)
	}

	// A's resolve must not close the aggregate: B is still firing in it.
	post(t, handler, "resolved", "aaaa1111", map[string]string{
		"alertname":  "KubeDeploymentReplicasMismatch",
		"namespace":  "admins",
		"deployment": "admins-mctl-agents-worker-base-service",
		"pod":        ksmPod,
	})
	legacy, err := store.Get(ctx, legacyID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if legacy.Status == ticket.StatusResolved {
		t.Error("the aggregate legacy ticket was closed by one workload's resolve; B is still firing in it")
	}
}

func TestAlertHandlerStoreFailureAsksForRetry(t *testing.T) {
	// AlertManager retries a webhook only on 5xx. Answering 200 after a
	// store failure tells it the batch is delivered and it never resends —
	// permanent for a `resolved` webhook, whose ticket then stays open
	// until (and unless) the poller's AM reconcile closes it.
	newClosedStore := func(t *testing.T) *ticket.Store {
		t.Helper()
		store, err := ticket.NewStore(context.Background(), ":memory:")
		if err != nil {
			t.Fatal(err)
		}
		// Closing the store is the cheapest faithful stand-in for a DB
		// that is down: every query returns an error.
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
		return store
	}

	firing := alertManagerPayload{
		Status: "firing",
		Alerts: []alert{{
			Status:      "firing",
			Fingerprint: "aaaa1111",
			Labels: map[string]string{
				"alertname": "KubePodCrashLooping",
				"namespace": "admins",
				"pod":       "admins-mctl-api-base-service-6d4b5c7f8-abc12",
			},
			Annotations: map[string]string{"summary": "crashloop"},
		}},
	}
	resolved := alertManagerPayload{
		Status: "resolved",
		Alerts: []alert{{
			Status:      "resolved",
			Fingerprint: "aaaa1111",
			Labels: map[string]string{
				"alertname": "KubePodCrashLooping",
				"namespace": "admins",
				"pod":       "admins-mctl-api-base-service-6d4b5c7f8-abc12",
			},
			Annotations: map[string]string{"summary": "crashloop"},
		}},
	}

	for _, tt := range []struct {
		name    string
		payload alertManagerPayload
	}{
		{"firing", firing},
		{"resolved", resolved},
	} {
		t.Run(tt.name+" alert returns 500 when the store is down", func(t *testing.T) {
			handler := NewAlertHandler(newClosedStore(t), func(*ticket.Ticket) {})
			body, _ := json.Marshal(tt.payload)
			req := httptest.NewRequest(http.MethodPost, "/api/v1/alerts", bytes.NewReader(body))
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code != http.StatusInternalServerError {
				t.Errorf("status: got %d, want %d (AlertManager only retries on 5xx)",
					rec.Code, http.StatusInternalServerError)
			}
		})
	}

	t.Run("healthy store still returns 200", func(t *testing.T) {
		handler := NewAlertHandler(newTestStore(t), func(*ticket.Ticket) {})
		body, _ := json.Marshal(firing)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/alerts", bytes.NewReader(body))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Errorf("status: got %d, want %d", rec.Code, http.StatusOK)
		}
	})

	t.Run("a redelivered batch does not duplicate the ticket", func(t *testing.T) {
		// The 500 above costs a redelivery of the whole batch, which is
		// only safe if replaying it is idempotent.
		store := newTestStore(t)
		var received []*ticket.Ticket
		handler := NewAlertHandler(store, func(tk *ticket.Ticket) {
			received = append(received, tk)
		})
		body, _ := json.Marshal(firing)
		for i := 0; i < 3; i++ {
			req := httptest.NewRequest(http.MethodPost, "/api/v1/alerts", bytes.NewReader(body))
			handler.ServeHTTP(httptest.NewRecorder(), req)
		}
		if len(received) != 1 {
			t.Errorf("expected the replayed batch to dedup onto one ticket, got %d", len(received))
		}
	})
}

func TestAlertHandlerMixedBatchProcessesEveryAlert(t *testing.T) {
	// The batch loop deliberately continues past a failure so one bad
	// alert does not hide the rest. Every other test here uses a store
	// that is entirely up or entirely down, so that behaviour was
	// undertested (claude[bot] P2 on PR #106).
	//
	// A per-alert failure is provoked without breaking the whole store: an
	// oversized summary is fine, so instead the first alert is made to hit
	// the ignore filter (dropped, not failed) and the assertion is that
	// the second still lands. For a genuine mid-batch store error the
	// closed-store test above already covers the 500.
	store := newTestStore(t)
	var received []*ticket.Ticket
	handler := NewAlertHandler(store, func(tk *ticket.Ticket) {
		received = append(received, tk)
	})
	handler.IgnoreService = regexp.MustCompile(`^openclawpr\d+`)

	payload := alertManagerPayload{
		Status: "firing",
		Alerts: []alert{
			{
				Status:      "firing",
				Fingerprint: "dropped1",
				Labels: map[string]string{
					"alertname": "KubePodCrashLooping",
					"namespace": "labs",
					"pod":       "openclawpr4-base-service-6d4b5c7f8-abc12",
				},
				Annotations: map[string]string{"summary": "preview crashloop"},
			},
			{
				Status:      "firing",
				Fingerprint: "kept2222",
				Labels: map[string]string{
					"alertname": "KubePodCrashLooping",
					"namespace": "admins",
					"pod":       "admins-mctl-api-base-service-6d4b5c7f8-abc12",
				},
				Annotations: map[string]string{"summary": "real crashloop"},
			},
		},
	}
	body, _ := json.Marshal(payload)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/alerts", bytes.NewReader(body)))

	if rec.Code != http.StatusOK {
		t.Errorf("status: got %d, want %d", rec.Code, http.StatusOK)
	}
	if len(received) != 1 {
		t.Fatalf("expected the second alert to still produce a ticket, got %d", len(received))
	}
	if received[0].Service != "admins-mctl-api-base-service" {
		t.Errorf("wrong alert survived: %q", received[0].Service)
	}
}

func TestAlertHandlerReplayedResolveSparesNewerIncident(t *testing.T) {
	// A 500 costs a redelivery of the whole batch, so an already-applied
	// resolve gets replayed. (tenant, service, type) is a coarse key —
	// several alertnames map to one ticket type — so without the
	// fingerprint scope that replay would close a DIFFERENT incident that
	// opened under the same key in between (Codex P2 on PR #106).
	store := newTestStore(t)
	handler := NewAlertHandler(store, func(*ticket.Ticket) {})

	post := func(status, fp, alertname string) {
		t.Helper()
		body, _ := json.Marshal(alertManagerPayload{
			Status: status,
			Alerts: []alert{{
				Status:      status,
				Fingerprint: fp,
				Labels: map[string]string{
					"alertname": alertname,
					"namespace": "admins",
					"pod":       "admins-mctl-api-base-service-6d4b5c7f8-abc12",
				},
				Annotations: map[string]string{"summary": alertname},
			}},
		})
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/api/v1/alerts", bytes.NewReader(body)))
	}

	// Incident A fires and resolves.
	post("firing", "aaaa1111", "PodCrashLooping")
	post("resolved", "aaaa1111", "PodCrashLooping")

	// A different alert of the same ticket type opens a new incident under
	// the identical (tenant, service, type) key.
	post("firing", "bbbb2222", "KubePodNotReady")
	open, err := store.ListOpen(ctx)
	if err != nil {
		t.Fatalf("ListOpen: %v", err)
	}
	if len(open) != 1 {
		t.Fatalf("setup: expected the new incident to be open, got %d", len(open))
	}
	newID := open[0].ID

	// AlertManager replays the older batch after a 500.
	post("resolved", "aaaa1111", "PodCrashLooping")

	got, err := store.Get(ctx, newID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status == ticket.StatusResolved {
		t.Error("a replayed resolve closed a newer, unrelated incident sharing the same (tenant, service, type) key")
	}
}

func TestAlertHandlerReplayedResolveSparesTheSameAlertsNextOccurrence(t *testing.T) {
	// The fingerprint scope alone does not make a replayed resolve
	// idempotent: an AlertManager fingerprint names an alert's LABEL SET,
	// not one occurrence of it, so the same alert flapping back to firing
	// carries the identical fingerprint. Without the EndsAt boundary the
	// redelivered resolve matches the fresh ticket too and closes an
	// incident that is still firing (Codex P2 on PR #106).
	store := newTestStore(t)
	handler := NewAlertHandler(store, func(*ticket.Ticket) {})

	post := func(status string, endsAt time.Time) {
		t.Helper()
		body, _ := json.Marshal(alertManagerPayload{
			Status: status,
			Alerts: []alert{{
				Status:      status,
				Fingerprint: "aaaa1111",
				EndsAt:      endsAt,
				Labels: map[string]string{
					"alertname": "PodCrashLooping",
					"namespace": "admins",
					"pod":       "admins-mctl-api-base-service-6d4b5c7f8-abc12",
				},
				Annotations: map[string]string{"summary": "PodCrashLooping"},
			}},
		})
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/api/v1/alerts", bytes.NewReader(body)))
	}

	// The alert fires, then resolves at endsAt.
	post("firing", time.Time{})
	endsAt := time.Now().UTC()
	post("resolved", endsAt)

	// It flaps back after the batch was acknowledged, opening a new ticket.
	post("firing", time.Time{})
	open, err := store.ListOpen(ctx)
	if err != nil {
		t.Fatalf("ListOpen: %v", err)
	}
	if len(open) != 1 {
		t.Fatalf("setup: expected the re-fired alert to be open, got %d", len(open))
	}
	newID := open[0].ID

	// AlertManager redelivers the older batch after a 500 elsewhere in it.
	post("resolved", endsAt)

	got, err := store.Get(ctx, newID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status == ticket.StatusResolved {
		t.Error("a replayed resolve closed the same alert's next, still-firing occurrence")
	}
}

// failFirstCreateStore delegates everything to a real store but makes the
// FIRST Create call fail. That is the one shape no real *ticket.Store can
// produce: closing the database fails every alert in the batch at once, which
// proves the 500 but never that a LATER alert still lands after an earlier one
// errored — the batch-drain property the 5xx design rests on.
type failFirstCreateStore struct {
	alertStore
	failed bool
}

func (s *failFirstCreateStore) Create(ctx context.Context, t *ticket.Ticket) error {
	if !s.failed {
		s.failed = true
		return errors.New("injected store failure")
	}
	return s.alertStore.Create(ctx, t)
}

func TestAlertHandlerMixedBatchProcessesLaterAlertsAfterAStoreError(t *testing.T) {
	// claude[bot] P2 on PR #106: the existing mixed-batch test provokes
	// alert 1's early exit through IgnoreService, which returns nil — the
	// error branch is never taken, so replacing the loop's implicit continue
	// with an early return after recording firstErr would pass it.
	store := newTestStore(t)
	var created []*ticket.Ticket
	handler := NewAlertHandler(store, func(tk *ticket.Ticket) { created = append(created, tk) })
	handler.store = &failFirstCreateStore{alertStore: store}

	mkAlert := func(fp, pod string) alert {
		return alert{
			Status:      "firing",
			Fingerprint: fp,
			Labels: map[string]string{
				"alertname": "PodCrashLooping",
				"namespace": "admins",
				"pod":       pod,
			},
			Annotations: map[string]string{"summary": "PodCrashLooping"},
		}
	}
	body, _ := json.Marshal(alertManagerPayload{
		Status: "firing",
		Alerts: []alert{
			mkAlert("aaaa1111", "admins-first-service-6d4b5c7f8-abc12"),
			mkAlert("bbbb2222", "admins-second-service-6d4b5c7f8-def34"),
		},
	})

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/alerts", bytes.NewReader(body)))

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("a failed alert must ask AlertManager to retry: want 500, got %d", rec.Code)
	}

	open, err := store.ListOpen(ctx)
	if err != nil {
		t.Fatalf("ListOpen: %v", err)
	}
	if len(open) != 1 {
		t.Fatalf("the second alert must still be processed after the first errored: want 1 open ticket, got %d", len(open))
	}
	if open[0].Service != "admins-second-service" {
		t.Errorf("want the second alert's ticket to survive, got service %q", open[0].Service)
	}
	if len(created) != 1 {
		t.Errorf("onTicket must fire for the alert that succeeded: want 1 call, got %d", len(created))
	}
}

func TestAlertHandlerFinishesTheWriteAfterTheCallerHangsUp(t *testing.T) {
	// AlertManager gives a webhook a short deadline and hangs up when it
	// expires. If the store work rode on r.Context(), that disconnect would
	// cancel the ticket write partway through: the alert keeps firing while
	// the incident it describes exists nowhere, and the resolve that
	// eventually arrives finds nothing to close. The write is deliberately
	// detached from the request (ctxutil.DetachedWrite) so it completes.
	store := newTestStore(t)
	handler := NewAlertHandler(store, func(*ticket.Ticket) {})

	body, _ := json.Marshal(alertManagerPayload{
		Status: "firing",
		Alerts: []alert{{
			Status:      "firing",
			Fingerprint: "aaaa1111",
			Labels: map[string]string{
				"alertname": "PodCrashLooping",
				"namespace": "billing",
				"pod":       "api-6d4b5c7f8-abc12",
			},
			Annotations: map[string]string{"summary": "PodCrashLooping"},
		}},
	})

	reqCtx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodPost, "/api/v1/alerts", bytes.NewReader(body)).WithContext(reqCtx)
	cancel() // the caller is already gone before we ever touch the store

	handler.ServeHTTP(httptest.NewRecorder(), req)

	open, err := store.ListOpen(context.Background())
	if err != nil {
		t.Fatalf("ListOpen: %v", err)
	}
	if len(open) != 1 {
		t.Fatalf("the ticket must survive the caller hanging up: want 1 open ticket, got %d", len(open))
	}
}

func TestAlertHandlerRefusesToResolveWithoutAFingerprint(t *testing.T) {
	// An empty fingerprint used to disable the scope entirely, so a single
	// resolved webhook closed every open ticket under (tenant, service,
	// type). The webhook's bearer token is optional, which put that blast
	// radius within reach of an unauthenticated caller (mctl-agent#107).
	store := newTestStore(t)
	handler := NewAlertHandler(store, func(*ticket.Ticket) {})
	var resolved []string
	handler.OnResolve = func(ids []string) { resolved = append(resolved, ids...) }

	for _, fp := range []string{"aaaa1111", "bbbb2222"} {
		tk := &ticket.Ticket{
			Source: ticket.SourceAlertManager, Type: ticket.TypePodCrashloop,
			Tenant: "billing", Service: "api", Summary: "crashloop",
			AlertFingerprint: fp,
		}
		if err := store.Create(ctx, tk); err != nil {
			t.Fatal(err)
		}
	}

	body, _ := json.Marshal(alertManagerPayload{
		Status: "resolved",
		Alerts: []alert{{
			Status:      "resolved",
			Fingerprint: "",
			Labels: map[string]string{
				"alertname": "PodCrashLooping",
				"namespace": "billing",
				"pod":       "api-6d4b5c7f8-abc12",
			},
		}},
	})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/alerts", bytes.NewReader(body)))

	// Dropped, not failed: a 500 would make AlertManager redeliver a
	// payload that no retry can improve.
	if rec.Code != http.StatusOK {
		t.Errorf("a fingerprintless resolve must be dropped, not retried: want 200, got %d", rec.Code)
	}
	open, err := store.ListOpen(ctx)
	if err != nil {
		t.Fatalf("ListOpen: %v", err)
	}
	if len(open) != 2 {
		t.Errorf("a fingerprintless resolve must close nothing: want 2 tickets still open, got %d", len(open))
	}
	if len(resolved) != 0 {
		t.Errorf("OnResolve must not fan out for a dropped resolve, got %v", resolved)
	}
}

// deadlineRecordingStore captures the deadline each alert's store work runs
// under, which is what distinguishes a per-alert ceiling from a per-batch one.
// A timing test cannot: any delay short enough for a unit test fits inside a
// 30s budget whether that budget is shared or not.
type deadlineRecordingStore struct {
	alertStore
	deadlines []time.Time
}

func (s *deadlineRecordingStore) Create(ctx context.Context, t *ticket.Ticket) error {
	d, ok := ctx.Deadline()
	if !ok {
		s.deadlines = append(s.deadlines, time.Time{})
	} else {
		s.deadlines = append(s.deadlines, d)
	}
	return s.alertStore.Create(ctx, t)
}

func TestAlertHandlerGivesEachAlertItsOwnDeadline(t *testing.T) {
	// One deadline spanning the batch is a budget the earlier alerts spend.
	// Under sustained store latency a long batch exhausts it partway and every
	// alert after that point fails instantly; redelivery preserves order, so
	// the tail can starve on every retry (codex P2 / claude P3 on PR #112).
	store := newTestStore(t)
	handler := NewAlertHandler(store, func(*ticket.Ticket) {})
	rec := &deadlineRecordingStore{alertStore: store}
	handler.store = rec

	mkAlert := func(fp, pod string) alert {
		return alert{
			Status:      "firing",
			Fingerprint: fp,
			Labels: map[string]string{
				"alertname": "PodCrashLooping",
				"namespace": "admins",
				"pod":       pod,
			},
			Annotations: map[string]string{"summary": "PodCrashLooping"},
		}
	}
	body, _ := json.Marshal(alertManagerPayload{
		Status: "firing",
		Alerts: []alert{
			mkAlert("aaaa1111", "admins-first-service-6d4b5c7f8-abc12"),
			mkAlert("bbbb2222", "admins-second-service-6d4b5c7f8-def34"),
		},
	})

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/v1/alerts", bytes.NewReader(body)))

	if w.Code != http.StatusOK {
		t.Errorf("both alerts should have been processed: want 200, got %d", w.Code)
	}
	if len(rec.deadlines) != 2 {
		t.Fatalf("expected both alerts to reach the store, got %d", len(rec.deadlines))
	}
	for i, d := range rec.deadlines {
		if d.IsZero() {
			t.Fatalf("alert %d ran with no deadline at all", i)
		}
	}
	if rec.deadlines[0].Equal(rec.deadlines[1]) {
		t.Error("both alerts shared one deadline; each must get its own ceiling so a slow batch cannot starve its tail")
	}
}
