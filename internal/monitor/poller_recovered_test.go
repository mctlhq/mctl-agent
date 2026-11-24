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

package monitor

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mctlhq/mctl-agent/internal/mctlclient"
	"github.com/mctlhq/mctl-agent/internal/metrics"
	"github.com/mctlhq/mctl-agent/internal/ticket"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// argoAPI is a stub mctl-api for pollDegraded: one service, labs/web, whose
// status response the test chooses, plus a record of the incident ids the
// poller asked mctl-api to resolve.
type argoAPI struct {
	srv *httptest.Server

	mu       sync.Mutex
	resolved []string
}

// newArgoAPI serves servicesCode/servicesBody on the listing and
// statusCode/statusBody on labs/web's status.
func newArgoAPI(t *testing.T, servicesCode int, servicesBody string, statusCode int, statusBody string) *argoAPI {
	t.Helper()
	a := &argoAPI{}
	a.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/services":
			w.WriteHeader(servicesCode)
			_, _ = w.Write([]byte(servicesBody))
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/status/labs/web":
			w.WriteHeader(statusCode)
			_, _ = w.Write([]byte(statusBody))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/resolve"):
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v1/incidents/"), "/resolve")
			a.mu.Lock()
			a.resolved = append(a.resolved, id)
			a.mu.Unlock()
			_, _ = w.Write([]byte(`{}`))
		default:
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
		}
	}))
	t.Cleanup(a.srv.Close)
	return a
}

// awaitResolved waits for the asynchronous resolve propagation of id.
func (a *argoAPI) awaitResolved(t *testing.T, id string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		a.mu.Lock()
		for _, got := range a.resolved {
			if got == id {
				a.mu.Unlock()
				return
			}
		}
		a.mu.Unlock()
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("resolve of %s was not propagated to mctl-api", id)
}

const (
	oneService    = `{"items":[{"team":"labs","name":"web"}]}`
	healthySynced = `{"argocd":{"name":"labs-web","health":"Healthy","syncStatus":"Synced"}}`
)

// The poller opens argocd_app_degraded tickets and must close its own once it
// sees the application Healthy and Synced again; before this it never did, and
// a recovered app's ticket waited for a status TTL (ticket 288d5629).
//
// The table covers both directions. Every "stays" row is a way the pass could
// close something it must not: a reading that is not a recovery, a read that
// failed or came back partial (could not observe is not observed healthy), a
// ticket some other source owns, a ticket the pipeline or a PR still holds,
// and a ticket still inside the flap grace.
//
// Mutation check: each guard in pollDegraded / resolveRecovered, removed on
// its own, fails the row named after it.
func TestPollerResolvesOwnTicketsForARecoveredApp(t *testing.T) {
	type tc struct {
		name string
		// ticket under test
		source  string
		typ     string
		service string
		status  string
		pr      int
		prURL   string
		fresh   bool // leave UpdatedAt at now instead of backdating it
		// what mctl-api answers
		servicesCode int
		servicesBody string
		statusCode   int
		statusBody   string
		// poller config
		recoveredAfter time.Duration

		wantResolved bool
	}
	base := tc{
		source: ticket.SourcePolling, typ: ticket.TypeArgoCDDegraded, service: "web",
		status:       ticket.StatusEscalated,
		servicesCode: http.StatusOK, servicesBody: oneService,
		statusCode: http.StatusOK, statusBody: healthySynced,
		recoveredAfter: DefaultRecoveredAfter,
	}
	with := func(name string, wantResolved bool, mod func(*tc)) tc {
		c := base
		c.name, c.wantResolved = name, wantResolved
		if mod != nil {
			mod(&c)
		}
		return c
	}

	cases := []tc{
		with("escalated ticket, app healthy and synced", true, nil),
		with("open ticket, app healthy and synced", true, func(c *tc) { c.status = ticket.StatusOpen }),

		// Not a recovery.
		with("healthy but out of sync stays", false, func(c *tc) {
			c.statusBody = `{"argocd":{"health":"Healthy","syncStatus":"OutOfSync"}}`
		}),
		with("synced but progressing stays", false, func(c *tc) {
			c.statusBody = `{"argocd":{"health":"Progressing","syncStatus":"Synced"}}`
		}),
		with("healthy with no sync status stays", false, func(c *tc) {
			c.statusBody = `{"argocd":{"health":"Healthy"}}`
		}),
		with("synced with no health stays", false, func(c *tc) {
			c.statusBody = `{"argocd":{"syncStatus":"Synced"}}`
		}),
		with("still degraded stays", false, func(c *tc) {
			c.statusBody = `{"argocd":{"health":"Degraded","syncStatus":"Synced"}}`
		}),

		// Could not observe.
		with("status read fails stays", false, func(c *tc) { c.statusCode = http.StatusInternalServerError }),
		with("status without an argocd block stays", false, func(c *tc) { c.statusBody = `{}` }),
		with("malformed status body stays", false, func(c *tc) { c.statusBody = `{"argocd":{"health":"Healthy","syncSta` }),
		with("service listing fails stays", false, func(c *tc) { c.servicesCode = http.StatusBadGateway }),
		with("service missing from the listing stays", false, func(c *tc) { c.servicesBody = `{"items":[]}` }),

		// Not the poller's ticket for this app.
		with("alertmanager ticket stays", false, func(c *tc) { c.source = ticket.SourceAlertManager }),
		with("manual ticket stays", false, func(c *tc) { c.source = ticket.SourceManual }),
		with("another ticket type stays", false, func(c *tc) { c.typ = ticket.TypePodCrashloop }),
		with("another service stays", false, func(c *tc) { c.service = "worker" }),

		// Held by the pipeline or by a pull request.
		with("analyzing ticket stays", false, func(c *tc) { c.status = ticket.StatusAnalyzing }),
		with("fix_proposed ticket with a PR stays", false, func(c *tc) {
			c.status, c.pr, c.prURL = ticket.StatusFixProposed, 7, "https://github.com/o/r/pull/7"
		}),
		with("fix_applied ticket stays", false, func(c *tc) {
			c.status, c.pr, c.prURL = ticket.StatusFixApplied, 7, "https://github.com/o/r/pull/7"
		}),
		with("escalated ticket with a PR number stays", false, func(c *tc) { c.pr = 7 }),
		with("escalated ticket with a PR url stays", false, func(c *tc) { c.prURL = "https://github.com/o/r/pull/7" }),

		// Flap grace and the off switch.
		with("ticket touched within the grace stays", false, func(c *tc) { c.fresh = true }),
		with("pass disabled stays", false, func(c *tc) { c.recoveredAfter = 0 }),
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			api := newArgoAPI(t, c.servicesCode, c.servicesBody, c.statusCode, c.statusBody)
			store := newTestStore(t)
			p := NewPoller(mctlclient.NewClient(api.srv.URL, ""), store, nil)
			p.RecoveredAfter = c.recoveredAfter

			tk := &ticket.Ticket{
				Source: c.source, Type: c.typ, Tenant: "labs", Service: c.service,
				Summary: "ArgoCD app labs-web health: Degraded", Severity: ticket.SeverityWarning,
			}
			if err := store.Create(ctx, tk); err != nil {
				t.Fatal(err)
			}
			tk.Status, tk.PRNumber, tk.PRURL = c.status, c.pr, c.prURL
			tk.Analysis = "diagnosed"
			if err := store.Update(ctx, tk); err != nil {
				t.Fatal(err)
			}
			if !c.fresh {
				backdate(t, store, tk.ID, time.Now().UTC().Add(-time.Hour))
			}

			before := testutil.ToFloat64(metrics.ArgoRecoveredResolved)
			p.pollDegraded(ctx)

			got, err := store.Get(ctx, tk.ID)
			if err != nil {
				t.Fatal(err)
			}
			resolved := got.Status == ticket.StatusResolved
			if resolved != c.wantResolved {
				t.Fatalf("status = %q, want resolved=%v", got.Status, c.wantResolved)
			}
			delta := testutil.ToFloat64(metrics.ArgoRecoveredResolved) - before
			if !c.wantResolved {
				if got.Status != c.status {
					t.Errorf("status = %q, want it left at %q", got.Status, c.status)
				}
				if delta != 0 {
					t.Errorf("recovered-resolved counter moved by %v for a ticket that stayed open", delta)
				}
				return
			}
			if got.ResolvedAt == nil {
				t.Error("resolved ticket has no resolved_at")
			}
			if !strings.Contains(got.Analysis, "diagnosed") || !strings.Contains(got.Analysis, "Healthy and Synced") {
				t.Errorf("analysis = %q, want the diagnosis kept and the recovery reason appended", got.Analysis)
			}
			if delta != 1 {
				t.Errorf("recovered-resolved counter moved by %v, want 1", delta)
			}
			api.awaitResolved(t, tk.ID)
		})
	}
}

