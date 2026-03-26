package ticket

import (
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := NewStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func TestStoreCreateAndGet(t *testing.T) {
	store := newTestStore(t)

	tk := &Ticket{
		Source:    SourceAlertManager,
		AlertName: "PodCrashLooping",
		Type:      TypePodCrashloop,
		Tenant:    "billing",
		Service:   "api",
		Summary:   "Pod is crash looping",
		Severity:  SeverityCritical,
	}

	if err := store.Create(tk); err != nil {
		t.Fatal(err)
	}

	if tk.ID == "" {
		t.Fatal("expected ID to be generated")
	}
	if tk.Status != StatusOpen {
		t.Errorf("expected status open, got %s", tk.Status)
	}
	if tk.CreatedAt.IsZero() {
		t.Error("expected CreatedAt to be set")
	}

	got, err := store.Get(tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Type != TypePodCrashloop {
		t.Errorf("expected type %s, got %s", TypePodCrashloop, got.Type)
	}
	if got.Tenant != "billing" {
		t.Errorf("expected tenant billing, got %s", got.Tenant)
	}
	if got.Service != "api" {
		t.Errorf("expected service api, got %s", got.Service)
	}
	if got.AlertName != "PodCrashLooping" {
		t.Errorf("expected alert name PodCrashLooping, got %s", got.AlertName)
	}
}

func TestStoreUpdate(t *testing.T) {
	store := newTestStore(t)

	tk := &Ticket{Source: SourcePolling, Type: TypeArgoCDDegraded, Tenant: "data", Service: "etl"}
	if err := store.Create(tk); err != nil {
		t.Fatal(err)
	}

	tk.Status = StatusAnalyzing
	tk.Analysis = "ArgoCD app is degraded"
	if err := store.Update(tk); err != nil {
		t.Fatal(err)
	}

	got, err := store.Get(tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusAnalyzing {
		t.Errorf("expected status analyzing, got %s", got.Status)
	}
	if got.Analysis != "ArgoCD app is degraded" {
		t.Errorf("unexpected analysis: %s", got.Analysis)
	}
}

func TestStoreAddEvidence(t *testing.T) {
	store := newTestStore(t)

	tk := &Ticket{Source: SourceAlertManager, Type: TypePodCrashloop, Tenant: "t", Service: "s"}
	if err := store.Create(tk); err != nil {
		t.Fatal(err)
	}

	ev := Evidence{Type: "logs", Content: "error: OOMKilled", CollectedAt: time.Now().UTC()}
	if err := store.AddEvidence(tk.ID, ev); err != nil {
		t.Fatal(err)
	}

	got, err := store.Get(tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Evidence) != 1 {
		t.Fatalf("expected 1 evidence, got %d", len(got.Evidence))
	}
	if got.Evidence[0].Type != "logs" {
		t.Errorf("expected evidence type logs, got %s", got.Evidence[0].Type)
	}
	if got.Evidence[0].Content != "error: OOMKilled" {
		t.Errorf("unexpected evidence content: %s", got.Evidence[0].Content)
	}
}

func TestStoreFindDuplicate(t *testing.T) {
	store := newTestStore(t)

	// No tickets yet — should return nil.
	dup, err := store.FindDuplicate("billing", "api", TypePodCrashloop)
	if err != nil {
		t.Fatal(err)
	}
	if dup != nil {
		t.Error("expected nil for no duplicate")
	}

	// Create a ticket.
	tk := &Ticket{Source: SourceAlertManager, Type: TypePodCrashloop, Tenant: "billing", Service: "api"}
	if err := store.Create(tk); err != nil {
		t.Fatal(err)
	}

	// Same tenant/service/type → should find duplicate.
	dup, err = store.FindDuplicate("billing", "api", TypePodCrashloop)
	if err != nil {
		t.Fatal(err)
	}
	if dup == nil {
		t.Fatal("expected duplicate to be found")
	}
	if dup.ID != tk.ID {
		t.Errorf("expected dup ID %s, got %s", tk.ID, dup.ID)
	}

	// Different type → no duplicate.
	dup, err = store.FindDuplicate("billing", "api", TypeResourceLimit)
	if err != nil {
		t.Fatal(err)
	}
	if dup != nil {
		t.Error("expected no duplicate for different type")
	}

	// Resolved ticket → no duplicate.
	tk.Status = StatusResolved
	now := time.Now().UTC()
	tk.ResolvedAt = &now
	if err := store.Update(tk); err != nil {
		t.Fatal(err)
	}
	dup, err = store.FindDuplicate("billing", "api", TypePodCrashloop)
	if err != nil {
		t.Fatal(err)
	}
	if dup != nil {
		t.Error("expected no duplicate for resolved ticket")
	}
}

func TestStoreListOpenAndListAll(t *testing.T) {
	store := newTestStore(t)

	// Create several tickets with different statuses.
	for _, s := range []string{StatusOpen, StatusAnalyzing, StatusResolved, StatusSuppressed} {
		tk := &Ticket{Source: SourcePolling, Type: TypeArgoCDDegraded, Tenant: "t", Service: "s", Status: s}
		tk.Status = "" // Let Create set default
		if err := store.Create(tk); err != nil {
			t.Fatal(err)
		}
		if s != StatusOpen {
			tk.Status = s
			if err := store.Update(tk); err != nil {
				t.Fatal(err)
			}
		}
	}

	open, err := store.ListOpen()
	if err != nil {
		t.Fatal(err)
	}
	// Should include open + analyzing but not resolved/suppressed.
	if len(open) != 2 {
		t.Errorf("expected 2 open tickets, got %d", len(open))
	}

	all, err := store.ListAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 4 {
		t.Errorf("expected 4 total tickets, got %d", len(all))
	}
}

func TestStoreResolveByTenantService(t *testing.T) {
	store := newTestStore(t)

	tk := &Ticket{Source: SourceAlertManager, Type: TypePodCrashloop, Tenant: "billing", Service: "api"}
	if err := store.Create(tk); err != nil {
		t.Fatal(err)
	}

	if err := store.ResolveByTenantService("billing", "api", TypePodCrashloop); err != nil {
		t.Fatal(err)
	}

	got, err := store.Get(tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusResolved {
		t.Errorf("expected resolved, got %s", got.Status)
	}
	if got.ResolvedAt == nil {
		t.Error("expected ResolvedAt to be set")
	}
}

func TestStoreCountPRsInWindow(t *testing.T) {
	store := newTestStore(t)

	// No tickets → count = 0.
	count, err := store.CountPRsInWindow(1)
	if err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Errorf("expected 0, got %d", count)
	}

	// Create ticket with PR URL.
	tk := &Ticket{Source: SourcePolling, Type: TypePodCrashloop, Tenant: "t", Service: "s", PRURL: "https://github.com/mctlhq/mctl-gitops/pull/1"}
	if err := store.Create(tk); err != nil {
		t.Fatal(err)
	}

	count, err = store.CountPRsInWindow(1)
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Errorf("expected 1, got %d", count)
	}

	// Ticket without PR URL should not count.
	tk2 := &Ticket{Source: SourcePolling, Type: TypePodCrashloop, Tenant: "t2", Service: "s2"}
	if err := store.Create(tk2); err != nil {
		t.Fatal(err)
	}

	count, err = store.CountPRsInWindow(1)
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Errorf("expected 1 (only PR ticket), got %d", count)
	}
}

func TestEvidenceJSON(t *testing.T) {
	data := map[string]string{"health": "Degraded"}
	got := EvidenceJSON(data)
	if got != `{"health":"Degraded"}` {
		t.Errorf("unexpected JSON: %s", got)
	}
}
