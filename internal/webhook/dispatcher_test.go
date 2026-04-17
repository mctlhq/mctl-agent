package webhook

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mctlhq/mctl-agent/internal/ticket"
	_ "modernc.org/sqlite"
)

func newDispatcherStore(t *testing.T) *Store {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	store, err := NewStore(db, "sqlite")
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func TestFilterEndpointsByTenant(t *testing.T) {
	endpoints := []WebhookEndpoint{
		{AgentID: "open", AllowedTenants: nil},
		{AgentID: "labs-only", AllowedTenants: []string{"labs"}},
		{AgentID: "multi", AllowedTenants: []string{"labs", "platform-db"}},
		{AgentID: "ops", AllowedTenants: []string{"ops"}},
	}

	got := filterEndpointsByTenant(endpoints, "platform-db")
	want := map[string]struct{}{"open": {}, "multi": {}}
	if len(got) != len(want) {
		t.Fatalf("expected %d endpoints, got %d", len(want), len(got))
	}
	for _, ep := range got {
		if _, ok := want[ep.AgentID]; !ok {
			t.Errorf("unexpected endpoint passed filter: %s", ep.AgentID)
		}
	}
}

func TestDispatcherEmitFiltersScopedAgent(t *testing.T) {
	store := newDispatcherStore(t)

	var hits int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	ep := &WebhookEndpoint{
		AgentID:        "openclaw-labs",
		URL:            server.URL,
		Secret:         "secret",
		EventTypes:     []string{string(EventTicketCreated)},
		AllowedTenants: []string{"labs"},
		Active:         true,
	}
	if err := store.CreateEndpoint(ep); err != nil {
		t.Fatal(err)
	}

	d := NewDispatcher(store, "http://self", time.Minute)

	tk := &ticket.Ticket{ID: "t1", Tenant: "platform-db", Service: "shared", Type: ticket.TypeResourceLimit}
	if err := d.Emit(context.Background(), EventTicketCreated, tk, nil); err != nil {
		t.Fatalf("emit: %v", err)
	}
	if err := d.DispatchPending(context.Background(), 20); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if got := atomic.LoadInt32(&hits); got != 0 {
		t.Errorf("expected 0 deliveries to scoped agent for foreign tenant, got %d", got)
	}

	tk2 := &ticket.Ticket{ID: "t2", Tenant: "labs", Service: "openclaw", Type: ticket.TypeResourceLimit}
	if err := d.Emit(context.Background(), EventTicketCreated, tk2, nil); err != nil {
		t.Fatalf("emit: %v", err)
	}
	if err := d.DispatchPending(context.Background(), 20); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	// Give the HTTP roundtrip a moment.
	deadline := time.Now().Add(time.Second)
	for atomic.LoadInt32(&hits) < 1 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Errorf("expected 1 delivery to scoped agent for allowed tenant, got %d", got)
	}
}

func TestDispatcherEmitUnscopedAgentGetsAll(t *testing.T) {
	store := newDispatcherStore(t)

	var body atomic.Value
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf, _ := io.ReadAll(r.Body)
		body.Store(string(buf))
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	ep := &WebhookEndpoint{
		AgentID:    "global",
		URL:        server.URL,
		Secret:     "secret",
		EventTypes: []string{string(EventTicketCreated)},
		Active:     true,
	}
	if err := store.CreateEndpoint(ep); err != nil {
		t.Fatal(err)
	}

	d := NewDispatcher(store, "http://self", time.Minute)
	tk := &ticket.Ticket{ID: "tx", Tenant: "platform-db", Service: "shared", Type: ticket.TypeResourceLimit}
	if err := d.Emit(context.Background(), EventTicketCreated, tk, nil); err != nil {
		t.Fatalf("emit: %v", err)
	}
	if err := d.DispatchPending(context.Background(), 20); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	deadline := time.Now().Add(time.Second)
	for body.Load() == nil && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	raw, _ := body.Load().(string)
	if raw == "" {
		t.Fatal("expected global endpoint to receive event")
	}
	var ev Event
	if err := json.Unmarshal([]byte(raw), &ev); err != nil {
		t.Fatalf("decoding event: %v", err)
	}
	if ev.Ticket.Team != "platform-db" {
		t.Errorf("expected team platform-db, got %q", ev.Ticket.Team)
	}
}
