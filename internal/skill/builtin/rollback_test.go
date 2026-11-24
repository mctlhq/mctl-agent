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
	// Both ends of the rollback travel with the diagnosis, so the patch can
	// be held to them.
	if diag.CurrentValue != "main-4b20a1c" || diag.SuggestedValue != "main-f66c8a7" {
		t.Errorf("rollback bound to %q -> %q, want main-4b20a1c -> main-f66c8a7", diag.CurrentValue, diag.SuggestedValue)
	}
}

// A deploy known only from the mctl-api audit names no image, so its
// diagnosis binds the rollback to none.
func TestPostDeployRollbackDiagnosisFromAuditNamesNoImage(t *testing.T) {
	ev := skill.NewEvidenceSet([]ticket.Evidence{{Type: "audit", Content: evidenceFromAPI(t,
		apiAuditRow("deploy-service", "acme", "api", "submitted", time.Now().Add(-5*time.Minute)))}})
	tk := &ticket.Ticket{Type: ticket.TypePodCrashloop, Tenant: "acme", Service: "api"}

	diag, err := NewPostDeployRollbackSkill().Diagnose(context.Background(), tk, ev)
	if err != nil {
		t.Fatal(err)
	}
	if diag.FixType != "rollback_image" || diag.CurrentValue != "" || diag.SuggestedValue != "" {
		t.Errorf("diagnosis = %+v", diag)
	}
}

// rolloutBump is one image bump of the values file `age` ago.
func rolloutBump(sha string, age time.Duration, from, to, subject string) fixer.ImageBump {
	return fixer.ImageBump{SHA: sha, CommittedAt: time.Now().UTC().Add(-age), Subject: subject, FromTag: from, ToTag: to}
}

// What the skill blames, and what it would roll back to, for histories with
// more than one change in them.
func TestRecentImageRollout(t *testing.T) {
	const (
		tenant, service = "labs", "mctl-telegram-preview"
		own             = "platform-gitops/services/labs/mctl-telegram-preview/values.yaml"
		bot             = "chore: bump labs/mctl-telegram-preview to x"
	)
	tests := []struct {
		name      string
		bumps     []fixer.ImageBump
		want      bool
		wantFrom  string
		wantTo    string
		wantCount int
	}{
		{
			// Preview services are bumped on every push, 15 minutes apart
			// and less, and the alert fires only after its `for` period: the
			// newest bump is often younger than the crash-loop. Rolling back
			// one step would land on B, which may be what started it.
			name: "two bumps inside the window roll back to the image before the first",
			bumps: []fixer.ImageBump{
				rolloutBump("aaaaaaaa1", 25*time.Minute, "A", "B", bot),
				rolloutBump("bbbbbbbb2", 3*time.Minute, "B", "C", bot),
			},
			want: true, wantFrom: "A", wantTo: "C", wantCount: 2,
		},
		{
			name: "a bump before the window is not part of the rollout",
			bumps: []fixer.ImageBump{
				rolloutBump("aaaaaaaa1", 45*time.Minute, "A", "B", bot),
				rolloutBump("bbbbbbbb2", 3*time.Minute, "B", "C", bot),
			},
			want: true, wantFrom: "B", wantTo: "C", wantCount: 1,
		},
		{
			name: "a rollback inside the window bounds the rollout",
			bumps: []fixer.ImageBump{
				rolloutBump("aaaaaaaa1", 25*time.Minute, "A", "B", bot),
				rolloutBump("bbbbbbbb2", 15*time.Minute, "B", "A", "chore: rollback labs/mctl-telegram-preview to A"),
				rolloutBump("cccccccc3", 3*time.Minute, "A", "C", bot),
			},
			want: true, wantFrom: "A", wantTo: "C", wantCount: 1,
		},
		{
			// A downgrade made by hand deploys nothing new: the service is
			// back on A, and a rollback from A would re-deploy B.
			name: "newest change returns to an older image under a bump subject",
			bumps: []fixer.ImageBump{
				rolloutBump("aaaaaaaa1", 25*time.Minute, "A", "B", bot),
				rolloutBump("bbbbbbbb2", 3*time.Minute, "B", "A", "chore: bump labs/mctl-telegram-preview to A"),
			},
		},
		{
			name: "newest change returns to an image left before the window",
			bumps: []fixer.ImageBump{
				rolloutBump("aaaaaaaa1", 50*time.Minute, "A", "B", bot),
				rolloutBump("bbbbbbbb2", 40*time.Minute, "B", "C", bot),
				rolloutBump("cccccccc3", 3*time.Minute, "C", "A", bot),
			},
		},
		{
			name: "a hand-made downgrade inside the window bounds the rollout",
			bumps: []fixer.ImageBump{
				rolloutBump("aaaaaaaa1", 25*time.Minute, "A", "B", bot),
				rolloutBump("bbbbbbbb2", 15*time.Minute, "B", "A", bot),
				rolloutBump("cccccccc3", 3*time.Minute, "A", "C", bot),
			},
			want: true, wantFrom: "A", wantTo: "C", wantCount: 1,
		},
		{
			// The bumps do not chain: something between them was not seen.
			name: "bumps that do not follow from each other are not understood",
			bumps: []fixer.ImageBump{
				rolloutBump("aaaaaaaa1", 25*time.Minute, "A", "B", bot),
				rolloutBump("bbbbbbbb2", 3*time.Minute, "X", "C", bot),
			},
		},
		{
			name: "an incomplete bump inside the rollout is not understood",
			bumps: []fixer.ImageBump{
				rolloutBump("", 25*time.Minute, "A", "B", bot),
				rolloutBump("bbbbbbbb2", 3*time.Minute, "B", "C", bot),
			},
		},
		{
			// Committer dates are client-set. One ahead of the clock would
			// be "recent" and the newest change until that date arrives.
			name: "newest change is dated in the future",
			bumps: []fixer.ImageBump{
				rolloutBump("aaaaaaaa1", -2*time.Hour, "A", "B", bot),
			},
		},
		{
			name: "a future-dated change leaves the real bump under it unblamed",
			bumps: []fixer.ImageBump{
				rolloutBump("aaaaaaaa1", -2*time.Hour, "A", "B", bot),
				rolloutBump("bbbbbbbb2", 3*time.Minute, "B", "C", bot),
			},
		},
		{
			name: "a few seconds of clock skew are not the future",
			bumps: []fixer.ImageBump{
				rolloutBump("aaaaaaaa1", -30*time.Second, "A", "B", bot),
			},
			want: true, wantFrom: "A", wantTo: "B", wantCount: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ev := bumpEvidence(t, own, tt.bumps...)
			got, ok := recentImageRollout(tenant, service, ev.Get(gitopsBumpsEvidence))
			if ok != tt.want {
				t.Fatalf("matched = %v, want %v (rollout %+v)", ok, tt.want, got)
			}
			if !tt.want {
				return
			}
			if got.From() != tt.wantFrom || got.To() != tt.wantTo || got.Count != tt.wantCount {
				t.Fatalf("rollout %s -> %s over %d bump(s), want %s -> %s over %d",
					got.From(), got.To(), got.Count, tt.wantFrom, tt.wantTo, tt.wantCount)
			}
		})
	}
}

