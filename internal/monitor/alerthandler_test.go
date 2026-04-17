package monitor

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/mctlhq/mctl-agent/internal/ticket"
	_ "modernc.org/sqlite"
)

func newTestStore(t *testing.T) *ticket.Store {
	t.Helper()
	store, err := ticket.NewStore(":memory:")
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

	// Verify ticket exists.
	open, _ := store.ListOpen()
	if len(open) != 1 {
		t.Fatalf("expected 1 open ticket, got %d", len(open))
	}

	// Then: resolve alert.
	resolve := alertManagerPayload{
		Alerts: []alert{
			{
				Status: "resolved",
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
	open, _ = store.ListOpen()
	if len(open) != 0 {
		t.Errorf("expected 0 open tickets after resolve, got %d", len(open))
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
				Status: "firing",
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
				Status: "resolved",
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
				Status: "firing",
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
				Status: "resolved",
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
