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
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
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
	got, err := c.ListServices()
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
	c.ResolveAlert("ticket-abc-123", "auto-resolved: stale TTL GC")

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
	c.ResolveAlert("ticket-abc-123", "reason") // must not panic, must not block.
}
