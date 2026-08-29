package pipeline

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/mctlhq/mctl-agent/internal/mctlclient"
	"github.com/mctlhq/mctl-agent/internal/notify"
	"github.com/mctlhq/mctl-agent/internal/skill"
	"github.com/mctlhq/mctl-agent/internal/ticket"
)

// newAsyncTestPipeline builds a pipeline whose async path actually runs end to
// end: an empty registry sends every ticket down the "no skill matched" branch,
// which escalates and returns. The mctl-api client points at a stub server —
// collectEvidence calls it unguarded, so a nil client panics inside the
// goroutine rather than failing the assertion. dispatcher stays nil (guarded)
// and telegram is disabled, so the notify call is real but inert.
func newAsyncTestPipeline(t *testing.T, slots int) (*Pipeline, *ticket.Store) {
	t.Helper()
	store, err := ticket.NewStore(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)

	return &Pipeline{
		store:     store,
		registry:  skill.NewRegistry(),
		telegram:  notify.NewTelegram("", "", "", nil),
		apiClient: mctlclient.NewClient(srv.URL, "test-token"),
		sem:       make(chan struct{}, slots),
	}, store
}

// TriggerAnalysis must not hand the caller the same *ticket.Ticket the pipeline
// goroutine mutates. The MCP handler reads fields off the returned value while
// processTicketSync is writing Status and Analysis on the original.
//
// Mutation check: return `t` instead of `&snapshot` and `go test -race` reports
// a write/read data race on this test.
func TestTriggerAnalysisReturnsSnapshotNotSharedPointer(t *testing.T) {
	p, _ := newAsyncTestPipeline(t, 1)

	got, err := p.TriggerAnalysis(context.Background(), "argocd", "root-app", "manual")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID == "" {
		t.Fatal("snapshot must be taken after store.Create, so it carries the ID")
	}

	// Read the returned value hard while the pipeline goroutine runs.
	deadline := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) {
		_ = got.Status
		_ = got.Analysis
		_ = got.ID
	}

	if got.Status != ticket.StatusOpen {
		t.Fatalf("snapshot should keep the status it was created with, got %q", got.Status)
	}
}

// A full semaphore must not block the caller. One caller is the AlertManager
// webhook handler; blocking it makes AlertManager time out and retry, piling
// more load onto a pipeline that is already saturated.
//
// Mutation check: move `p.sem <- struct{}{}` back above `go func()` and this
// test times out.
func TestProcessTicketDoesNotBlockCallerWhenSaturated(t *testing.T) {
	p, store := newAsyncTestPipeline(t, 1)

	// Occupy the only slot and keep it occupied.
	p.sem <- struct{}{}
	defer func() { <-p.sem }()

	tk := &ticket.Ticket{
		Source:   ticket.SourceManual,
		Type:     ticket.TypeArgoCDDegraded,
		Tenant:   "argocd",
		Service:  "root-app",
		Summary:  "saturated",
		Severity: ticket.SeverityWarning,
		Status:   ticket.StatusOpen,
	}
	if err := store.Create(ctx, tk); err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	go func() {
		p.ProcessTicket(tk)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("ProcessTicket blocked the caller while the semaphore was full")
	}
}

// The process context reaches the goroutine, so shutdown stops diagnosis
// instead of letting it run against a closing process.
func TestProcessTicketUsesBaseContext(t *testing.T) {
	p, _ := newAsyncTestPipeline(t, 1)

	if p.baseContext() != context.Background() {
		t.Fatal("unset base context should fall back to context.Background()")
	}

	ctx, cancel := context.WithCancel(context.Background())
	p.SetBaseContext(ctx)
	if p.baseContext() != ctx {
		t.Fatal("SetBaseContext did not take effect")
	}
	cancel()
	if p.baseContext().Err() == nil {
		t.Fatal("cancelling the installed context must be observable through baseContext")
	}
}

// TriggerAnalysis must not create a ticket for a request that is already gone.
func TestTriggerAnalysisRejectsCancelledContext(t *testing.T) {
	p, store := newAsyncTestPipeline(t, 1)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := p.TriggerAnalysis(ctx, "argocd", "root-app", "manual"); err == nil {
		t.Fatal("expected an error for a cancelled caller context")
	}
	// Not the cancelled ctx above: the assertion is about what the store
	// holds, not about honouring the caller's cancellation.
	open, err := store.ListOpen(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 0 {
		t.Fatalf("no ticket should have been created, got %d", len(open))
	}
}

// Escalation is a terminal state, and the path that reaches it most often is a
// fix step that failed because the diagnosis deadline expired — so escalate is
// routinely called with an already-cancelled context. Writing the terminal
// status under it fails instantly while the rest of escalate still mirrors to
// mctl-api and emits the external event, leaving the ticket stuck in
// `analyzing` and disagreeing with everything downstream (codex P2 on #112).
func TestEscalatePersistsUnderAnExpiredContext(t *testing.T) {
	p, store := newAsyncTestPipeline(t, 1)

	tk := &ticket.Ticket{
		Source: ticket.SourceAlertManager, Type: ticket.TypePodCrashloop,
		Tenant: "billing", Service: "api", Summary: "crashloop",
		Status: ticket.StatusAnalyzing,
	}
	if err := store.Create(context.Background(), tk); err != nil {
		t.Fatal(err)
	}

	expired, cancel := context.WithCancel(context.Background())
	cancel()

	p.escalate(expired, tk, "[escalated] the fix step ran out of time", nil)

	got, err := store.Get(context.Background(), tk.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != ticket.StatusEscalated {
		t.Errorf("the terminal escalation must be persisted even when the caller's context is spent: want %q, got %q",
			ticket.StatusEscalated, got.Status)
	}
}

// Detaching the escalation write removed the accident that used to protect a
// concurrent resolution: while the write rode a cancelled context it could not
// clobber anything, and now it can. A resolve landing during the slow fix step
// must not be overwritten by the stale escalated status and its stale nil
// ResolvedAt (codex P1 on #113).
func TestEscalateDoesNotClobberAConcurrentResolution(t *testing.T) {
	p, store := newAsyncTestPipeline(t, 1)

	tk := &ticket.Ticket{
		Source: ticket.SourceAlertManager, Type: ticket.TypePodCrashloop,
		Tenant: "billing", Service: "api", Summary: "crashloop",
		Status: ticket.StatusAnalyzing,
	}
	if err := store.Create(context.Background(), tk); err != nil {
		t.Fatal(err)
	}

	// The alert resolves while the fix step is still burning its deadline.
	applied, err := store.ResolveByIDFromStatus(context.Background(), tk.ID,
		ticket.StatusAnalyzing, "resolved by AlertManager")
	if err != nil {
		t.Fatal(err)
	}
	if !applied {
		t.Fatal("setup: the concurrent resolve should have applied")
	}

	// The pipeline still holds its pre-resolve copy and now escalates it.
	expired, cancel := context.WithCancel(context.Background())
	cancel()
	p.escalate(expired, tk, "[escalated] the fix step ran out of time", nil)

	got, err := store.Get(context.Background(), tk.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != ticket.StatusResolved {
		t.Errorf("a concurrently resolved ticket must stay resolved: want %q, got %q",
			ticket.StatusResolved, got.Status)
	}
	if got.ResolvedAt == nil {
		t.Error("resolved_at was cleared by the stale escalation write")
	}
}
