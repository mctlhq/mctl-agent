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

package pipeline

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mctlhq/mctl-agent/internal/mctlclient"
	"github.com/mctlhq/mctl-agent/internal/skill/builtin"
	"github.com/mctlhq/mctl-agent/internal/ticket"
)

const crashingPods = `{"items":[{"name":"billing-api-5c7-xk2","containers":[{"name":"app","ready":false,"restart_count":12,"state":{"kind":"waiting","reason":"CrashLoopBackOff"},"last_termination":{"kind":"terminated","reason":"Error","exit_code":1}}]}]}`

// workloadAPI stubs mctl-api: pods and events answer as given (an empty body
// means 503), every other route answers {}.
func workloadAPI(t *testing.T, p *Pipeline, pods, events string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := `{}`
		switch {
		case strings.HasPrefix(r.URL.Path, "/api/v1/pods/billing/api"):
			body = pods
		case strings.HasPrefix(r.URL.Path, "/api/v1/events/billing"):
			body = events
		case strings.HasPrefix(r.URL.Path, "/api/v1/logs/"):
			body = `{"lines":[{"line":"level=error msg=\"db: connection refused\""}]}`
		}
		if body == "" {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	p.apiClient = mctlclient.NewClient(srv.URL, "test-token")
}

func crashloopTicket(t *testing.T, store *ticket.Store) *ticket.Ticket {
	t.Helper()
	tk := &ticket.Ticket{Source: ticket.SourceAlertManager, Type: ticket.TypePodCrashloop, Tenant: "billing", Service: "api", Summary: "crashloop", Status: ticket.StatusAnalyzing}
	if err := store.Create(context.Background(), tk); err != nil {
		t.Fatal(err)
	}
	return tk
}

func evidenceTypes(t *testing.T, store *ticket.Store, id string) map[string]string {
	t.Helper()
	got, err := store.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, e := range got.Evidence {
		out[e.Type] = e.Content
	}
	return out
}

// A crash-looping service is diagnosed from its pod state: the ticket ends
// escalated with the container, the exit code and the error line, not with
// "no skill matched".
//
// Mutation check: remove the pods block from collectEvidence and the skill no
// longer matches, so the analysis stays empty and this fails.
func TestCrashLoopIsDiagnosedFromPodState(t *testing.T) {
	p, store := newAsyncTestPipeline(t, 1)
	workloadAPI(t, p, crashingPods, `{"items":[{"reason":"BackOff","count":30}]}`)
	p.registry.Register(builtin.NewCrashLoopSkill())
	tk := crashloopTicket(t, store)

	p.processTicketSync(context.Background(), tk)

	ev := evidenceTypes(t, store, tk.ID)
	if ev["pods"] != crashingPods {
		t.Errorf("pods evidence = %q", ev["pods"])
	}
	if !strings.Contains(ev["events"], "BackOff") {
		t.Errorf("events evidence = %q", ev["events"])
	}
	got, err := store.Get(context.Background(), tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != ticket.StatusEscalated {
		t.Errorf("status = %q, want %q", got.Status, ticket.StatusEscalated)
	}
	for _, want := range []string{"Container app of pod billing-api-5c7-xk2", "12 restart(s), last exit code 1", "db: connection refused", "BackOff ×30"} {
		if !strings.Contains(got.Analysis, want) {
			t.Errorf("analysis lacks %q:\n%s", want, got.Analysis)
		}
	}
}

// When mctl-api cannot list pods or events, nothing is stored for them: no
// empty listing a skill could read as "no restarts".
//
// Mutation check: store the evidence regardless of the error and this fails.
func TestUnreadablePodStateLeavesNoEvidence(t *testing.T) {
	p, store := newAsyncTestPipeline(t, 1)
	workloadAPI(t, p, "", "")
	p.registry.Register(builtin.NewCrashLoopSkill())
	tk := crashloopTicket(t, store)

	p.processTicketSync(context.Background(), tk)

	ev := evidenceTypes(t, store, tk.ID)
	for _, typ := range []string{"pods", "events"} {
		if _, ok := ev[typ]; ok {
			t.Errorf("%s evidence stored from a failed read: %q", typ, ev[typ])
		}
	}
	if _, ok := ev["logs"]; !ok {
		t.Error("logs evidence missing; the stub no longer reaches collectEvidence")
	}
	got, err := store.Get(context.Background(), tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got.Analysis, "no skill matched") {
		t.Errorf("analysis = %q; an unreadable pod state must not be diagnosed", got.Analysis)
	}
}
