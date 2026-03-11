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

package remote

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mctlhq/mctl-agent/internal/skill"
	"github.com/mctlhq/mctl-agent/internal/ticket"
)

func TestRemoteSkillMatchAndDiagnose(t *testing.T) {
	// Start a mock remote skill server.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/match":
			_ = json.NewEncoder(w).Encode(matchResponse{
				Matched:    true,
				Confidence: 0.9,
				Priority:   100,
				Reason:     "test match",
			})
		case "/diagnose":
			_ = json.NewEncoder(w).Encode(diagnoseResponse{
				Diagnosis:  "Test diagnosis result",
				Confidence: "HIGH",
				Fixable:    true,
				FixType:    "test_fix",
			})
		case "/fix":
			_ = json.NewEncoder(w).Encode(fixResponse{
				Applied: true,
				Summary: "Applied test fix",
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	s := New(Registration{
		Name:        "test-remote",
		Version:     "1.0",
		Description: "Test remote skill",
		Endpoint:    srv.URL,
	})

	ctx := context.Background()
	tk := &ticket.Ticket{ID: "t1", Type: "pod_crashloop", Tenant: "billing", Service: "api"}
	ev := skill.NewEvidenceSet([]ticket.Evidence{
		{Type: "logs", Content: "some error"},
	})

	// Test Match.
	result := s.Match(ctx, tk, ev)
	if !result.Matched {
		t.Fatal("expected match")
	}
	if result.Confidence != 0.9 {
		t.Errorf("expected confidence 0.9, got %f", result.Confidence)
	}

	// Test Diagnose.
	diag, err := s.Diagnose(ctx, tk, ev)
	if err != nil {
		t.Fatal(err)
	}
	if diag.Diagnosis != "Test diagnosis result" {
		t.Errorf("unexpected diagnosis: %s", diag.Diagnosis)
	}
	if !diag.Fixable {
		t.Error("expected fixable")
	}

	// Test Fix.
	fix, err := s.Fix(ctx, tk, diag)
	if err != nil {
		t.Fatal(err)
	}
	if !fix.Applied {
		t.Error("expected Applied=true")
	}
	if fix.Summary != "Applied test fix" {
		t.Errorf("unexpected summary: %s", fix.Summary)
	}
}

func TestRemoteSkillFixAppliedFalse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/match":
			_ = json.NewEncoder(w).Encode(matchResponse{Matched: true, Confidence: 0.9})
		case "/diagnose":
			_ = json.NewEncoder(w).Encode(diagnoseResponse{
				Diagnosis:  "Test diagnosis",
				Confidence: "HIGH",
				Fixable:    true,
			})
		case "/fix":
			_ = json.NewEncoder(w).Encode(fixResponse{
				Applied: false,
				Summary: "Could not apply fix",
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	s := New(Registration{
		Name:     "test-not-applied",
		Version:  "1.0",
		Endpoint: srv.URL,
	})

	ctx := context.Background()
	tk := &ticket.Ticket{ID: "t2", Type: "pod_crashloop", Tenant: "billing", Service: "api"}
	ev := skill.NewEvidenceSet(nil)

	diag, err := s.Diagnose(ctx, tk, ev)
	if err != nil {
		t.Fatal(err)
	}

	fix, err := s.Fix(ctx, tk, diag)
	if err != nil {
		t.Fatal(err)
	}
	if fix.Applied {
		t.Error("expected Applied=false")
	}
	if fix.Summary != "Could not apply fix" {
		t.Errorf("unexpected summary: %s", fix.Summary)
	}
}

func TestRemoteSkillAlertTypeFilter(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(matchResponse{Matched: true, Confidence: 0.8})
	}))
	defer srv.Close()

	s := New(Registration{
		Name:       "filtered-skill",
		Version:    "1.0",
		Endpoint:   srv.URL,
		AlertTypes: []string{"pod_crashloop"},
	})

	ctx := context.Background()
	ev := skill.NewEvidenceSet(nil)

	// Matching alert type.
	tk := &ticket.Ticket{Type: "pod_crashloop"}
	result := s.Match(ctx, tk, ev)
	if !result.Matched {
		t.Error("expected match for pod_crashloop")
	}

	// Non-matching alert type — should be filtered before HTTP call.
	tk2 := &ticket.Ticket{Type: "workflow_failed"}
	result2 := s.Match(ctx, tk2, ev)
	if result2.Matched {
		t.Error("should not match workflow_failed")
	}
}

func TestRemoteSkillServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("internal error"))
	}))
	defer srv.Close()

	s := New(Registration{Name: "failing", Version: "1.0", Endpoint: srv.URL})
	ctx := context.Background()
	tk := &ticket.Ticket{Type: "pod_crashloop"}
	ev := skill.NewEvidenceSet(nil)

	// Match should return false on error.
	result := s.Match(ctx, tk, ev)
	if result.Matched {
		t.Error("should not match on server error")
	}

	// Diagnose should return error.
	_, err := s.Diagnose(ctx, tk, ev)
	if err == nil {
		t.Error("expected error from diagnose")
	}
}

func TestManagerRegisterAndList(t *testing.T) {
	reg := skill.NewRegistry()
	mgr := NewManager(reg)

	err := mgr.Register(Registration{
		Name:     "ext-skill",
		Version:  "2.0",
		Endpoint: "http://example.com",
	})
	if err != nil {
		t.Fatal(err)
	}

	list := mgr.List()
	if len(list) != 1 {
		t.Fatalf("expected 1 registered, got %d", len(list))
	}
	if list[0].Name != "ext-skill" {
		t.Errorf("unexpected name: %s", list[0].Name)
	}

	// Should be in the skill registry.
	if _, ok := reg.Get("ext-skill"); !ok {
		t.Error("skill not found in registry")
	}

	// Unregister.
	if !mgr.Unregister("ext-skill") {
		t.Error("expected successful unregister")
	}
	if mgr.Unregister("ext-skill") {
		t.Error("double unregister should return false")
	}
}

func TestManagerValidation(t *testing.T) {
	reg := skill.NewRegistry()
	mgr := NewManager(reg)

	if err := mgr.Register(Registration{Endpoint: "http://example.com"}); err == nil {
		t.Error("expected error for empty name")
	}
	if err := mgr.Register(Registration{Name: "test"}); err == nil {
		t.Error("expected error for empty endpoint")
	}
}
