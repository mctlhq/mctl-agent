package webhook

import (
	"database/sql"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func newWebhookStore(t *testing.T) *Store {
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

func TestEndpointCRUDAndClaims(t *testing.T) {
	store := newWebhookStore(t)
	ep := &WebhookEndpoint{
		AgentID:         "openclaw-prod",
		URL:             "https://example.com/hook",
		Secret:          "secret",
		AuthHeaderName:  "Authorization",
		AuthHeaderValue: "Bearer test-token",
		EventTypes:      []string{string(EventTicketCreated), string(EventTicketFixFailed)},
		Active:          true,
	}
	if err := store.CreateEndpoint(ep); err != nil {
		t.Fatal(err)
	}
	items, err := store.ListEndpoints()
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 endpoint, got %d", len(items))
	}
	if items[0].AuthHeaderName != "Authorization" || items[0].AuthHeaderValue != "Bearer test-token" {
		t.Fatalf("expected auth header fields to round-trip, got %#v", items[0])
	}

	event := &Event{ID: "evt_1", Type: EventTicketCreated, OccurredAt: time.Now().UTC()}
	if err := store.SaveEvent(event, "ticket-1"); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateDeliveries(event.ID, items, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	deliveries, err := store.DispatchBatch(time.Now().UTC(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(deliveries) != 1 {
		t.Fatalf("expected 1 delivery, got %d", len(deliveries))
	}

	claim, err := store.CreateClaim("ticket-1", ClaimRequest{AgentID: ep.AgentID, EventID: event.ID}, 15*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if claim.LeaseID == "" {
		t.Fatal("expected lease id")
	}
	if _, err := store.CreateClaim("ticket-1", ClaimRequest{AgentID: "other", EventID: event.ID}, 15*time.Minute); err != ErrAlreadyClaimed {
		t.Fatalf("expected ErrAlreadyClaimed, got %v", err)
	}
}

func TestCreateEndpointDeactivatesPriorForSameAgent(t *testing.T) {
	store := newWebhookStore(t)

	// Simulate the pre-scoping registration: no allowed_tenants.
	old := &WebhookEndpoint{
		AgentID:    "openclaw-labs",
		URL:        "https://old.example.com/hook",
		Secret:     "secret",
		EventTypes: []string{string(EventTicketCreated)},
	}
	if err := store.CreateEndpoint(old); err != nil {
		t.Fatal(err)
	}

	// Re-register the same agent with a tenant scope — this should
	// deactivate the previous endpoint so the dispatcher stops delivering
	// cross-tenant events via the unrestricted row.
	scoped := &WebhookEndpoint{
		AgentID:        "openclaw-labs",
		URL:            "https://new.example.com/hook",
		Secret:         "secret",
		EventTypes:     []string{string(EventTicketCreated)},
		AllowedTenants: []string{"labs"},
	}
	if err := store.CreateEndpoint(scoped); err != nil {
		t.Fatal(err)
	}

	endpoints, err := store.ListEndpointsForEvent(EventTicketCreated)
	if err != nil {
		t.Fatal(err)
	}
	if len(endpoints) != 1 {
		t.Fatalf("expected exactly one active endpoint after re-register, got %d", len(endpoints))
	}
	if endpoints[0].URL != "https://new.example.com/hook" {
		t.Errorf("expected the new endpoint to be active, got URL=%q", endpoints[0].URL)
	}
	if len(endpoints[0].AllowedTenants) != 1 || endpoints[0].AllowedTenants[0] != "labs" {
		t.Errorf("expected allowed_tenants=[labs], got %v", endpoints[0].AllowedTenants)
	}
}
