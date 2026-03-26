package pipeline

import (
	"testing"

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
