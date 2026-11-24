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

package mctlclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mctlhq/mctl-agent/internal/ticket"
)

// TestServiceJSONDecodesAppFromNameField verifies that mctl-api's
// `name` field correctly populates Service.App. Before this fix the
// Go struct had `App json:"app"` while mctl-api emits `name`, so
// every decoded Service had App="" and pollDegraded skipped every
// service in the inventory — silently disabling pollDegraded's
// ArgoCDDegraded detection and Phase 3's orphan pruning.
func TestServiceJSONDecodesAppFromNameField(t *testing.T) {
	payload := `{"team":"admins","name":"mctl-docs","imageTag":"0.1.17","componentType":"base-service","hasDatabase":false}`

	var svc Service
	if err := json.Unmarshal([]byte(payload), &svc); err != nil {
		t.Fatal(err)
	}
	if svc.Team != "admins" {
		t.Errorf("Team: want %q, got %q", "admins", svc.Team)
	}
	if svc.App != "mctl-docs" {
		t.Errorf("App: want %q, got %q (mctl-api emits the service identifier under \"name\"; check the json tag on Service.App)",
			"mctl-docs", svc.App)
	}
}

// TestListServicesParsesItems exercises the full ListServices code
// path against an httptest server returning the same shape that
// mctl-api uses today. This guards against future drift where
// mctl-api renames the field again — the test will fail fast.
func TestListServicesParsesItems(t *testing.T) {
	const body = `{
		"count": 3,
		"items": [
			{"team":"admins","name":"mctl-docs"},
			{"team":"labs","name":"openclaw"},
			{"team":"ovk","name":"openclaw"}
		]
	}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got, want := r.Header.Get("Authorization"), "Bearer test-token"; got != want {
			t.Errorf("Authorization header: want %q, got %q", want, got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "test-token")
	got, err := c.ListServices(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("want 3 services, got %d: %+v", len(got), got)
	}
	for _, s := range got {
		if s.Team == "" || s.App == "" {
			t.Errorf("service has empty Team or App: %+v", s)
		}
	}
}

// TestResolveAlertHitsCorrectEndpoint pins the resolve-propagation
// path: AlertManager `resolved` events reach mctl-api via
// POST /api/v1/incidents/{id}/resolve. A drift here (wrong path,
// wrong method, missing Bearer) silently leaves mctl-api alerts
// `open` forever — the exact bug that produced 198 stale rows.
func TestResolveAlertHitsCorrectEndpoint(t *testing.T) {
	var (
		gotMethod string
		gotPath   string
		gotAuth   string
		gotReason string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		var body struct {
			Reason string `json:"reason"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotReason = body.Reason
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "test-token")
	_ = c.ResolveAlert(context.Background(), "ticket-abc-123", "auto-resolved: stale TTL GC")

	if gotMethod != http.MethodPost {
		t.Errorf("method: want POST, got %q", gotMethod)
	}
	if want := "/api/v1/incidents/ticket-abc-123/resolve"; gotPath != want {
		t.Errorf("path: want %q, got %q", want, gotPath)
	}
	if want := "Bearer test-token"; gotAuth != want {
		t.Errorf("auth: want %q, got %q", want, gotAuth)
	}
	if want := "auto-resolved: stale TTL GC"; gotReason != want {
		t.Errorf("reason: want %q, got %q", want, gotReason)
	}
}

// TestResolveAlertSwallowsServerError verifies the AlertManager
// webhook path is never blocked by a flaky mctl-api: errors must be
// logged (in the real binary) and returned as no-ops to the caller.
func TestResolveAlertSwallowsServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"boom"}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "test-token")
	if err := c.ResolveAlert(context.Background(), "ticket-abc-123", "reason"); err == nil {
		t.Error("server 500 must surface as an error, not be reported as delivered")
	}
}

