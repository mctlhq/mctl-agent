package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mctlhq/mctl-agent/internal/pipeline"
	"github.com/mctlhq/mctl-agent/internal/skill"
	"github.com/mctlhq/mctl-agent/internal/skill/builtin"
	"github.com/mctlhq/mctl-agent/internal/skill/remote"
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

func newTestPipeline(t *testing.T, store *ticket.Store) *pipeline.Pipeline {
	t.Helper()
	reg := skill.NewRegistry()
	builtin.RegisterAll(reg, "")

	metrics, err := skill.NewMetrics(store.DB(), 0.3, 10)
	if err != nil {
		t.Fatal(err)
	}

	return pipeline.NewPipeline(store, reg, metrics, nil, nil, nil, nil, true, false, "")
}

func TestHealthEndpoints(t *testing.T) {
	store := newTestStore(t)
	pipe := newTestPipeline(t, store)
	reg := skill.NewRegistry()
	remoteMgr := remote.NewManager(reg)

	router := NewRouter(Options{
		Store:         store,
		Pipeline:      pipe,
		RemoteManager: remoteMgr,
		OnAlert: func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		},
	})

	tests := []struct {
		path       string
		wantStatus int
		wantBody   string
	}{
		{"/healthz", http.StatusOK, "ok"},
		{"/readyz", http.StatusOK, "ready"},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tt.path, nil)
			w := httptest.NewRecorder()
			router.ServeHTTP(w, req)

			if w.Code != tt.wantStatus {
				t.Errorf("%s: status = %d, want %d", tt.path, w.Code, tt.wantStatus)
			}
			var resp map[string]string
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
				t.Fatal(err)
			}
			if resp["status"] != tt.wantBody {
				t.Errorf("%s: body status = %q, want %q", tt.path, resp["status"], tt.wantBody)
			}
		})
	}
}

func TestTicketListEndpoint(t *testing.T) {
	store := newTestStore(t)
	pipe := newTestPipeline(t, store)

	// Create a ticket.
	tk := &ticket.Ticket{
		Source:   ticket.SourcePolling,
		Type:     ticket.TypeArgoCDDegraded,
		Tenant:   "data",
		Service:  "etl",
		Severity: ticket.SeverityWarning,
	}
	if err := store.Create(tk); err != nil {
		t.Fatal(err)
	}

	router := NewRouter(Options{
		Store:    store,
		Pipeline: pipe,
		OnAlert: func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		},
	})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/tickets", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	count := int(resp["count"].(float64))
	if count != 1 {
		t.Errorf("count = %d, want 1", count)
	}
}

func TestSkillListEndpoint(t *testing.T) {
	store := newTestStore(t)
	pipe := newTestPipeline(t, store)

	router := NewRouter(Options{
		Store:    store,
		Pipeline: pipe,
		OnAlert: func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		},
	})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/skills", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	count := int(resp["count"].(float64))
	if count < 9 {
		t.Errorf("skills count = %d, expected at least 9 builtin skills", count)
	}
}

func TestRemoteSkillEndpoints(t *testing.T) {
	store := newTestStore(t)
	pipe := newTestPipeline(t, store)
	reg := skill.NewRegistry()
	remoteMgr := remote.NewManager(reg)

	router := NewRouter(Options{
		Store:         store,
		Pipeline:      pipe,
		RemoteManager: remoteMgr,
		OnAlert: func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		},
	})

	// Register a remote skill.
	regPayload := remote.Registration{
		Name:     "test-remote",
		Version:  "1.0",
		Endpoint: "http://example.com",
	}
	body, _ := json.Marshal(regPayload)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/skills/register", bytes.NewReader(body))
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Errorf("register: status = %d, want 201. Body: %s", w.Code, w.Body.String())
	}

	// List remote skills.
	req = httptest.NewRequest(http.MethodGet, "/api/v1/skills/remote", nil)
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("list: status = %d", w.Code)
	}
	var listResp map[string]interface{}
	_ = json.Unmarshal(w.Body.Bytes(), &listResp)
	if int(listResp["count"].(float64)) != 1 {
		t.Errorf("expected 1 remote skill, got %v", listResp["count"])
	}

	// Unregister.
	req = httptest.NewRequest(http.MethodDelete, "/api/v1/skills/test-remote", nil)
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("unregister: status = %d, body: %s", w.Code, w.Body.String())
	}

	// Unregister again → 404.
	req = httptest.NewRequest(http.MethodDelete, "/api/v1/skills/test-remote", nil)
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("double unregister: status = %d, want 404", w.Code)
	}
}

func TestMCPEndpoint(t *testing.T) {
	store := newTestStore(t)
	pipe := newTestPipeline(t, store)

	router := NewRouter(Options{
		Store:    store,
		Pipeline: pipe,
		OnAlert: func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		},
	})

	// Send MCP initialize request.
	rpcReq := map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "initialize",
		"params": map[string]interface{}{
			"protocolVersion": "2024-11-05",
			"clientInfo":      map[string]string{"name": "test"},
		},
	}
	body, _ := json.Marshal(rpcReq)
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("MCP status = %d, body: %s", w.Code, w.Body.String())
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["jsonrpc"] != "2.0" {
		t.Errorf("expected jsonrpc 2.0, got %v", resp["jsonrpc"])
	}
}

func TestSkillMetricsEndpoint(t *testing.T) {
	store := newTestStore(t)
	pipe := newTestPipeline(t, store)

	router := NewRouter(Options{
		Store:    store,
		Pipeline: pipe,
		OnAlert: func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		},
	})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/skills/oomkilled/metrics", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body: %s", w.Code, w.Body.String())
	}
}
