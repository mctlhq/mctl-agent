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

package capability

import (
	"context"
	"testing"

	"github.com/mctlhq/mctl-agent/internal/fixer"
	"github.com/mctlhq/mctl-agent/internal/skill"
	"github.com/mctlhq/mctl-agent/internal/ticket"
)

// mockSkill implements skill.Skill with configurable capabilities.
type mockSkill struct {
	name string
	caps []skill.CapabilityID
}

func (s *mockSkill) Name() string        { return s.name }
func (s *mockSkill) Version() string      { return "1.0" }
func (s *mockSkill) Description() string  { return "test" }
func (s *mockSkill) RequiredCapabilities() []skill.CapabilityID { return s.caps }
func (s *mockSkill) Match(_ context.Context, _ *ticket.Ticket, _ skill.EvidenceSet) skill.MatchResult {
	return skill.MatchResult{}
}
func (s *mockSkill) Diagnose(_ context.Context, _ *ticket.Ticket, _ skill.EvidenceSet) (*skill.DiagnosisResult, error) {
	return nil, nil
}
func (s *mockSkill) Fix(_ context.Context, _ *ticket.Ticket, _ *skill.DiagnosisResult) (*skill.FixResult, error) {
	return nil, nil
}

func TestContextCapabilityEnforcement(t *testing.T) {
	// Skill only declares read_logs capability.
	s := &mockSkill{
		name: "limited",
		caps: []skill.CapabilityID{skill.CapReadLogs},
	}

	tk := &ticket.Ticket{Tenant: "team-a", Service: "app-1"}
	ev := skill.NewEvidenceSet(nil)

	// Provider is nil — we're testing access control, not actual calls.
	ctx := NewContext(nil, s, tk, ev)

	// CapReadLogs is allowed — check should pass (will panic on nil provider, which is fine).
	if err := ctx.check(skill.CapReadLogs); err != nil {
		t.Errorf("expected CapReadLogs to be allowed, got: %v", err)
	}

	// CapCreatePR is NOT allowed.
	if err := ctx.check(skill.CapCreatePR); err == nil {
		t.Error("expected CapCreatePR to be denied")
	}

	// CapReadStatus is NOT allowed.
	if err := ctx.check(skill.CapReadStatus); err == nil {
		t.Error("expected CapReadStatus to be denied")
	}

	// CapSendNotify is NOT allowed.
	if err := ctx.check(skill.CapSendNotify); err == nil {
		t.Error("expected CapSendNotify to be denied")
	}
}

func TestContextAllCapabilities(t *testing.T) {
	s := &mockSkill{
		name: "full-access",
		caps: []skill.CapabilityID{
			skill.CapReadLogs, skill.CapReadConfig, skill.CapReadStatus,
			skill.CapReadResources, skill.CapReadAudit,
			skill.CapModifyGitOps, skill.CapCreatePR,
			skill.CapSendNotify, skill.CapCallLLM,
		},
	}

	ctx := NewContext(nil, s, &ticket.Ticket{}, skill.NewEvidenceSet(nil))

	for _, cap := range s.caps {
		if err := ctx.check(cap); err != nil {
			t.Errorf("expected %s to be allowed, got: %v", cap, err)
		}
	}
}

func TestContextDeniedCapabilityMethods(t *testing.T) {
	s := &mockSkill{name: "read-only", caps: []skill.CapabilityID{skill.CapReadLogs}}
	ctx := NewContext(&Provider{}, s, &ticket.Ticket{}, skill.NewEvidenceSet(nil))

	// All write methods should fail.
	_, _, err := ctx.CreatePR(context.Background(), fixer.PRRequest{})
	if err == nil {
		t.Error("CreatePR should be denied")
	}

	err = ctx.SendNotification("test")
	if err == nil {
		t.Error("SendNotification should be denied")
	}

	_, err = ctx.GetFileContent(context.Background(), "test", "main")
	if err == nil {
		t.Error("GetFileContent should be denied")
	}

	// Read methods without the right capability should also fail.
	_, err = ctx.GetServiceStatus("t", "s")
	if err == nil {
		t.Error("GetServiceStatus should be denied without CapReadStatus")
	}

	_, err = ctx.GetResourceUsage("t")
	if err == nil {
		t.Error("GetResourceUsage should be denied without CapReadResources")
	}

	_, err = ctx.ListAudit()
	if err == nil {
		t.Error("ListAudit should be denied without CapReadAudit")
	}

	_, err = ctx.GetServiceConfig("t", "s")
	if err == nil {
		t.Error("GetServiceConfig should be denied without CapReadConfig")
	}

	// But GetServiceLogs should pass capability check (CapReadLogs is granted).
	// We can't actually call it with nil apiClient, so just verify capability check.
	if err := ctx.check(skill.CapReadLogs); err != nil {
		t.Error("GetServiceLogs capability check should pass with CapReadLogs")
	}
}
