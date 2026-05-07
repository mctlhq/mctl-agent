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

package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMetricsEndpoint(t *testing.T) {
	store := newTestStore(t)
	pipe := newTestPipeline(t, store)

	router := NewRouter(Options{
		Store:    store,
		Pipeline: pipe,
		OnAlert: func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		},
	})

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("/metrics: status = %d, want 200", w.Code)
	}

	ct := w.Header().Get("Content-Type")
	if !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("/metrics: Content-Type = %q, want prefix \"text/plain\"", ct)
	}

	body := w.Body.String()
	if !strings.Contains(body, "mctl_agent_stale_ttl_resolved_total") {
		t.Errorf("/metrics: body does not contain mctl_agent_stale_ttl_resolved_total; got:\n%s", body)
	}
}
