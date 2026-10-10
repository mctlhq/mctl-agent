package monitor

import (
	"testing"

	"github.com/mctlhq/mctl-agent/internal/ticket"
)

// Two distinct alert names that map to one ticket type can now be open at the
// same time under one (tenant, service, type) key. ResolveByTenantService does
// not key on alert name, so what keeps a resolve for one of them from closing
// the other is its fingerprint scope. Pin that, because dedup keyed on alert
// name is what made the concurrent state reachable.
func TestAlertHandlerResolveSparesConcurrentTicketOfAnotherAlertName(t *testing.T) {
	store := newTestStore(t)
	var created []*ticket.Ticket
	handler := NewAlertHandler(store, func(tk *ticket.Ticket) { created = append(created, tk) })

	postAlert(t, handler, "firing", "fp-throttle", "CPUThrottlingHigh")
	postAlert(t, handler, "firing", "fp-memquota", "TenantMemoryQuotaHigh")
	if len(created) != 2 {
		t.Fatalf("setup: expected two concurrent tickets, got %d", len(created))
	}

	postAlert(t, handler, "resolved", "fp-throttle", "CPUThrottlingHigh")

	throttle, err := store.Get(ctx, created[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	memquota, err := store.Get(ctx, created[1].ID)
	if err != nil {
		t.Fatal(err)
	}
	if throttle.Status != ticket.StatusResolved {
		t.Errorf("the resolved alert's own ticket must close, got status %q", throttle.Status)
	}
	if memquota.Status == ticket.StatusResolved {
		t.Error("resolving CPUThrottlingHigh closed the still-firing TenantMemoryQuotaHigh ticket")
	}
}
