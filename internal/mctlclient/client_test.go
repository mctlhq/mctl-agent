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
