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
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestAMClientActiveFingerprints_ThreeAlerts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[
			{"fingerprint":"aaa","status":{"state":"active"}},
			{"fingerprint":"bbb","status":{"state":"active"}},
			{"fingerprint":"ccc","status":{"state":"active"}}
		]`))
	}))
	defer srv.Close()

	c := &AlertManagerClient{BaseURL: srv.URL, Timeout: 5 * time.Second, HTTP: srv.Client()}
	got, err := c.ActiveFingerprints(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("want 3 fingerprints, got %d", len(got))
	}
	for _, fp := range []string{"aaa", "bbb", "ccc"} {
		if _, ok := got[fp]; !ok {
			t.Errorf("missing fingerprint %q", fp)
		}
	}
}

func TestAMClientActiveFingerprints_EmptyArray(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()

	c := &AlertManagerClient{BaseURL: srv.URL, Timeout: 5 * time.Second, HTTP: srv.Client()}
	got, err := c.ActiveFingerprints(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("want empty set, got %d", len(got))
	}
}

func TestAMClientActiveFingerprints_NonOKStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "internal error", http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := &AlertManagerClient{BaseURL: srv.URL, Timeout: 5 * time.Second, HTTP: srv.Client()}
	got, err := c.ActiveFingerprints(context.Background())
	if err == nil {
		t.Fatal("expected error on non-2xx response")
	}
	if got != nil {
		t.Fatalf("expected nil result on error, got %v", got)
	}
}

func TestAMClientActiveFingerprints_MalformedJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`not json`))
	}))
	defer srv.Close()

	c := &AlertManagerClient{BaseURL: srv.URL, Timeout: 5 * time.Second, HTTP: srv.Client()}
	got, err := c.ActiveFingerprints(context.Background())
	if err == nil {
		t.Fatal("expected error on malformed JSON")
	}
	if got != nil {
		t.Fatalf("expected nil result on error, got %v", got)
	}
}

func TestAMClientActiveFingerprints_ContextDeadline(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Block until context is cancelled.
		<-r.Context().Done()
	}))
	defer srv.Close()

	c := &AlertManagerClient{BaseURL: srv.URL, Timeout: 50 * time.Millisecond, HTTP: srv.Client()}
	got, err := c.ActiveFingerprints(context.Background())
	if err == nil {
		t.Fatal("expected error on context deadline")
	}
	if got != nil {
		t.Fatalf("expected nil result on error, got %v", got)
	}
}

func TestAMClientActiveFingerprints_OnlyActiveIncluded(t *testing.T) {
	// Suppressed/inactive alerts should be excluded
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[
			{"fingerprint":"active1","status":{"state":"active"}},
			{"fingerprint":"suppressed1","status":{"state":"suppressed"}},
			{"fingerprint":"","status":{"state":"active"}}
		]`))
	}))
	defer srv.Close()

	c := &AlertManagerClient{BaseURL: srv.URL, Timeout: 5 * time.Second, HTTP: srv.Client()}
	got, err := c.ActiveFingerprints(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 fingerprint (only active non-empty), got %d: %v", len(got), got)
	}
	if _, ok := got["active1"]; !ok {
		t.Error("missing active1")
	}
}
