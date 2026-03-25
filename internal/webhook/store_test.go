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
		AgentID:    "openclaw-prod",
		URL:        "https://example.com/hook",
		Secret:     "secret",
		EventTypes: []string{string(EventTicketCreated), string(EventTicketFixFailed)},
		Active:     true,
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

