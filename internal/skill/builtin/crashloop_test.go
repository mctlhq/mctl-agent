// Copyright 2025 MCTL Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package builtin

import (
	"context"
	"strings"
	"testing"

	"github.com/mctlhq/mctl-agent/internal/skill"
	"github.com/mctlhq/mctl-agent/internal/ticket"
)

func podsEvidence(containers string) string {
	return `{"items":[{"name":"labs-web-7d9f-abcde","phase":"Running","ready":false,"containers":[` + containers + `]}],"count":1}`
}

const (
	backoffContainer   = `{"name":"app","ready":false,"restart_count":7,"state":{"kind":"waiting","reason":"CrashLoopBackOff"},"last_termination":{"kind":"terminated","reason":"Error","exit_code":1}}`
	earlyBackoff       = `{"name":"app","ready":false,"restart_count":1,"state":{"kind":"waiting","reason":"CrashLoopBackOff"},"last_termination":{"kind":"terminated","reason":"Error","exit_code":1}}`
	betweenCrashes     = `{"name":"app","ready":false,"restart_count":5,"state":{"kind":"running"},"last_termination":{"kind":"terminated","reason":"Error","exit_code":2}}`
	recoveredContainer = `{"name":"app","ready":true,"restart_count":9,"state":{"kind":"running"},"last_termination":{"kind":"terminated","reason":"Error","exit_code":1}}`
	fewRestarts        = `{"name":"app","ready":false,"restart_count":2,"state":{"kind":"waiting","reason":"ContainerCreating"},"last_termination":{"kind":"terminated","reason":"Error","exit_code":1}}`
	cleanExits         = `{"name":"job","ready":false,"restart_count":6,"state":{"kind":"waiting","reason":"ContainerCreating"},"last_termination":{"kind":"terminated","reason":"Completed","exit_code":0}}`
	healthyContainer   = `{"name":"app","ready":true,"restart_count":0,"state":{"kind":"running"}}`
)

// Mutation check: each case fails when its branch of inCrashLoop is removed
// or inverted (the CrashLoopBackOff test, the restart floor, the ready-again
// exemption, the clean-exit exemption).
func TestCrashLoopSkillMatch(t *testing.T) {
	s := NewCrashLoopSkill()
	tests := []struct {
		name    string
		pods    string
		matched bool
	}{
		{"kubelet reports CrashLoopBackOff", podsEvidence(backoffContainer), true},
		{"CrashLoopBackOff below the restart floor", podsEvidence(earlyBackoff), true},
		{"caught running between two crashes", podsEvidence(betweenCrashes), true},
		{"restarted earlier, ready again", podsEvidence(recoveredContainer), false},
		{"two restarts are not a loop", podsEvidence(fewRestarts), false},
		{"restarts that exit 0 are not crashes", podsEvidence(cleanExits), false},
		{"healthy", podsEvidence(healthyContainer), false},
		{"one looping container among healthy ones", podsEvidence(healthyContainer + "," + backoffContainer), true},
		{"looping init container", `{"items":[{"name":"p","init_containers":[` + backoffContainer + `],"containers":[]}]}`, true},
		// A read that failed leaves no pods evidence. That is unknown, not
		// "no restarts", and the skill must stay out of it.
		{"no pods evidence", "", false},
		{"unparseable pods evidence", `{"items":`, false},
		{"empty listing", `{"items":[],"count":0}`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var evs []ticket.Evidence
			if tt.pods != "" {
				evs = append(evs, ticket.Evidence{Type: "pods", Content: tt.pods})
			}
			// Log text must not decide the match either way.
			evs = append(evs, ticket.Evidence{Type: "logs", Content: `{"lines":[{"line":"CrashLoopBackOff"}]}`})
			got := s.Match(context.Background(), &ticket.Ticket{}, skill.NewEvidenceSet(evs))
			if got.Matched != tt.matched {
				t.Errorf("Matched = %v, want %v", got.Matched, tt.matched)
			}
		})
	}
}