// PublishAlert and UpdateAlert carry the ticket's trace id when it has one
// and omit the key entirely when it does not, so mctl-api never stores an empty id that
// reads like data (mctlhq/mctl-agent#97).
//
// Mutation check: send trace_id unconditionally, or drop it, from either
// call and this fails.
func TestPublishAlertTraceID(t *testing.T) {
	for name, tc := range map[string]struct {
		traceID string
		want    any
		present bool
	}{
		"traced":   {"164ac97d8d7264326c406240d263dcc1", "164ac97d8d7264326c406240d263dcc1", true},
		"untraced": {"", nil, false},
	} {
		t.Run(name, func(t *testing.T) {
			var body map[string]any
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewDecoder(r.Body).Decode(&body)
				_, _ = w.Write([]byte(`{}`))
			}))
			defer srv.Close()

			c := NewClient(srv.URL, "test-token")
			for call, send := range map[string]func(*ticket.Ticket){"PublishAlert": c.PublishAlert, "UpdateAlert": c.UpdateAlert} {
				body = nil
				send(&ticket.Ticket{ID: "tk-1", TraceID: tc.traceID})
				got, ok := body["trace_id"]
				if ok != tc.present || got != tc.want {
					t.Errorf("%s: trace_id = %v (present %v), want %v (present %v)", call, got, ok, tc.want, tc.present)
				}
				if _, ok := body["span_id"]; ok {
					t.Errorf("%s: span_id must not be published", call)
				}
			}
		})
	}
}

// ListAudit must decode mctl-api's field names. The client used to read
// "action" and "target", which the API never sends, so every entry came back
// blank.
func TestListAuditDecodesAPISchema(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/audit" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"items":[{"id":"a1","timestamp":"2026-11-14T10:00:00Z","userId":"ci:github",` +
			`"operation":"deploy-service","parameters":{"team_name":"acme","component_name":"api"},` +
			`"workflowName":"deploy-service-x1","status":"submitted","riskLevel":"medium"}],"count":1}`))
	}))
	defer srv.Close()

	entries, err := NewClient(srv.URL, "tok").ListAudit(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(entries))
	}
	e := entries[0]
	if e.Operation != "deploy-service" || e.UserID != "ci:github" || e.Status != "submitted" ||
		e.Parameters["team_name"] != "acme" || e.Parameters["component_name"] != "api" ||
		e.WorkflowName != "deploy-service-x1" || e.Timestamp.IsZero() {
		t.Fatalf("decoded entry = %+v", e)
	}
}

// A stalled mctl-api must not pin the caller: the 10s budget (shrunk here)
// has to reach the HTTP request itself.
func TestResolveAlertTimesOutOnStalledServer(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release)

	old := resolveTimeout
	resolveTimeout = 100 * time.Millisecond
	defer func() { resolveTimeout = old }()

	c := NewClient(srv.URL, "t")
	start := time.Now()
	err := c.ResolveAlert(context.Background(), "stuck", "r")
	if err == nil {
		t.Fatal("expected a timeout error")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want deadline exceeded", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("returned after %v, want about 100ms", d)
	}
}

// One failing id is logged with alert_id and must not abort the rest.
func TestResolveAlertsLogsFailureAndContinues(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.URL.Path)
		mu.Unlock()
		if strings.Contains(r.URL.Path, "/bad/") {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(prev)

	c := NewClient(srv.URL, "t")
	failed := c.ResolveAlerts(context.Background(), []string{"a", "bad", "c"}, "r")

	if failed != 1 {
		t.Errorf("failed = %d, want 1", failed)
	}
	want := []string{"/api/v1/incidents/a/resolve", "/api/v1/incidents/bad/resolve", "/api/v1/incidents/c/resolve"}
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(seen, ",") != strings.Join(want, ",") {
		t.Errorf("requests = %v, want %v (sequential, none skipped)", seen, want)
	}
	if out := buf.String(); !strings.Contains(out, "alert_id=bad") || !strings.Contains(out, "level=ERROR") {
		t.Errorf("log missing ERROR with alert_id=bad: %q", out)
	}
}

// The pod and event reads hit mctl-api's workload routes, ask only for the
// service's own Warning events, and hand the body back untouched.
//
// Mutation check: drop the app or type parameter, or change a path, and this
// fails.
func TestGetServicePodsAndEvents(t *testing.T) {
	var gotPaths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPaths = append(gotPaths, r.URL.Path+"?"+r.URL.Query().Encode())
		_, _ = w.Write([]byte(`{"items":[{"name":"x"}]}`))
	}))
	defer srv.Close()
	c := NewClient(srv.URL, "tok")

	pods, err := c.GetServicePods(context.Background(), "labs", "mctl-telegram")
	if err != nil {
		t.Fatal(err)
	}
	events, err := c.ListServiceEvents(context.Background(), "labs", "mctl-telegram")
	if err != nil {
		t.Fatal(err)
	}
	if string(pods) != `{"items":[{"name":"x"}]}` || string(events) != string(pods) {
		t.Errorf("bodies altered: %s / %s", pods, events)
	}
	want := []string{
		"/api/v1/pods/labs/mctl-telegram?",
		"/api/v1/events/labs?app=mctl-telegram&since=1h&type=Warning",
	}
	if len(gotPaths) != 2 || gotPaths[0] != want[0] || gotPaths[1] != want[1] {
		t.Errorf("requests = %q, want %q", gotPaths, want)
	}
}

