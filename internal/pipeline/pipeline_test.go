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
