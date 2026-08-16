package pipeline

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/mctlhq/mctl-agent/internal/mctlclient"
	"github.com/mctlhq/mctl-agent/internal/ticket"
)

func TestQuietAlertPolicy_RecordingRulesNoData(t *testing.T) {
	for _, alertName := range []string{
		quietAlertRecordingRulesNoData,
		quietAlertScrapePoolHasNoTargets,
		quietAlertTooManyScrapeErrors,
		quietAlertTooManyLogs,
	} {
		tk := &ticket.Ticket{
			Source:    ticket.SourceAlertManager,
			AlertName: alertName,
			Type:      ticket.TypeGeneric,
		}

		if shouldNotifyNewTicket(tk) {
			t.Fatalf("expected new ticket notification to be suppressed for %s", alertName)
		}
		if shouldNotifyDiagnosis(tk) {
			t.Fatalf("expected diagnosis notification to be suppressed for %s", alertName)
		}
	}
}

func TestQuietAlertPolicy_NonQuietAlertStillNotifies(t *testing.T) {
	tk := &ticket.Ticket{
		Source:    ticket.SourceAlertManager,
		AlertName: "PodCrashLooping",
		Type:      ticket.TypePodCrashloop,
	}

	if !shouldNotifyNewTicket(tk) {
		t.Fatal("expected new ticket notification to be sent")
	}
	if !shouldNotifyDiagnosis(tk) {
		t.Fatal("expected diagnosis notification to be sent")
	}
}

func TestHumanReviewOnlyAlertPolicy(t *testing.T) {
	tests := []struct {
		alertName string
		want      bool
	}{
		{alertName: "CPUThrottlingHigh", want: true},
		{alertName: "KubeJobNotCompleted", want: true},
		{alertName: "KubePersistentVolumeFillingUp", want: true},
		{alertName: "KubeStatefulSetReplicasMismatch", want: true},
		{alertName: "KubePodCrashLooping", want: false},
		{alertName: "PodCrashLooping", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.alertName, func(t *testing.T) {
			tk := &ticket.Ticket{
				Source:    ticket.SourceAlertManager,
				AlertName: tt.alertName,
			}
			if got := isHumanReviewOnlyAlert(tk); got != tt.want {
				t.Fatalf("isHumanReviewOnlyAlert(%q) = %v, want %v", tt.alertName, got, tt.want)
			}
		})
	}
}

