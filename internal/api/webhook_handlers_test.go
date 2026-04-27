package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/mctlhq/mctl-agent/internal/notify"
	"github.com/mctlhq/mctl-agent/internal/ticket"
	"github.com/mctlhq/mctl-agent/internal/webhook"
)

func newWebhookEnabledRouter(t *testing.T) http.Handler {
	t.Helper()
	store := newTestStore(t)
	pipe := newTestPipeline(t, store)
	webhookStore, err := webhook.NewStore(store.DB(), store.Dialect())
	if err != nil {
		t.Fatal(err)
	}
	return NewRouter(Options{
		Store:        store,
		Pipeline:     pipe,
		Telegram:     notify.NewTelegram("", "", "", nil),
		WebhookStore: webhookStore,
		WebhookTTL:   15 * time.Minute,
		OnAlert: func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		},
	})
}

func TestWebhookRegisterListDelete(t *testing.T) {
	router := newWebhookEnabledRouter(t)
	body := []byte(`{"agent_id":"openclaw-prod","url":"https://example.com/hook","secret":"secret","auth_header_name":"Authorization","auth_header_value":"Bearer hook-token","event_types":["ticket.created"]}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("register status=%d body=%s", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/api/v1/webhooks", nil)
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("list status=%d body=%s", rec.Code, rec.Body.String())
	}
	var list map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	items := list["items"].([]interface{})
	if len(items) != 1 {
		t.Fatalf("expected 1 webhook, got %d", len(items))
	}
	if items[0].(map[string]interface{})["auth_header_name"].(string) != "Authorization" {
		t.Fatalf("expected auth_header_name in list response, got %v", items[0])
	}
	id := items[0].(map[string]interface{})["id"].(string)

	req = httptest.NewRequest(http.MethodDelete, "/api/v1/webhooks/"+id, nil)
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestExternalClaimAndResult(t *testing.T) {
	store := newTestStore(t)
	pipe := newTestPipeline(t, store)
	tk := &ticket.Ticket{
		Source:   ticket.SourceManual,
		Type:     ticket.TypeGeneric,
		Tenant:   "labs",
		Service:  "svc",
		Summary:  "test",
		Severity: ticket.SeverityWarning,
		Status:   ticket.StatusOpen,
	}
	if err := store.Create(tk); err != nil {
		t.Fatal(err)
	}
	webhookStore, err := webhook.NewStore(store.DB(), store.Dialect())
	if err != nil {
		t.Fatal(err)
	}
	if err := webhookStore.CreateEndpoint(&webhook.WebhookEndpoint{
		AgentID:    "openclaw-prod",
		URL:        "https://example.com/hook",
		Secret:     "secret",
		EventTypes: []string{string(webhook.EventTicketCreated)},
		Active:     true,
	}); err != nil {
		t.Fatal(err)
	}
	event := &webhook.Event{
		ID:         "evt_1",
		Type:       webhook.EventTicketCreated,
		OccurredAt: time.Now().UTC(),
		Ticket:     webhook.TicketPayload{ID: tk.ID},
		Delivery: webhook.DeliveryInfo{
			CallbackAuthHeader: "Authorization",
			CallbackAuthValue:  "Bearer event-token",
		},
	}
	if err := webhookStore.SaveEvent(event, tk.ID); err != nil {
		t.Fatal(err)
	}

	router := NewRouter(Options{
		Store:        store,
		Pipeline:     pipe,
		Telegram:     notify.NewTelegram("", "", "", nil),
		WebhookStore: webhookStore,
		WebhookTTL:   15 * time.Minute,
		OnAlert: func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		},
	})

	claimBody := []byte(`{"agent_id":"openclaw-prod","event_id":"evt_1"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/tickets/"+tk.ID+"/external-claims", bytes.NewReader(claimBody))
	req.Header.Set("Authorization", "Bearer event-token")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("claim status=%d body=%s", rec.Code, rec.Body.String())
	}
	var claim webhook.ClaimResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &claim); err != nil {
		t.Fatal(err)
	}

	result := webhook.ExternalResult{
		AgentID:        "openclaw-prod",
		EventID:        "evt_1",
		LeaseID:        claim.LeaseID,
		IdempotencyKey: "res_1",
		Status:         webhook.ResultPRCreated,
		Summary:        "Raised memory limit",
		Artifacts: map[string]string{
			"pr_url":     "https://github.com/mctlhq/mctl-gitops/pull/42",
			"pr_number":  "42",
			"repo":       "mctlhq/mctl-gitops",
			"branch":     "openclaw/ticket-123",
			"commit_sha": "abc123def456",
		},
	}
	resultBody, _ := json.Marshal(result)
	req = httptest.NewRequest(http.MethodPatch, "/api/v1/tickets/"+tk.ID+"/external-results", bytes.NewReader(resultBody))
	req.Header.Set("Authorization", "Bearer event-token")
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("result status=%d body=%s", rec.Code, rec.Body.String())
	}

	updated, err := store.Get(tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.PRNumber != 42 || updated.Status != ticket.StatusFixProposed {
		t.Fatalf("ticket not updated: pr=%d status=%s", updated.PRNumber, updated.Status)
	}
	if updated.PRRepo != "mctlhq/mctl-gitops" || updated.PRBranch != "openclaw/ticket-123" || updated.PRCommitSHA != "abc123def456" {
		t.Fatalf("ticket PR metadata not updated: repo=%s branch=%s sha=%s", updated.PRRepo, updated.PRBranch, updated.PRCommitSHA)
	}
}

func TestExternalClaimRejectsWrongBearerToken(t *testing.T) {
	store := newTestStore(t)
	pipe := newTestPipeline(t, store)
	tk := &ticket.Ticket{
		Source:   ticket.SourceManual,
		Type:     ticket.TypeGeneric,
		Tenant:   "labs",
		Service:  "svc",
		Summary:  "test",
		Severity: ticket.SeverityWarning,
		Status:   ticket.StatusOpen,
	}
	if err := store.Create(tk); err != nil {
		t.Fatal(err)
	}
	webhookStore, err := webhook.NewStore(store.DB(), store.Dialect())
	if err != nil {
		t.Fatal(err)
	}
	if err := webhookStore.CreateEndpoint(&webhook.WebhookEndpoint{
		AgentID:    "openclaw-prod",
		URL:        "https://example.com/hook",
		Secret:     "secret",
		EventTypes: []string{string(webhook.EventTicketCreated)},
		Active:     true,
	}); err != nil {
		t.Fatal(err)
	}
	event := &webhook.Event{
		ID:         "evt_unauth",
		Type:       webhook.EventTicketCreated,
		OccurredAt: time.Now().UTC(),
		Ticket:     webhook.TicketPayload{ID: tk.ID},
		Delivery: webhook.DeliveryInfo{
			CallbackAuthHeader: "Authorization",
			CallbackAuthValue:  "Bearer expected-token",
		},
	}
	if err := webhookStore.SaveEvent(event, tk.ID); err != nil {
		t.Fatal(err)
	}
	router := NewRouter(Options{
		Store:        store,
		Pipeline:     pipe,
		Telegram:     notify.NewTelegram("", "", "", nil),
		WebhookStore: webhookStore,
		WebhookTTL:   15 * time.Minute,
		OnAlert: func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		},
	})
	claimBody := []byte(`{"agent_id":"openclaw-prod","event_id":"evt_unauth"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/tickets/"+tk.ID+"/external-claims", bytes.NewReader(claimBody))
	req.Header.Set("Authorization", "Bearer wrong-token")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d body=%s", rec.Code, rec.Body.String())
	}
}
