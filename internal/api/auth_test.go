package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mctlhq/mctl-agent/internal/notify"
	"github.com/mctlhq/mctl-agent/internal/webhook"
)

func TestControlPlaneRequiresBearerWhenConfigured(t *testing.T) {
	store := newTestStore(t)
	pipe := newTestPipeline(t, store)
	router := NewRouter(Options{
		Store:    store,
		Pipeline: pipe,
		APIToken: "op-secret",
		OnAlert: func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		},
	})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/tickets", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated tickets: status=%d, want 401", w.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/v1/tickets", nil)
	req.Header.Set("Authorization", "Bearer op-secret")
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("authenticated tickets: status=%d, want 200", w.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/healthz", nil)
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("healthz must stay public: status=%d", w.Code)
	}
}

func TestAlertWebhookRequiresBearerWhenConfigured(t *testing.T) {
	store := newTestStore(t)
	pipe := newTestPipeline(t, store)
	called := false
	router := NewRouter(Options{
		Store:             store,
		Pipeline:          pipe,
		AlertWebhookToken: "am-secret",
		OnAlert: func(w http.ResponseWriter, r *http.Request) {
			called = true
			w.WriteHeader(http.StatusOK)
		},
	})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/alerts", bytes.NewReader([]byte(`{}`)))
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated alerts: status=%d, want 401", w.Code)
	}
	if called {
		t.Fatal("alert handler must not run without token")
	}

	req = httptest.NewRequest(http.MethodPost, "/api/v1/alerts", bytes.NewReader([]byte(`{}`)))
	req.Header.Set("Authorization", "Bearer am-secret")
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("authenticated alerts: status=%d, want 200", w.Code)
	}
	if !called {
		t.Fatal("alert handler should run with token")
	}
}

func TestTelegramWebhookSecretAndChatAllowlist(t *testing.T) {
	store := newTestStore(t)
	pipe := newTestPipeline(t, store)
	tg := notify.NewTelegram("token", "210408407", "", nil)
	router := NewRouter(Options{
		Store:                 store,
		Pipeline:              pipe,
		Telegram:              tg,
		TelegramWebhookSecret: "tg-secret",
		OnAlert: func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		},
	})

	body := []byte(`{"message":{"text":"/pause","chat":{"id":1}}}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/telegram", bytes.NewReader(body))
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("missing telegram secret: status=%d, want 401", w.Code)
	}

	req = httptest.NewRequest(http.MethodPost, "/api/v1/telegram", bytes.NewReader(body))
	req.Header.Set("X-Telegram-Bot-Api-Secret-Token", "tg-secret")
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("foreign chat should be ignored with 200: status=%d", w.Code)
	}

	allowed := []byte(`{"message":{"text":"/status","chat":{"id":210408407}}}`)
	req = httptest.NewRequest(http.MethodPost, "/api/v1/telegram", bytes.NewReader(allowed))
	req.Header.Set("X-Telegram-Bot-Api-Secret-Token", "tg-secret")
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("allowlisted chat: status=%d, want 200", w.Code)
	}
}

func TestWebhookCRUDRequiresBearerWhenConfigured(t *testing.T) {
	store := newTestStore(t)
	pipe := newTestPipeline(t, store)
	webhookStore, err := webhook.NewStore(store.DB(), store.Dialect())
	if err != nil {
		t.Fatal(err)
	}
	router := NewRouter(Options{
		Store:        store,
		Pipeline:     pipe,
		APIToken:     "op-secret",
		WebhookStore: webhookStore,
		OnAlert: func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		},
	})

	body := []byte(`{"agent_id":"x","url":"https://example.com","secret":"s"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks", bytes.NewReader(body))
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated webhook create: status=%d, want 401", w.Code)
	}
}

func TestMCPRequiresBearerWhenConfigured(t *testing.T) {
	store := newTestStore(t)
	pipe := newTestPipeline(t, store)
	router := NewRouter(Options{
		Store:    store,
		Pipeline: pipe,
		APIToken: "op-secret",
		OnAlert: func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		},
	})

	rpcReq := map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "initialize",
		"params":  map[string]interface{}{"protocolVersion": "2024-11-05"},
	}
	body, _ := json.Marshal(rpcReq)
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(body))
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated MCP: status=%d, want 401", w.Code)
	}
}