func newEscalateTestPipeline(t *testing.T) (*Pipeline, *ticket.Store) {
	t.Helper()
	store, err := ticket.NewStore(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	// apiClient, dispatcher and telegram stay nil: escalate guards the first
	// two, and it never touches the third.
	return &Pipeline{store: store, sem: make(chan struct{}, 1)}, store
}

func newAnalyzingTicket(t *testing.T, store *ticket.Store) *ticket.Ticket {
	t.Helper()
	tk := &ticket.Ticket{
		Source:    ticket.SourceAlertManager,
		AlertName: "ArgoCDApplicationDegraded",
		Type:      ticket.TypeArgoCDDegraded,
		Tenant:    "argocd",
		Service:   "root-app",
		Summary:   "Degraded for 30m",
		Severity:  ticket.SeverityWarning,
	}
	if err := store.Create(tk); err != nil {
		t.Fatal(err)
	}
	tk.Status = ticket.StatusAnalyzing
	if err := store.Update(tk); err != nil {
		t.Fatal(err)
	}
	return tk
}

// TestEscalateLeavesAnalyzing is the regression this whole change exists for:
// a ticket the pipeline is finished with must not keep reporting "analyzing".
func TestEscalateLeavesAnalyzing(t *testing.T) {
	p, store := newEscalateTestPipeline(t)
	tk := newAnalyzingTicket(t, store)

	p.escalate(context.Background(), tk, "Escalated: no skill matched this ticket", nil)

	got, err := store.Get(tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != ticket.StatusEscalated {
		t.Errorf("status = %q, want %q", got.Status, ticket.StatusEscalated)
	}
	if got.Analysis == "" {
		t.Error("analysis is empty; an escalated ticket must say why it was escalated")
	}
	if got.Confidence != ticket.ConfidenceLow {
		t.Errorf("confidence = %q, want %q when no skill supplied one", got.Confidence, ticket.ConfidenceLow)
	}
}

// A skill's diagnosis must survive escalation — the reason is appended, not
// substituted, so the ticket carries both the finding and why no fix followed.
func TestEscalateAppendsToExistingAnalysis(t *testing.T) {
	p, store := newEscalateTestPipeline(t)
	tk := newAnalyzingTicket(t, store)
	tk.Analysis = "root-app has a Degraded CronJob in its tree"
	tk.Confidence = ticket.ConfidenceHigh
	if err := store.Update(tk); err != nil {
		t.Fatal(err)
	}

	p.escalate(context.Background(), tk, "[escalated] Infrastructure-scope alert", nil)

	got, err := store.Get(tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got.Analysis, "root-app has a Degraded CronJob") {
		t.Errorf("diagnosis lost: %q", got.Analysis)
	}
	if !strings.Contains(got.Analysis, "[escalated] Infrastructure-scope alert") {
		t.Errorf("escalation reason missing: %q", got.Analysis)
	}
	if got.Confidence != ticket.ConfidenceHigh {
		t.Errorf("confidence = %q, want the skill's %q preserved", got.Confidence, ticket.ConfidenceHigh)
	}
}

// Escalated tickets must stay visible to the watchdog and the reconcile loops.
// Dropping them out of ListOpen would recreate the original stuck-forever bug
// with no watchdog at all.
func TestEscalatedTicketStaysInListOpen(t *testing.T) {
	p, store := newEscalateTestPipeline(t)
	tk := newAnalyzingTicket(t, store)

	p.escalate(context.Background(), tk, "Escalated: no skill matched", nil)

	open, err := store.ListOpen()
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range open {
		if o.ID == tk.ID {
			return
		}
	}
	t.Error("escalated ticket missing from ListOpen")
}

// escalate mutates the same ticket updateAlertAsync hands to mctl-api. This
// needs a REAL client: with a nil one the nil guard returns before spawning a
// goroutine, so the test would pass with the snapshot fix reverted — which is
// exactly what the first version of this test did.
//
// The server blocks briefly so the request is still being marshalled while
// escalate rewrites Status, Analysis and Confidence. Reverting either snapshot
// in publishAlertAsync/updateAlertAsync makes this fail under -race.
func TestEscalateNoDataRaceWithAlertSync(t *testing.T) {
	released := make(chan struct{})
	var served sync.WaitGroup
	served.Add(1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer served.Done()
		// Read the body here, inside the handler, so the client goroutine is
		// demonstrably still touching the ticket while escalate runs.
		_, _ = io.ReadAll(r.Body)
		<-released
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	p, store := newEscalateTestPipeline(t)
	p.apiClient = mctlclient.NewClient(srv.URL, "test-token")
	tk := newAnalyzingTicket(t, store)

	p.updateAlertAsync(tk)
	p.escalate(context.Background(), tk, "Escalated: concurrent sync", nil)
	close(released)
	served.Wait()

	if got, _ := store.Get(tk.ID); got.Status != ticket.StatusEscalated {
		t.Errorf("status = %q, want %q", got.Status, ticket.StatusEscalated)
	}
}

// The nil guard is a separate property: a pipeline built without an mctl-api
// client must not panic on the alert-sync path.
func TestAlertSyncHelpersTolerateNilClient(t *testing.T) {
	p, store := newEscalateTestPipeline(t)
	tk := newAnalyzingTicket(t, store)

	p.publishAlertAsync(tk)
	p.updateAlertAsync(tk)
}
