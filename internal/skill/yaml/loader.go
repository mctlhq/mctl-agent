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

// Package yaml implements config-driven skills loaded from YAML files.
//
// YAML skills support simple pattern-matching triggers, template-based
// diagnosis, and optional notification context — no Go code required.
package yaml

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"text/template"

	"github.com/mctlhq/mctl-agent/internal/skill"
	"github.com/mctlhq/mctl-agent/internal/ticket"
	"gopkg.in/yaml.v3"
)

// SkillDef is the YAML schema for a config-driven skill.
type SkillDef struct {
	Name        string   `yaml:"name"`
	Version     string   `yaml:"version"`
	Description string   `yaml:"description"`
	Trigger     Trigger  `yaml:"trigger"`
	Diagnosis   Diag     `yaml:"diagnosis"`
	Notification *Notify `yaml:"notification,omitempty"`
	Capabilities []string `yaml:"capabilities,omitempty"`
}

// Trigger defines when this skill activates.
type Trigger struct {
	AlertTypes  []string `yaml:"alert_types,omitempty"`
	LogPatterns []string `yaml:"log_patterns,omitempty"`
	MinRestarts int      `yaml:"min_restarts,omitempty"`
}

// Diag defines how this skill diagnoses the problem.
type Diag struct {
	Template   string `yaml:"template"`
	Confidence string `yaml:"confidence"` // HIGH, MEDIUM, LOW
	Fixable    bool   `yaml:"fixable"`
}

// Notify adds extra context to notifications.
type Notify struct {
	ExtraContext string `yaml:"extra_context,omitempty"`
}

// YAMLSkill wraps a SkillDef into the skill.Skill interface.
type YAMLSkill struct {
	def          SkillDef
	logPatterns  []*regexp.Regexp
	diagTemplate *template.Template
}

// NewFromDef creates a YAMLSkill from a parsed definition.
func NewFromDef(def SkillDef) (*YAMLSkill, error) {
	patterns := make([]*regexp.Regexp, 0, len(def.Trigger.LogPatterns))
	for _, p := range def.Trigger.LogPatterns {
		re, err := regexp.Compile("(?i)" + p)
		if err != nil {
			return nil, fmt.Errorf("invalid log pattern %q: %w", p, err)
		}
		patterns = append(patterns, re)
	}

	tmpl, err := template.New(def.Name).Parse(def.Diagnosis.Template)
	if err != nil {
		return nil, fmt.Errorf("invalid diagnosis template: %w", err)
	}

	return &YAMLSkill{
		def:          def,
		logPatterns:  patterns,
		diagTemplate: tmpl,
	}, nil
}

func (s *YAMLSkill) Name() string        { return s.def.Name }
func (s *YAMLSkill) Version() string      { return s.def.Version }
func (s *YAMLSkill) Description() string  { return s.def.Description }

func (s *YAMLSkill) RequiredCapabilities() []skill.CapabilityID {
	caps := make([]skill.CapabilityID, len(s.def.Capabilities))
	for i, c := range s.def.Capabilities {
		caps[i] = skill.CapabilityID(c)
	}
	return caps
}

func (s *YAMLSkill) Match(_ context.Context, t *ticket.Ticket, ev skill.EvidenceSet) skill.MatchResult {
	// Check alert type match.
	if len(s.def.Trigger.AlertTypes) > 0 {
		matched := false
		for _, at := range s.def.Trigger.AlertTypes {
			if t.Type == at {
				matched = true
				break
			}
		}
		if !matched {
			return skill.MatchResult{}
		}
	}

	// Check log pattern match.
	if len(s.logPatterns) > 0 {
		logs := ev.Get("logs")
		if logs == "" {
			return skill.MatchResult{}
		}
		matchedAny := false
		for _, re := range s.logPatterns {
			if re.MatchString(logs) {
				matchedAny = true
				break
			}
		}
		if !matchedAny {
			return skill.MatchResult{}
		}
	}

	conf := 0.6 // YAML skills get medium-low base confidence
	switch strings.ToUpper(s.def.Diagnosis.Confidence) {
	case "HIGH":
		conf = 0.85
	case "MEDIUM":
		conf = 0.65
	case "LOW":
		conf = 0.45
	}

	return skill.MatchResult{
		Matched:    true,
		Confidence: conf,
		Priority:   50, // Below builtin skills
		Reason:     fmt.Sprintf("YAML skill %q matched", s.def.Name),
	}
}

