package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/mctlhq/mctl-agent/internal/mctlclient"
	"github.com/mctlhq/mctl-agent/internal/notify"
	"github.com/mctlhq/mctl-agent/internal/skill"
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

// The two sends for one ticket must reach mctl-api in the order they happened.
// While they were goroutines, the diagnosis sync (analyzing) and the escalation
// sync (escalated) raced; the older one arriving last reverted the incident to
// analyzing with the reason stripped — the very state escalated exists to
// prevent. Synchronous sends make the order a property of the code.
func TestAlertSyncPreservesOrder(t *testing.T) {
	var mu sync.Mutex
	var seen []string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var payload struct {
			Status string `json:"status"`
		}
		_ = json.Unmarshal(body, &payload)
		mu.Lock()
		seen = append(seen, payload.Status)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	p, store := newEscalateTestPipeline(t)
	p.apiClient = mctlclient.NewClient(srv.URL, "test-token")
	tk := newAnalyzingTicket(t, store)

	// Mirrors processTicketSync: the diagnosis sync, then an early return that
	// escalates.
	p.updateAlert(tk)
	p.escalate(context.Background(), tk, "Escalated: no skill matched", nil)

	mu.Lock()
	defer mu.Unlock()
	want := []string{ticket.StatusAnalyzing, ticket.StatusEscalated}
	if len(seen) != len(want) {
		t.Fatalf("mctl-api saw %v, want %v", seen, want)
	}
	for i := range want {
		if seen[i] != want[i] {
			t.Fatalf("mctl-api saw %v, want %v — the last write wins, so order decides the final state", seen, want)
		}
	}
}

// A pipeline built without an mctl-api client must not panic on the sync path.
func TestAlertSyncHelpersTolerateNilClient(t *testing.T) {
	p, store := newEscalateTestPipeline(t)
	tk := newAnalyzingTicket(t, store)

	p.publishAlert(tk)
	p.updateAlert(tk)
}

// stubSkill lets handleHighConfidenceFix be driven to its early returns without
// a GitHub client: the paths under test bail before p.github is touched.
type stubSkill struct {
	fix    *skill.FixResult
	fixErr error
}

func (s stubSkill) Name() string        { return "stub" }
func (s stubSkill) Version() string     { return "0.0.0" }
func (s stubSkill) Description() string { return "test stub" }
func (s stubSkill) Match(context.Context, *ticket.Ticket, skill.EvidenceSet) skill.MatchResult {
	return skill.MatchResult{Matched: true, Confidence: 1}
}
func (s stubSkill) Diagnose(context.Context, *ticket.Ticket, skill.EvidenceSet) (*skill.DiagnosisResult, error) {
	return &skill.DiagnosisResult{Diagnosis: "stub", Confidence: ticket.ConfidenceHigh, Fixable: true}, nil
}
func (s stubSkill) Fix(context.Context, *ticket.Ticket, *skill.DiagnosisResult) (*skill.FixResult, error) {
	return s.fix, s.fixErr
}
func (s stubSkill) RequiredCapabilities() []skill.CapabilityID { return nil }

// handleHighConfidenceFix had early returns that called store.Update without
// touching Status, so the ticket stayed `analyzing` for good — the same defect
// this PR fixes in processTicketSync, one function further down. These two
// paths are reachable without a GitHub client.
func TestHandleHighConfidenceFixDoesNotLeaveAnalyzing(t *testing.T) {
	cases := []struct {
		name       string
		s          skill.Skill
		wantStatus string
	}{
		{
			// Skill declined to apply: diagnosed, no fix, no PR → escalated.
			name:       "skill declined to apply",
			s:          stubSkill{fix: &skill.FixResult{Applied: false, Summary: "declined"}},
			wantStatus: ticket.StatusEscalated,
		},
		{
			// Fix generation itself failed: a fix was identified, so the existing
			// behaviour of recording fix_proposed is kept — but it must now reach
			// mctl-api too, rather than only the local store.
			name:       "fix generation failed",
			s:          stubSkill{fixErr: errors.New("boom")},
			wantStatus: ticket.StatusFixProposed,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, store := newEscalateTestPipeline(t)
			p.telegram = notify.NewTelegram("", "", "", nil)
			tk := newAnalyzingTicket(t, store)
			diag := &skill.DiagnosisResult{Diagnosis: "stub diagnosis", Confidence: ticket.ConfidenceHigh, Fixable: true}

			p.handleHighConfidenceFix(context.Background(), tk, tc.s, diag, slog.Default())

			got, err := store.Get(tk.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.Status == ticket.StatusAnalyzing {
				t.Fatal("ticket left in analyzing with nothing running")
			}
			if got.Status != tc.wantStatus {
				t.Errorf("status = %q, want %q", got.Status, tc.wantStatus)
			}
		})
	}
}

// mctl-api must first see the incident as analyzing, not open. The publish used
// to happen before the status was set and got away with it only because it ran
// in a goroutine that read the live ticket and usually observed the write on
// the next line. Once that race was removed the incident sat at `open` in
// mctl-api for the entire diagnosis, with nothing scheduled to correct it.
func TestPublishAlertCarriesAnalyzingStatus(t *testing.T) {
	var mu sync.Mutex
	var firstStatus string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var payload struct {
			Status string `json:"status"`
		}
		_ = json.Unmarshal(body, &payload)
		mu.Lock()
		if firstStatus == "" {
			firstStatus = payload.Status
		}
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	p, store := newEscalateTestPipeline(t)
	p.apiClient = mctlclient.NewClient(srv.URL, "test-token")

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

	// The two statements processTicketSync runs, in the order it runs them.
	tk.Status = ticket.StatusAnalyzing
	if err := store.Update(tk); err != nil {
		t.Fatal(err)
	}
	p.publishAlert(tk)

	mu.Lock()
	defer mu.Unlock()
	if firstStatus != ticket.StatusAnalyzing {
		t.Errorf("mctl-api first saw status %q, want %q", firstStatus, ticket.StatusAnalyzing)
	}
}
