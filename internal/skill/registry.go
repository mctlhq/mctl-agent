package skill

import (
	"context"
	"log/slog"
	"sort"
	"sync"

	"github.com/mctlhq/mctl-agent/internal/ticket"
)

// RankedSkill pairs a skill with its match result for priority sorting.
type RankedSkill struct {
	Skill  Skill
	Result MatchResult
}

// Registry holds all registered skills and matches them against tickets.
type Registry struct {
	mu       sync.RWMutex
	skills   []Skill
	disabled map[string]bool
}

// NewRegistry creates an empty skill registry.
func NewRegistry() *Registry {
	return &Registry{
		disabled: make(map[string]bool),
	}
}

// Register adds a skill to the registry.
func (r *Registry) Register(s Skill) {
	r.mu.Lock()
	defer r.mu.Unlock()

	// Replace existing skill with same name.
	for i, existing := range r.skills {
		if existing.Name() == s.Name() {
			r.skills[i] = s
			slog.Info("skill replaced", "name", s.Name(), "version", s.Version())
			return
		}
	}

	r.skills = append(r.skills, s)
	slog.Info("skill registered", "name", s.Name(), "version", s.Version())
}

// Match returns all skills that match the given ticket, ranked by confidence
// (descending) then priority (descending). Disabled skills are excluded.
func (r *Registry) Match(ctx context.Context, t *ticket.Ticket, ev EvidenceSet) []RankedSkill {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var ranked []RankedSkill
	for _, s := range r.skills {
		if r.disabled[s.Name()] {
			continue
		}

		result := s.Match(ctx, t, ev)
		if result.Matched {
			ranked = append(ranked, RankedSkill{Skill: s, Result: result})
		}
	}

	sort.Slice(ranked, func(i, j int) bool {
		if ranked[i].Result.Confidence != ranked[j].Result.Confidence {
			return ranked[i].Result.Confidence > ranked[j].Result.Confidence
		}
		return ranked[i].Result.Priority > ranked[j].Result.Priority
	})

	return ranked
}

// Get returns a skill by name.
func (r *Registry) Get(name string) (Skill, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	for _, s := range r.skills {
		if s.Name() == name {
			return s, true
		}
	}
	return nil, false
}

// List returns metadata for all registered skills.
func (r *Registry) List() []Info {
	r.mu.RLock()
	defer r.mu.RUnlock()

	infos := make([]Info, 0, len(r.skills))
	for _, s := range r.skills {
		infos = append(infos, Info{
			Name:         s.Name(),
			Version:      s.Version(),
			Description:  s.Description(),
			Capabilities: s.RequiredCapabilities(),
			Enabled:      !r.disabled[s.Name()],
		})
	}
	return infos
}

// Disable prevents a skill from being matched.
func (r *Registry) Disable(name string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	for _, s := range r.skills {
		if s.Name() == name {
			r.disabled[name] = true
			slog.Info("skill disabled", "name", name)
			return true
		}
	}
	return false
}

// Enable re-enables a previously disabled skill.
func (r *Registry) Enable(name string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.disabled[name] {
		delete(r.disabled, name)
		slog.Info("skill enabled", "name", name)
		return true
	}
	return false
}

// IsEnabled checks if a skill is enabled.
func (r *Registry) IsEnabled(name string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return !r.disabled[name]
}

// Count returns the total number of registered skills.
func (r *Registry) Count() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.skills)
}
