package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/mctlhq/mctl-agent/internal/fixer"
	"github.com/mctlhq/mctl-agent/internal/mctlclient"
	"github.com/mctlhq/mctl-agent/internal/skill"
	"github.com/mctlhq/mctl-agent/internal/ticket"
)

// apiAuditRow renders one row the way mctl-api's GET /api/v1/audit serves it
// (internal/audit.Entry plus the list wrapper in handlers_read.go).
func apiAuditRow(op, team, component, status string, at time.Time) string {
	return fmt.Sprintf(`{"id":"a1","timestamp":%q,"userId":"ci:github","operation":%q,`+
		`"parameters":{"action":"deploy","team_name":%q,"component_name":%q,"git_tag":"1.2.3"},`+
		`"workflowName":"deploy-service-x1","status":%q,"riskLevel":"medium"}`,
		at.UTC().Format(time.RFC3339Nano), op, team, component, status)
}

// evidenceFromAPI runs an API response through the agent's real path: the
// client's AuditEntry decode, then the JSON the pipeline stores as evidence.
func evidenceFromAPI(t *testing.T, rows ...string) string {
	t.Helper()
	body := `{"items":[`
	for i, r := range rows {
		if i > 0 {
			body += ","
		}
		body += r
	}
	body += fmt.Sprintf(`],"count":%d}`, len(rows))

	var resp struct {
		Items []mctlclient.AuditEntry `json:"items"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatal(err)
	}
	ev, err := json.Marshal(resp.Items)
	if err != nil {
		t.Fatal(err)
	}
	return string(ev)
}

func TestHasRecentDeployMatchesAPISchema(t *testing.T) {
	now := time.Now()
	recent := now.Add(-5 * time.Minute)
	old := now.Add(-45 * time.Minute)

	tests := []struct {
		name string
		row  string
		want bool
	}{
		{"exact service, submitted", apiAuditRow("deploy-service", "acme", "api", "submitted", recent), true},
		{"exact service, succeeded", apiAuditRow("deploy-service", "acme", "api", "succeeded", recent), true},
		{"other service sharing a prefix", apiAuditRow("deploy-service", "acme", "api-gateway", "submitted", recent), false},
		{"same name in another tenant", apiAuditRow("deploy-service", "acme-staging", "api", "submitted", recent), false},
		{"not a deploy", apiAuditRow("scale-service", "acme", "api", "submitted", recent), false},
		{"rollback is not a deploy", apiAuditRow("rollback-service", "acme", "api", "submitted", recent), false},
		{"denied request never ran", apiAuditRow("deploy-service", "acme", "api", "denied", recent), false},
		{"failed submit never ran", apiAuditRow("deploy-service", "acme", "api", "failed", recent), false},
		{"outside the window", apiAuditRow("deploy-service", "acme", "api", "submitted", old), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := hasRecentDeploy("acme", "api", evidenceFromAPI(t, tt.row)); got != tt.want {
				t.Fatalf("hasRecentDeploy = %v, want %v", got, tt.want)
			}
		})
	}
}

// The raw API body (wrapped in "items") is accepted as well as the bare
// list the pipeline stores.
func TestHasRecentDeployAcceptsWrappedList(t *testing.T) {
	body := `{"items":[` + apiAuditRow("deploy-service", "acme", "api", "submitted", time.Now()) + `],"count":1}`
	if !hasRecentDeploy("acme", "api", body) {
		t.Fatal("wrapped API body not matched")
	}
}

// bumpEvidence renders gitops_bumps evidence the way the pipeline stores it:
// the JSON of the fixer.ImageBumpHistory one complete read returned.
func bumpEvidence(t *testing.T, path string, bumps ...fixer.ImageBump) skill.EvidenceSet {
	t.Helper()
	if bumps == nil {
		bumps = []fixer.ImageBump{}
	}
	b, err := json.Marshal(fixer.ImageBumpHistory{
		Repo: "mctlhq/mctl-gitops", Path: path, Since: time.Now().UTC().Add(-time.Hour), Bumps: bumps,
	})
	if err != nil {
		t.Fatal(err)
	}
	return skill.NewEvidenceSet([]ticket.Evidence{{Type: "gitops_bumps", Content: string(b)}})
}

// A preview or release rollout is a commit to the service's values file and
// never reaches the mctl-api audit. The skill has to see it there.
func TestPostDeployRollbackMatchesGitOpsBump(t *testing.T) {
	const own = "platform-gitops/services/labs/mctl-telegram-preview/values.yaml"
	now := time.Now().UTC()
	bumpAt := func(age time.Duration, subject string) fixer.ImageBump {
		return fixer.ImageBump{
			SHA: "ed07e9ec4b20a1c0", CommittedAt: now.Add(-age), Subject: subject,
			FromTag: "main-f66c8a7", ToTag: "main-4b20a1c",
		}
	}
	const botSubject = "chore: bump labs/mctl-telegram-preview to main-4b20a1c"
	recent := bumpAt(5*time.Minute, botSubject)
	old := bumpAt(45*time.Minute, botSubject)
	raw := func(content string) skill.EvidenceSet {
		return skill.NewEvidenceSet([]ticket.Evidence{{Type: "gitops_bumps", Content: content}})
	}

	tests := []struct {
		name string
		ev   skill.EvidenceSet
		want bool
	}{
		{"recent bump of this service", bumpEvidence(t, own, recent), true},
		{"recent bump made by hand", bumpEvidence(t, own, bumpAt(5*time.Minute, "feat: ship the new build")), true},
		{"bump outside the window", bumpEvidence(t, own, old), false},
		{"recent bump of a service sharing a prefix",
			bumpEvidence(t, "platform-gitops/services/labs/mctl-telegram-preview-2/values.yaml", recent), false},
		{"recent bump of the same name in another tenant",
			bumpEvidence(t, "platform-gitops/services/admins/mctl-telegram-preview/values.yaml", recent), false},
		{"history read, no bump in it", bumpEvidence(t, own), false},
		{"history never read", skill.NewEvidenceSet(nil), false},
		{"history unreadable", raw(`{"path":"` + own + `","bumps":[{"sha":`), false},
		{"newest change is the agent's own rollback",
			bumpEvidence(t, own, old, bumpAt(2*time.Minute, "fix(mctl-telegram-preview): Rollback image tag from main-4b20a1c to ...")), false},
		{"newest change is a revert, over a recent bump",
			bumpEvidence(t, own, bumpAt(10*time.Minute, botSubject), bumpAt(2*time.Minute, `Revert "chore: bump labs/mctl-telegram-preview to main-4b20a1c"`)), false},
		{"recent bump on top of an older rollback",
			bumpEvidence(t, own, bumpAt(40*time.Minute, "Revert the bad build"), recent), true},
		{"bump without its tags", bumpEvidence(t, own, fixer.ImageBump{SHA: "ed07e9ec", CommittedAt: now.Add(-time.Minute)}), false},
	}
	s := NewPostDeployRollbackSkill()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tk := &ticket.Ticket{Type: ticket.TypePodCrashloop, Tenant: "labs", Service: "mctl-telegram-preview"}
			got := s.Match(context.Background(), tk, tt.ev)
			if got.Matched != tt.want {
				t.Fatalf("Matched = %v, want %v (reason %q)", got.Matched, tt.want, got.Reason)
			}
			if !tt.want {
				return
			}
			// It must outrank the crashloop skill, which matches the same
			// ticket and proposes nothing.
			if got.Confidence <= 0.78 {
				t.Errorf("confidence %.2f does not outrank crashloop (0.78)", got.Confidence)
			}
			if !strings.Contains(got.Reason, "ed07e9ec") {
				t.Errorf("reason does not name the bump: %q", got.Reason)
			}
		})
	}

	t.Run("only a crash-loop ticket matches", func(t *testing.T) {
		tk := &ticket.Ticket{Type: ticket.TypeResourceLimit, Tenant: "labs", Service: "mctl-telegram-preview"}
		if s.Match(context.Background(), tk, bumpEvidence(t, own, recent)).Matched {
			t.Error("matched a ticket that is not a crash-loop")
		}
	})
}

// The diagnosis names the bump it blames, so a reader of the ticket can
// check it, and still asks for the rollback fix.
func TestPostDeployRollbackDiagnosisNamesTheBump(t *testing.T) {
	at := time.Now().UTC().Add(-5 * time.Minute)
	ev := bumpEvidence(t, "platform-gitops/services/labs/mctl-telegram-preview/values.yaml", fixer.ImageBump{
		SHA: "ed07e9ec4b20a1c0", CommittedAt: at, Subject: "chore: bump labs/mctl-telegram-preview to main-4b20a1c",
		FromTag: "main-f66c8a7", ToTag: "main-4b20a1c",
	})
	tk := &ticket.Ticket{Type: ticket.TypePodCrashloop, Tenant: "labs", Service: "mctl-telegram-preview"}

	diag, err := NewPostDeployRollbackSkill().Diagnose(context.Background(), tk, ev)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"labs/mctl-telegram-preview", "from main-f66c8a7 to main-4b20a1c", "ed07e9ec", at.Format("2006-01-02 15:04 UTC")} {
		if !strings.Contains(diag.Diagnosis, want) {
			t.Errorf("diagnosis lacks %q: %s", want, diag.Diagnosis)
		}
	}
	if !diag.Fixable || diag.FixType != "rollback_image" || diag.Confidence != ticket.ConfidenceHigh {
		t.Errorf("diagnosis = %+v", diag)
	}
}