func TestCrashLoopSkillDiagnose(t *testing.T) {
	s := NewCrashLoopSkill()
	long := strings.Repeat("x", 400)
	ev := skill.NewEvidenceSet([]ticket.Evidence{
		{Type: "pods", Content: podsEvidence(healthyContainer + "," + backoffContainer)},
		{Type: "logs", Content: `{"lines":[` +
			`{"line":"level=error msg=\"first, too old to quote\""},` +
			`{"line":"level=info msg=\"starting\""},` +
			`{"line":"level=error msg=\"metadata unusable: HTTP 503\""},` +
			`{"line":"level=info msg=\"shutting down\""},` +
			`{"line":"panic: ` + long + `"},` +
			`{"line":"FATAL exiting"}]}`},
		{Type: "events", Content: `{"items":[{"reason":"BackOff","count":40},{"reason":"Unhealthy","count":3},{"reason":"BackOff","count":2}]}`},
	})
	diag, err := s.Diagnose(context.Background(), &ticket.Ticket{}, ev)
	if err != nil {
		t.Fatal(err)
	}
	if diag.Fixable || diag.Confidence != ticket.ConfidenceMedium || diag.SkillName != "crashloop" {
		t.Errorf("fixable=%v confidence=%s skill=%s", diag.Fixable, diag.Confidence, diag.SkillName)
	}
	for _, want := range []string{
		"Container app of pod labs-web-7d9f-abcde keeps restarting: 7 restart(s), last exit code 1, reason Error.",
		"metadata unusable: HTTP 503",
		"FATAL exiting",
		"Warning events: BackOff ×42, Unhealthy ×3.",
	} {
		if !strings.Contains(diag.Diagnosis, want) {
			t.Errorf("diagnosis lacks %q:\n%s", want, diag.Diagnosis)
		}
	}
	for _, unwanted := range []string{"too old to quote", `msg="starting"`, "shutting down", long} {
		if strings.Contains(diag.Diagnosis, unwanted) {
			t.Errorf("diagnosis must not carry %q", unwanted[:min(len(unwanted), 20)])
		}
	}
	// Oldest first, so the lines read in the order they happened.
	if strings.Index(diag.Diagnosis, "HTTP 503") > strings.Index(diag.Diagnosis, "FATAL exiting") {
		t.Error("error lines are not in chronological order")
	}
}

// Without an error line the diagnosis says so instead of implying a clean
// log, and missing events are simply left out.
func TestCrashLoopSkillDiagnoseWithoutLogsOrEvents(t *testing.T) {
	s := NewCrashLoopSkill()
	ev := skill.NewEvidenceSet([]ticket.Evidence{{Type: "pods", Content: podsEvidence(betweenCrashes)}})
	diag, err := s.Diagnose(context.Background(), &ticket.Ticket{}, ev)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(diag.Diagnosis, "5 restart(s), last exit code 2, reason Error") {
		t.Errorf("diagnosis: %s", diag.Diagnosis)
	}
	if !strings.Contains(diag.Diagnosis, "holds no error line") {
		t.Errorf("diagnosis does not say the log had no error line: %s", diag.Diagnosis)
	}
	if strings.Contains(diag.Diagnosis, "Warning events") {
		t.Errorf("diagnosis reports events it never had: %s", diag.Diagnosis)
	}

	if _, err := s.Diagnose(context.Background(), &ticket.Ticket{}, skill.NewEvidenceSet(nil)); err == nil {
		t.Error("Diagnose without pod evidence must fail, so the pipeline tries the next skill")
	}
	if _, err := s.Fix(context.Background(), &ticket.Ticket{}, diag); err == nil {
		t.Error("Fix must refuse: the skill proposes nothing")
	}
}

// Quoted error lines lose URL userinfo and the values of secret-looking keys;
// the rest of the line, which names the failure, stays.
//
// Mutation check: drop either replacement in redactCredentials and a case
// fails.
func TestCrashLoopDiagnosisRedactsCredentials(t *testing.T) {
	tests := []struct {
		name, line, want string
		gone             []string
	}{
		{"DSN userinfo", `error: connect postgres://app:s3cr3t@db.internal:5432/app: refused`,
			`error: connect postgres://[redacted]@db.internal:5432/app: refused`, []string{"s3cr3t", "//app"}},
		{"key=value", `level=error msg="auth failed" api_key=sk-live-123 user=bob`,
			`level=error msg="auth failed" api_key=[redacted] user=bob`, []string{"sk-live-123"}},
		{"quoted value", `fatal: db_password="hunter two" rejected`,
			`fatal: db_password=[redacted] rejected`, []string{"hunter"}},
		{"JSON field", `{"level":"error","token":"abc.def","msg":"expired"}`,
			`{"level":"error","token":[redacted],"msg":"expired"}`, []string{"abc.def"}},
		{"bearer header", `error: upstream rejected Authorization: Bearer eyJhbGci.payload.sig (401)`,
			`error: upstream rejected Authorization: [redacted] (401)`, []string{"eyJhbGci", "Bearer"}},
		{"basic header", `error: proxy said authorization=Basic dXNlcjpwYXNz retry`,
			`error: proxy said authorization=[redacted] retry`, []string{"dXNlcjpwYXNz"}},
		{"quoted bearer in JSON", `{"level":"error","authorization":"Bearer abc123","msg":"denied"}`,
			`{"level":"error","authorization":[redacted],"msg":"denied"}`, []string{"abc123"}},
		{"nothing to redact", `error: dial tcp 10.0.0.5:5432: connection refused`,
			`error: dial tcp 10.0.0.5:5432: connection refused`, nil},
		{"plain URL keeps its host", `error: GET https://idp.example.com/.well-known: HTTP 503`,
			`error: GET https://idp.example.com/.well-known: HTTP 503`, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logs := ticket.EvidenceJSON(map[string]any{"lines": []map[string]string{{"line": tt.line}}})
			got := lastErrorLines(logs, 3)
			if len(got) != 1 || got[0] != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
			for _, g := range tt.gone {
				if strings.Contains(got[0], g) {
					t.Errorf("%q survived in %q", g, got[0])
				}
			}
		})
	}
}
