package builtin

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/mctlhq/mctl-agent/internal/mctlclient"
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
