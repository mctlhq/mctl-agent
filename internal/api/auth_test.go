package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mctlhq/mctl-agent/internal/monitor"
	"github.com/mctlhq/mctl-agent/internal/notify"
	"github.com/mctlhq/mctl-agent/internal/skill"
	"github.com/mctlhq/mctl-agent/internal/skill/remote"
	"github.com/mctlhq/mctl-agent/internal/ticket"
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

func TestTelegramFailClosedWithoutChatAllowlist(t *testing.T) {
	store := newTestStore(t)
	pipe := newTestPipeline(t, store)
	// Production auth (webhook secret) enabled, but no chat IDs configured:
	// commands must be dropped instead of falling back to open mode.
	tg := notify.NewTelegram("token", "", "", nil)
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
	req.Header.Set("X-Telegram-Bot-Api-Secret-Token", "tg-secret")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("rejected command should still return 200: status=%d", w.Code)
	}
	if pipe.IsPaused() {
		t.Fatal("command must not execute when webhook secret is set without a chat allowlist")
	}
}

// TestTelegramFailClosedWithoutWebhookSecret covers a webhook secret that is
// unset while a chat allowlist is configured. telegramSecretOK now fails
// closed on an empty secret (see auth.go), so the request itself is
// rejected with 401 at the door — the same fail-closed contract as every
// other inbound auth token this proposal covers — before the command ever
// reaches the webhook-secret/chat-allowlist execution gate below.
func TestTelegramFailClosedWithoutWebhookSecret(t *testing.T) {
	store := newTestStore(t)
	pipe := newTestPipeline(t, store)
	tg := notify.NewTelegram("token", "210408407", "", nil)
	router := NewRouter(Options{
		Store:    store,
		Pipeline: pipe,
		Telegram: tg,
		OnAlert: func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		},
	})

	body := []byte(`{"message":{"text":"/pause","chat":{"id":210408407}}}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/telegram", bytes.NewReader(body))
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("unset webhook secret must reject the request: status=%d, want 401", w.Code)
	}
	if pipe.IsPaused() {
		t.Fatal("command must not execute when a chat allowlist is set without a webhook secret")
	}
}

// TestTelegramFailsClosedWhenFullyUnconfigured used to be named
// TestTelegramOpenModeWhenFullyUnconfigured and asserted that leaving both
// TELEGRAM_WEBHOOK_SECRET and the chat allowlist unset opened a "local dev"
// bypass where commands executed. That was itself a fail-open bug: an
// unconfigured webhook secret must reject the request (401, via
// telegramSecretOK) and, independent of that, an unconfigured secret and/or
// allowlist must never let a command run (see the AND-gate in
// telegramWebhookHandler).
func TestTelegramFailsClosedWhenFullyUnconfigured(t *testing.T) {
	store := newTestStore(t)
	pipe := newTestPipeline(t, store)
	tg := notify.NewTelegram("token", "", "", nil)
	router := NewRouter(Options{
		Store:    store,
		Pipeline: pipe,
		Telegram: tg,
		OnAlert: func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		},
	})

	body := []byte(`{"message":{"text":"/pause","chat":{"id":1}}}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/telegram", bytes.NewReader(body))
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("fully unconfigured webhook secret must reject the request: status=%d, want 401", w.Code)
	}
	if pipe.IsPaused() {
		t.Fatal("command must not execute when neither webhook secret nor chat allowlist is configured")
	}
}

func TestTelegramWebhookBodyTooLarge(t *testing.T) {
	store := newTestStore(t)
	pipe := newTestPipeline(t, store)
	tg := notify.NewTelegram("token", "", "", nil)
	router := NewRouter(Options{
		Store:    store,
		Pipeline: pipe,
		Telegram: tg,
		OnAlert: func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		},
	})

	oversized := bytes.Repeat([]byte("a"), maxWebhookBodyBytes+1)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/telegram", bytes.NewReader(oversized))
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code < 400 || w.Code >= 500 {
		t.Fatalf("oversized telegram body: status=%d, want 4xx", w.Code)
	}
}

func TestAlertWebhookBodyTooLarge(t *testing.T) {
	store := newTestStore(t)
	pipe := newTestPipeline(t, store)
	router := NewRouter(Options{
		Store:    store,
		Pipeline: pipe,
		OnAlert: func(w http.ResponseWriter, r *http.Request) {
			monitor.NewAlertHandler(store, func(*ticket.Ticket) {}).ServeHTTP(w, r)
		},
	})

	oversized := bytes.Repeat([]byte("a"), 2<<20) // 2MB, over the 1MB cap
	req := httptest.NewRequest(http.MethodPost, "/api/v1/alerts", bytes.NewReader(oversized))
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code < 400 || w.Code >= 500 {
		t.Fatalf("oversized alert body: status=%d, want 4xx", w.Code)
	}
}

func TestTicketListHandlerSanitizesStoreError(t *testing.T) {
	store := newTestStore(t)
	pipe := newTestPipeline(t, store)
	store.Close() //nolint:errcheck // force ListByFilters to fail below
	router := NewRouter(Options{
		Store:    store,
		Pipeline: pipe,
		APIToken: "test-token",
		OnAlert: func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		},
	})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/tickets", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d, want 500", w.Code)
	}

	var resp map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp["error"] != "internal server error" {
		t.Fatalf("error message leaked internal detail: %q", resp["error"])
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

// TestRoutesFailClosedWhenGoverningTokenUnset is the core regression for
// this proposal: an unset AGENT_API_TOKEN / ALERTMANAGER_WEBHOOK_TOKEN must
// reject every request on the routes it governs (401), never fall back to
// "auth disabled". Each case fails against pre-fix code, where an empty
// token short-circuited requireBearer/requireBearerFunc to a no-op.
func TestRoutesFailClosedWhenGoverningTokenUnset(t *testing.T) {
	tests := []struct {
		name   string
		method string
		path   string
		body   []byte
	}{
		{"alerts", http.MethodPost, "/api/v1/alerts", []byte(`{}`)},
		{"skills register", http.MethodPost, "/api/v1/skills/register", []byte(`{"name":"x","version":"1.0","endpoint":"http://example.com"}`)},
		{"mcp", http.MethodPost, "/mcp", []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05"}}`)},
		{"tickets", http.MethodGet, "/api/v1/tickets", nil},
		{"skills", http.MethodGet, "/api/v1/skills", nil},
		{"webhooks", http.MethodGet, "/api/v1/webhooks", nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := newTestStore(t)
			pipe := newTestPipeline(t, store)
			reg := skill.NewRegistry()
			remoteMgr := remote.NewManager(reg)
			webhookStore, err := webhook.NewStore(store.DB(), store.Dialect())
			if err != nil {
				t.Fatal(err)
			}

			// APIToken and AlertWebhookToken are both left unset: the
			// scenario this proposal fixes.
			router := NewRouter(Options{
				Store:         store,
				Pipeline:      pipe,
				RemoteManager: remoteMgr,
				WebhookStore:  webhookStore,
				OnAlert: func(w http.ResponseWriter, r *http.Request) {
					w.WriteHeader(http.StatusOK)
				},
			})

			var body *bytes.Reader
			if tt.body != nil {
				body = bytes.NewReader(tt.body)
			} else {
				body = bytes.NewReader(nil)
			}
			req := httptest.NewRequest(tt.method, tt.path, body)
			w := httptest.NewRecorder()
			router.ServeHTTP(w, req)
			if w.Code != http.StatusUnauthorized {
				t.Fatalf("%s %s with no token configured: status=%d, want 401", tt.method, tt.path, w.Code)
			}
		})
	}
}
