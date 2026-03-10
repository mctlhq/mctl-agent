package skill

import (
	"context"
	"testing"

	"github.com/mctlhq/mctl-agent/internal/ticket"
)

// testSkill is a minimal Skill implementation for testing.
type testSkill struct {
	name        string
	version     string
	desc        string
	matchResult MatchResult
	diagResult  *DiagnosisResult
	diagErr     error
	fixResult   *FixResult
	fixErr      error
	caps        []CapabilityID
}

func (s *testSkill) Name() string        { return s.name }
func (s *testSkill) Version() string      { return s.version }
func (s *testSkill) Description() string  { return s.desc }
func (s *testSkill) RequiredCapabilities() []CapabilityID { return s.caps }

func (s *testSkill) Match(_ context.Context, _ *ticket.Ticket, _ EvidenceSet) MatchResult {
	return s.matchResult
}

func (s *testSkill) Diagnose(_ context.Context, _ *ticket.Ticket, _ EvidenceSet) (*DiagnosisResult, error) {
	return s.diagResult, s.diagErr
}

func (s *testSkill) Fix(_ context.Context, _ *ticket.Ticket, _ *DiagnosisResult) (*FixResult, error) {
	return s.fixResult, s.fixErr
}

func TestRegistryRegisterAndList(t *testing.T) {
	r := NewRegistry()

	s1 := &testSkill{name: "skill-a", version: "1.0", desc: "Skill A"}
	s2 := &testSkill{name: "skill-b", version: "2.0", desc: "Skill B"}

	r.Register(s1)
	r.Register(s2)

	if r.Count() != 2 {
		t.Fatalf("expected 2 skills, got %d", r.Count())
	}

	infos := r.List()
	if len(infos) != 2 {
		t.Fatalf("expected 2 infos, got %d", len(infos))
	}
	if infos[0].Name != "skill-a" || infos[1].Name != "skill-b" {
		t.Errorf("unexpected info order: %v", infos)
	}
}

func TestRegistryReplaceExisting(t *testing.T) {
	r := NewRegistry()

	r.Register(&testSkill{name: "s", version: "1.0"})
	r.Register(&testSkill{name: "s", version: "2.0"})

	if r.Count() != 1 {
		t.Fatalf("expected 1 skill after replace, got %d", r.Count())
	}

	s, ok := r.Get("s")
	if !ok {
		t.Fatal("skill not found")
	}
	if s.Version() != "2.0" {
		t.Errorf("expected version 2.0, got %s", s.Version())
	}
}

func TestRegistryMatch(t *testing.T) {
	r := NewRegistry()

	r.Register(&testSkill{
		name:        "low",
		matchResult: MatchResult{Matched: true, Confidence: 0.3, Priority: 1},
	})
	r.Register(&testSkill{
		name:        "high",
		matchResult: MatchResult{Matched: true, Confidence: 0.9, Priority: 1},
	})
	r.Register(&testSkill{
		name:        "no-match",
		matchResult: MatchResult{Matched: false},
	})

	ctx := context.Background()
	tk := &ticket.Ticket{ID: "test-1", Type: ticket.TypePodCrashloop}
	ev := NewEvidenceSet(nil)

	ranked := r.Match(ctx, tk, ev)
	if len(ranked) != 2 {
		t.Fatalf("expected 2 matches, got %d", len(ranked))
	}
	if ranked[0].Skill.Name() != "high" {
		t.Errorf("expected 'high' first, got %s", ranked[0].Skill.Name())
	}
	if ranked[1].Skill.Name() != "low" {
		t.Errorf("expected 'low' second, got %s", ranked[1].Skill.Name())
	}
}

func TestRegistryMatchSameConfidencePriority(t *testing.T) {
	r := NewRegistry()

	r.Register(&testSkill{
		name:        "low-pri",
		matchResult: MatchResult{Matched: true, Confidence: 0.8, Priority: 1},
	})
	r.Register(&testSkill{
		name:        "high-pri",
		matchResult: MatchResult{Matched: true, Confidence: 0.8, Priority: 10},
	})

	ranked := r.Match(context.Background(), &ticket.Ticket{}, NewEvidenceSet(nil))
	if len(ranked) != 2 {
		t.Fatalf("expected 2, got %d", len(ranked))
	}
	if ranked[0].Skill.Name() != "high-pri" {
		t.Errorf("expected 'high-pri' first, got %s", ranked[0].Skill.Name())
	}
}

func TestRegistryDisableEnable(t *testing.T) {
	r := NewRegistry()

	r.Register(&testSkill{
		name:        "s",
		matchResult: MatchResult{Matched: true, Confidence: 0.9},
	})

	// Should match initially.
	ranked := r.Match(context.Background(), &ticket.Ticket{}, NewEvidenceSet(nil))
	if len(ranked) != 1 {
		t.Fatalf("expected 1 match, got %d", len(ranked))
	}

	// Disable.
	if !r.Disable("s") {
		t.Fatal("Disable returned false")
	}
	if r.IsEnabled("s") {
		t.Error("expected skill to be disabled")
	}

	ranked = r.Match(context.Background(), &ticket.Ticket{}, NewEvidenceSet(nil))
	if len(ranked) != 0 {
		t.Fatalf("expected 0 matches after disable, got %d", len(ranked))
	}

	// Re-enable.
	if !r.Enable("s") {
		t.Fatal("Enable returned false")
	}
	ranked = r.Match(context.Background(), &ticket.Ticket{}, NewEvidenceSet(nil))
	if len(ranked) != 1 {
		t.Fatalf("expected 1 match after enable, got %d", len(ranked))
	}
}

func TestEvidenceSet(t *testing.T) {
	evs := []ticket.Evidence{
		{Type: "logs", Content: "some logs here"},
		{Type: "config", Content: `{"port": 8080}`},
	}
	es := NewEvidenceSet(evs)

	if !es.Has("logs") {
		t.Error("expected logs evidence")
	}
	if es.Has("missing") {
		t.Error("unexpected evidence type")
	}
	if es.Get("logs") != "some logs here" {
		t.Errorf("unexpected logs content: %s", es.Get("logs"))
	}
	if es.Get("missing") != "" {
		t.Error("expected empty string for missing evidence")
	}

	all := es.All()
	if len(all) != 2 {
		t.Errorf("expected 2 entries, got %d", len(all))
	}
}