func (s *YAMLSkill) Diagnose(_ context.Context, t *ticket.Ticket, ev skill.EvidenceSet) (*skill.DiagnosisResult, error) {
	data := map[string]string{
		"Tenant":  t.Tenant,
		"Service": t.Service,
		"Summary": t.Summary,
		"Type":    t.Type,
	}

	var buf bytes.Buffer
	if err := s.diagTemplate.Execute(&buf, data); err != nil {
		return nil, fmt.Errorf("rendering diagnosis template: %w", err)
	}

	conf := ticket.ConfidenceMedium
	switch strings.ToUpper(s.def.Diagnosis.Confidence) {
	case "HIGH":
		conf = ticket.ConfidenceHigh
	case "LOW":
		conf = ticket.ConfidenceLow
	}

	diag := buf.String()
	if s.def.Notification != nil && s.def.Notification.ExtraContext != "" {
		tmpl, err := template.New("notify").Parse(s.def.Notification.ExtraContext)
		if err == nil {
			var nbuf bytes.Buffer
			if tmpl.Execute(&nbuf, data) == nil {
				diag += "\n\n" + nbuf.String()
			}
		}
	}

	return &skill.DiagnosisResult{
		Diagnosis:  diag,
		Confidence: conf,
		Fixable:    s.def.Diagnosis.Fixable,
		FixType:    "",
	}, nil
}

func (s *YAMLSkill) Fix(_ context.Context, _ *ticket.Ticket, _ *skill.DiagnosisResult) (*skill.FixResult, error) {
	// YAML skills are currently diagnosis-only.
	return nil, fmt.Errorf("YAML skill %q does not support auto-fix", s.def.Name)
}

// Loader watches a directory for YAML skill definitions and registers/unregisters
// them in the skill registry.
type Loader struct {
	dir      string
	registry *skill.Registry
	mu       sync.Mutex
	loaded   map[string]struct{}
}

// NewLoader creates a loader that reads YAML skills from the given directory.
func NewLoader(dir string, registry *skill.Registry) *Loader {
	return &Loader{
		dir:      dir,
		registry: registry,
		loaded:   make(map[string]struct{}),
	}
}

// LoadAll scans the directory and registers all valid YAML skills.
// Returns the count of successfully loaded skills.
func (l *Loader) LoadAll() int {
	l.mu.Lock()
	defer l.mu.Unlock()

	entries, err := os.ReadDir(l.dir)
	if err != nil {
		if os.IsNotExist(err) {
			slog.Debug("yaml skills directory does not exist", "dir", l.dir)
			return 0
		}
		slog.Error("failed to read yaml skills directory", "dir", l.dir, "error", err)
		return 0
	}

	count := 0
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		ext := filepath.Ext(entry.Name())
		if ext != ".yaml" && ext != ".yml" {
			continue
		}

		path := filepath.Join(l.dir, entry.Name())
		if err := l.loadFile(path); err != nil {
			slog.Warn("failed to load yaml skill", "path", path, "error", err)
			continue
		}
		count++
	}

	slog.Info("yaml skills loaded", "count", count, "dir", l.dir)
	return count
}

func (l *Loader) loadFile(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}

	var def SkillDef
	if err := yaml.Unmarshal(data, &def); err != nil {
		return fmt.Errorf("parsing YAML: %w", err)
	}

	if def.Name == "" {
		return fmt.Errorf("skill name is required")
	}
	if def.Version == "" {
		def.Version = "1.0.0"
	}

	ys, err := NewFromDef(def)
	if err != nil {
		return err
	}

	l.registry.Register(ys)
	l.loaded[def.Name] = struct{}{}
	return nil
}

// Reload re-reads all YAML skills (hot-reload).
func (l *Loader) Reload() int {
	return l.LoadAll()
}

// LoadedCount returns how many YAML skills are currently loaded.
func (l *Loader) LoadedCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.loaded)
}