// A failed or malformed read is an error, never an empty listing: the caller
// stores no evidence for it.
func TestGetServicePodsAndEventsFailClosed(t *testing.T) {
	for name, h := range map[string]http.HandlerFunc{
		"503":      func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) },
		"not JSON": func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"items":[`)) },
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(h)
			defer srv.Close()
			c := NewClient(srv.URL, "tok")
			if body, err := c.GetServicePods(context.Background(), "labs", "web"); err == nil {
				t.Errorf("GetServicePods returned %q without an error", body)
			}
			if body, err := c.ListServiceEvents(context.Background(), "labs", "web"); err == nil {
				t.Errorf("ListServiceEvents returned %q without an error", body)
			}
		})
	}
}

// Every read carries the caller's context: once it is cancelled or past its
// deadline the call returns with that error and no result, while mctl-api is
// still holding the request open. Without this a diagnosis that has been
// given up on keeps its slot until the client's own 15s timeout, read after
// read.
//
// Mutation check: build the request in doGet with context.Background(), or
// hand doGet context.Background() in any one method, and that method's case
// fails with "still waiting".
func TestReadsStopWhenContextEnds(t *testing.T) {
	reads := map[string]func(context.Context, *Client) (any, error){
		"ListServices": func(ctx context.Context, c *Client) (any, error) { return c.ListServices(ctx) },
		"GetServiceStatus": func(ctx context.Context, c *Client) (any, error) {
			return c.GetServiceStatus(ctx, "labs", "web")
		},
		"GetServiceConfig": func(ctx context.Context, c *Client) (any, error) {
			return c.GetServiceConfig(ctx, "labs", "web")
		},
		"GetServiceLogs": func(ctx context.Context, c *Client) (any, error) {
			return c.GetServiceLogs(ctx, "labs", "web", 100, time.Hour)
		},
		"GetServicePods": func(ctx context.Context, c *Client) (any, error) {
			return c.GetServicePods(ctx, "labs", "web")
		},
		"ListServiceEvents": func(ctx context.Context, c *Client) (any, error) {
			return c.ListServiceEvents(ctx, "labs", "web")
		},
		"GetResourceUsage": func(ctx context.Context, c *Client) (any, error) { return c.GetResourceUsage(ctx, "labs") },
		"ListAudit":        func(ctx context.Context, c *Client) (any, error) { return c.ListAudit(ctx) },
		"GetWorkflow":      func(ctx context.Context, c *Client) (any, error) { return c.GetWorkflow(ctx, "wf-1") },
	}
	ends := map[string]struct {
		// end makes ctx finish once the request has reached the server.
		end  func(context.Context) (context.Context, context.CancelFunc)
		want error
	}{
		"cancelled": {context.WithCancel, context.Canceled},
		"deadline": {func(ctx context.Context) (context.Context, context.CancelFunc) {
			return context.WithTimeout(ctx, 50*time.Millisecond)
		}, context.DeadlineExceeded},
	}
	for name, read := range reads {
		for endName, e := range ends {
			t.Run(name+"/"+endName, func(t *testing.T) {
				arrived := make(chan struct{}, 1)
				release := make(chan struct{})
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					arrived <- struct{}{}
					select {
					case <-r.Context().Done():
					case <-release:
						// Answered late, with a body every read would accept.
						_, _ = w.Write([]byte(`{"items":[]}`))
					}
				}))
				defer srv.Close()
				defer close(release)

				ctx, cancel := e.end(context.Background())
				defer cancel()
				type result struct {
					got any
					err error
				}
				done := make(chan result, 1)
				go func() {
					got, err := read(ctx, NewClient(srv.URL, "tok"))
					done <- result{got, err}
				}()
				select {
				case <-arrived:
				case <-time.After(5 * time.Second):
					t.Fatal("the request never reached the server")
				}
				if e.want == context.Canceled {
					cancel()
				}
				select {
				case r := <-done:
					if !errors.Is(r.err, e.want) {
						t.Errorf("err = %v, want %v", r.err, e.want)
					}
					if r.err == nil {
						t.Errorf("returned %v from a read that was abandoned", r.got)
					}
				case <-time.After(3 * time.Second):
					t.Errorf("still waiting on mctl-api 3s after the context ended")
				}
			})
		}
	}
}