// Two of the poller's tickets, one application recovered: only that
// application's ticket closes, and the one still degraded is touched instead.
func TestPollerRecoveredAppDoesNotCloseAnotherAppsTicket(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/services":
			_, _ = w.Write([]byte(`{"items":[{"team":"labs","name":"web"},{"team":"labs","name":"worker"}]}`))
		case "/api/v1/status/labs/web":
			_, _ = w.Write([]byte(healthySynced))
		case "/api/v1/status/labs/worker":
			_, _ = w.Write([]byte(`{"argocd":{"health":"Degraded","syncStatus":"Synced"}}`))
		default:
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	t.Cleanup(srv.Close)
	store := newTestStore(t)
	p := NewPoller(mctlclient.NewClient(srv.URL, ""), store, nil)

	ids := map[string]string{}
	for _, svc := range []string{"web", "worker"} {
		tk := &ticket.Ticket{
			Source: ticket.SourcePolling, Type: ticket.TypeArgoCDDegraded,
			Tenant: "labs", Service: svc, Summary: "degraded", Severity: ticket.SeverityWarning,
		}
		if err := store.Create(ctx, tk); err != nil {
			t.Fatal(err)
		}
		tk.Status = ticket.StatusEscalated
		if err := store.Update(ctx, tk); err != nil {
			t.Fatal(err)
		}
		backdate(t, store, tk.ID, time.Now().UTC().Add(-time.Hour))
		ids[svc] = tk.ID
	}

	p.pollDegraded(ctx)

	if got, _ := store.Get(ctx, ids["web"]); got.Status != ticket.StatusResolved {
		t.Errorf("recovered app's ticket: status = %q, want resolved", got.Status)
	}
	got, _ := store.Get(ctx, ids["worker"])
	if got.Status != ticket.StatusEscalated {
		t.Errorf("degraded app's ticket: status = %q, want it left escalated", got.Status)
	}
	if time.Since(got.UpdatedAt) > time.Minute {
		t.Errorf("degraded app's ticket was not touched: updated_at = %s", got.UpdatedAt)
	}
}

// A ticket list that could not be read is unknown, not empty: the cycle
// records the failure and resolves nothing from it.
func TestOwnDegradedTicketsLoadFailureIsNotAnEmptySet(t *testing.T) {
	store := newTestStore(t)
	_ = store.Close()

	var own ownDegradedTickets
	own.load(ctx, store)
	if !own.failed {
		t.Fatal("a failed ListOpen must be recorded as failed")
	}
	if own.byService != nil {
		t.Errorf("a failed read must not produce a ticket set, got %v", own.byService)
	}
}
