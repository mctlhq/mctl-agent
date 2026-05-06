package monitor

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
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
		{"ArgoCDApplicationDegraded", ticket.TypeArgoCDDegraded, ticket.SeverityWarning},
		{"ArgoCDApplicationSyncFailed", ticket.TypeArgoCDDegraded, ticket.SeverityWarning},
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
				Status: "firing",
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
				Status: "resolved",
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
				Status: "firing",
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
	if err := store.Create(t0); err != nil {
		t.Fatal(err)
	}

	// Enable filter.
	handler.IgnoreService = regexp.MustCompile(`^hooktest-.*$`)

	// Send a resolved alert for the filtered service.
	resolve := alertManagerPayload{
		Alerts: []alert{
			{
				Status: "resolved",
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

	open, _ := store.ListOpen()
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

	open, _ := store.ListOpen()
	if len(open) != 1 {
		t.Fatalf("expected 1 ticket, got %d", len(open))
	}
	firstUpdated := open[0].UpdatedAt

	// Force a gap, then fire the duplicate. Touch should advance UpdatedAt.
	time.Sleep(20 * time.Millisecond)
	req = httptest.NewRequest(http.MethodPost, "/api/v1/alerts", bytes.NewReader(body))
	handler.ServeHTTP(httptest.NewRecorder(), req)

	open, _ = store.ListOpen()
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

	tickets, err := store.ListOpen()
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