// With several bumps in the window the match and the diagnosis say so, name
// both ends, and bind the rollback to the image before the first of them.
func TestPostDeployRollbackNamesEveryBumpOfTheRollout(t *testing.T) {
	const bot = "chore: bump labs/mctl-telegram-preview to x"
	ev := bumpEvidence(t, "platform-gitops/services/labs/mctl-telegram-preview/values.yaml",
		rolloutBump("aaaaaaaa1111", 25*time.Minute, "main-a", "main-b", bot),
		rolloutBump("bbbbbbbb2222", 3*time.Minute, "main-b", "main-c", bot))
	tk := &ticket.Ticket{Type: ticket.TypePodCrashloop, Tenant: "labs", Service: "mctl-telegram-preview"}
	s := NewPostDeployRollbackSkill()

	m := s.Match(context.Background(), tk, ev)
	if !m.Matched {
		t.Fatal("not matched")
	}
	for _, want := range []string{"2 GitOps image bumps", "aaaaaaaa", "bbbbbbbb", "main-a -> main-c"} {
		if !strings.Contains(m.Reason, want) {
			t.Errorf("reason lacks %q: %s", want, m.Reason)
		}
	}

	diag, err := s.Diagnose(context.Background(), tk, ev)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"2 GitOps image bumps", "from main-a to main-c", "aaaaaaaa", "bbbbbbbb", "rollback to main-a"} {
		if !strings.Contains(diag.Diagnosis, want) {
			t.Errorf("diagnosis lacks %q: %s", want, diag.Diagnosis)
		}
	}
	if diag.CurrentValue != "main-c" || diag.SuggestedValue != "main-a" {
		t.Errorf("rollback bound to %q -> %q, want main-c -> main-a", diag.CurrentValue, diag.SuggestedValue)
	}
}

func TestIsRollbackSubject(t *testing.T) {
	tests := []struct {
		subject string
		want    bool
	}{
		{"fix(mctl-telegram-preview): Rollback image tag from main-4b20a1c to main-f66c8a7 (digest restored)", true},
		{"chore: rollback labs/mctl-telegram to 0.147.0", true},
		{"chore(labs): roll back mctl-telegram", true},
		{`Revert "chore: bump labs/mctl-telegram-preview to main-4b20a1c"`, true},
		{"Revert the bad build", true},
		{"fix!: revert the migration image", true},
		{"chore: bump labs/mctl-telegram-preview to main-4b20a1c", false},
		// The bots embed the service name; a name is not a statement.
		{"chore: bump labs/revert-proxy to main-4b20a1c", false},
		{"chore: bump labs/rollback-controller to 1.2.0", false},
		{"fix(rollback-controller): raise the memory limit", false},
		{"feat: ship the new build", false},
		{"", false},
	}
	for _, tt := range tests {
		if got := isRollbackSubject(tt.subject); got != tt.want {
			t.Errorf("isRollbackSubject(%q) = %v, want %v", tt.subject, got, tt.want)
		}
	}
}
