package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/mctlhq/mctl-agent/internal/notify"
	"github.com/mctlhq/mctl-agent/internal/pipeline"
	"github.com/mctlhq/mctl-agent/internal/skill"
	"github.com/mctlhq/mctl-agent/internal/skill/builtin"
	"github.com/mctlhq/mctl-agent/internal/ticket"
	"github.com/mctlhq/mctl-agent/internal/webhook"
)

func TestExternalWebhookEndToEnd(t *testing.T) {
	store := newTestStore(t)
	tk := &ticket.Ticket{
		Source:   ticket.SourceManual,
		Type:     ticket.TypeGeneric,
		Tenant:   "labs",
		Service:  "openclaw",
		Summary:  "test incident",
		Severity: ticket.SeverityWarning,
		Status:   ticket.StatusAnalyzing,
	}
	if err := store.Create(tk); err != nil {
		t.Fatal(err)
	}

	webhookStore, err := webhook.NewStore(store.DB(), store.Dialect())
	if err != nil {
		t.Fatal(err)
	}

	eventCh := make(chan webhook.Event, 1)
	externalSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer hook-token" {
			t.Errorf("unexpected authorization header: %q", got)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		body, _ := io.ReadAll(r.Body)
		ts := r.Header.Get("X-Mctl-Webhook-Timestamp")
		sig := r.Header.Get("X-Mctl-Webhook-Signature")
		if !webhook.Verify(body, ts, sig, "secret") {
			t.Errorf("invalid signature")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		var ev webhook.Event
		if err := json.Unmarshal(body, &ev); err != nil {
			t.Errorf("invalid event payload: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		eventCh <- ev
		w.WriteHeader(http.StatusAccepted)
	}))
	defer externalSrv.Close()

	if err := webhookStore.CreateEndpoint(&webhook.WebhookEndpoint{
		AgentID:         "openclaw-prod",
		URL:             externalSrv.URL,
		Secret:          "secret",
		AuthHeaderName:  "Authorization",
		AuthHeaderValue: "Bearer hook-token",
		EventTypes:      []string{string(webhook.EventTicketCreated)},
		Active:          true,
	}); err != nil {
		t.Fatal(err)
	}

	reg := skill.NewRegistry()
	builtin.RegisterAll(reg, "")
	metrics, err := skill.NewMetrics(store.DB(), 0.3, 10)
	if err != nil {
		t.Fatal(err)
	}
	telegram := notify.NewTelegram("", "", "")
	agentSrv := httptest.NewServer(nil)
	defer agentSrv.Close()
	dispatcher := webhook.NewDispatcher(webhookStore, agentSrv.URL, 15*time.Minute)
	pipe := pipeline.NewPipeline(store, reg, metrics, nil, nil, nil, telegram, true, false, "", dispatcher)
	router := NewRouter(Options{
		Store:        store,
		Pipeline:     pipe,
		Telegram:     telegram,
		WebhookStore: webhookStore,
		WebhookTTL:   15 * time.Minute,
		OnAlert: func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		},
	})
	agentSrv.Config.Handler = router

	if err := dispatcher.Emit(context.Background(), webhook.EventTicketCreated, tk, nil); err != nil {
		t.Fatal(err)
	}
	if err := dispatcher.DispatchPending(context.Background(), 10); err != nil {
		t.Fatal(err)
	}

	var ev webhook.Event
	select {
	case ev = <-eventCh:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for webhook delivery")
	}

	claimReq := webhook.ClaimRequest{AgentID: "openclaw-prod", EventID: ev.ID}
	claimBody, _ := json.Marshal(claimReq)
	ts := time.Now().UTC().Format(time.RFC3339)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/tickets/"+tk.ID+"/external-claims", bytes.NewReader(claimBody))
	req.Header.Set("X-Mctl-Webhook-Timestamp", ts)
	req.Header.Set("X-Mctl-Webhook-Signature", webhook.Sign(claimBody, ts, "secret"))
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
		EventID:        ev.ID,
		LeaseID:        claim.LeaseID,
		IdempotencyKey: "res-1",
		Status:         webhook.ResultPRCreated,
		Summary:        "External PR created",
		Artifacts:      map[string]string{"pr_url": "https://github.com/mctlhq/mctl-gitops/pull/99"},
	}
	resultBody, _ := json.Marshal(result)
	ts = time.Now().UTC().Format(time.RFC3339)
	req = httptest.NewRequest(http.MethodPatch, "/api/v1/tickets/"+tk.ID+"/external-results", bytes.NewReader(resultBody))
	req.Header.Set("X-Mctl-Webhook-Timestamp", ts)
	req.Header.Set("X-Mctl-Webhook-Signature", webhook.Sign(resultBody, ts, "secret"))
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("result status=%d body=%s", rec.Code, rec.Body.String())
	}

	updated, err := store.Get(tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.PRNumber != 99 || updated.Status != ticket.StatusFixProposed {
		t.Fatalf("expected external PR on ticket, got status=%s pr=%d", updated.Status, updated.PRNumber)
	}
}
