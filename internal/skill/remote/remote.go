// Package remote implements skills that delegate to external HTTP services.
//
// Remote skills allow extending the agent without modifying its code.
// External services register via POST /api/v1/skills/register and implement
// the /match, /diagnose, /fix endpoints.
package remote

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/mctlhq/mctl-agent/internal/skill"
	"github.com/mctlhq/mctl-agent/internal/ticket"
)

// Registration is sent by external services to register a remote skill.
type Registration struct {
	Name         string   `json:"name"`
	Version      string   `json:"version"`
	Description  string   `json:"description"`
	Endpoint     string   `json:"endpoint"`      // Base URL, e.g. https://my-skill.example.com
	AlertTypes   []string `json:"alert_types"`    // Skill is only considered for these alert types
	Capabilities []string `json:"capabilities"`
}

// matchRequest is sent to the remote skill's /match endpoint.
type matchRequest struct {
	Ticket   ticketPayload     `json:"ticket"`
	Evidence map[string]string `json:"evidence"`
}

// matchResponse is returned by the remote skill's /match endpoint.
type matchResponse struct {
	Matched    bool    `json:"matched"`
	Confidence float64 `json:"confidence"`
	Priority   int     `json:"priority"`
	Reason     string  `json:"reason"`
}

// diagnoseRequest is sent to the remote skill's /diagnose endpoint.
type diagnoseRequest struct {
	Ticket   ticketPayload     `json:"ticket"`
	Evidence map[string]string `json:"evidence"`
}

// diagnoseResponse is returned by the remote skill's /diagnose endpoint.
type diagnoseResponse struct {
	Diagnosis      string `json:"diagnosis"`
	Confidence     string `json:"confidence"`
	Fixable        bool   `json:"fixable"`
	FixType        string `json:"fix_type,omitempty"`
	YAMLPath       string `json:"yaml_path,omitempty"`
	YAMLField      string `json:"yaml_field,omitempty"`
	CurrentValue   string `json:"current_value,omitempty"`
	SuggestedValue string `json:"suggested_value,omitempty"`
	Reasoning      string `json:"reasoning,omitempty"`
}

// fixRequest is sent to the remote skill's /fix endpoint.
type fixRequest struct {
	Ticket    ticketPayload `json:"ticket"`
	Diagnosis diagnoseResponse `json:"diagnosis"`
}

// fixResponse is returned by the remote skill's /fix endpoint.
type fixResponse struct {
	Applied    bool     `json:"applied"`
	NewContent string   `json:"new_content,omitempty"`
	FilePath   string   `json:"file_path,omitempty"`
	Summary    string   `json:"summary"`
	NextSkills []string `json:"next_skills,omitempty"`
}

type ticketPayload struct {
	ID       string `json:"id"`
	Source   string `json:"source"`
	Type     string `json:"type"`
	Tenant   string `json:"tenant"`
	Service  string `json:"service"`
	Summary  string `json:"summary"`
	Severity string `json:"severity"`
}

func toPayload(t *ticket.Ticket) ticketPayload {
	return ticketPayload{
		ID:       t.ID,
		Source:   t.Source,
		Type:     t.Type,
		Tenant:   t.Tenant,
		Service:  t.Service,
		Summary:  t.Summary,
		Severity: t.Severity,
	}
}

// Skill implements the skill.Skill interface by delegating to a remote HTTP service.
type Skill struct {
	reg    Registration
	client *http.Client
}

// New creates a remote skill from a registration.
func New(reg Registration) *Skill {
	return &Skill{
		reg: reg,
		client: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

func (s *Skill) Name() string        { return s.reg.Name }
func (s *Skill) Version() string      { return s.reg.Version }
func (s *Skill) Description() string  { return s.reg.Description }

func (s *Skill) RequiredCapabilities() []skill.CapabilityID {
	caps := make([]skill.CapabilityID, len(s.reg.Capabilities))
	for i, c := range s.reg.Capabilities {
		caps[i] = skill.CapabilityID(c)
	}
	return caps
}

func (s *Skill) Match(ctx context.Context, t *ticket.Ticket, ev skill.EvidenceSet) skill.MatchResult {
	// Pre-filter by alert type if specified.
	if len(s.reg.AlertTypes) > 0 {
		matched := false
		for _, at := range s.reg.AlertTypes {
			if t.Type == at {
				matched = true
				break
			}
		}
		if !matched {
			return skill.MatchResult{}
		}
	}

	reqBody := matchRequest{
		Ticket:   toPayload(t),
		Evidence: ev.All(),
	}

	var resp matchResponse
	if err := s.post(ctx, "/match", reqBody, &resp); err != nil {
		slog.Warn("remote skill match failed", "skill", s.reg.Name, "error", err)
		return skill.MatchResult{}
	}

	return skill.MatchResult{
		Matched:    resp.Matched,
		Confidence: resp.Confidence,
		Priority:   resp.Priority,
		Reason:     resp.Reason,
	}
}

func (s *Skill) Diagnose(ctx context.Context, t *ticket.Ticket, ev skill.EvidenceSet) (*skill.DiagnosisResult, error) {
	reqBody := diagnoseRequest{
		Ticket:   toPayload(t),
		Evidence: ev.All(),
	}

	var resp diagnoseResponse
	if err := s.post(ctx, "/diagnose", reqBody, &resp); err != nil {
		return nil, fmt.Errorf("remote diagnose: %w", err)
	}

	return &skill.DiagnosisResult{
		Diagnosis:      resp.Diagnosis,
		Confidence:     resp.Confidence,
		Fixable:        resp.Fixable,
		FixType:        resp.FixType,
		YAMLPath:       resp.YAMLPath,
		YAMLField:      resp.YAMLField,
		CurrentValue:   resp.CurrentValue,
		SuggestedValue: resp.SuggestedValue,
		Reasoning:      resp.Reasoning,
	}, nil
}

func (s *Skill) Fix(ctx context.Context, t *ticket.Ticket, diag *skill.DiagnosisResult) (*skill.FixResult, error) {
	reqBody := fixRequest{
		Ticket: toPayload(t),
		Diagnosis: diagnoseResponse{
			Diagnosis:      diag.Diagnosis,
			Confidence:     diag.Confidence,
			Fixable:        diag.Fixable,
			FixType:        diag.FixType,
			YAMLPath:       diag.YAMLPath,
			YAMLField:      diag.YAMLField,
			CurrentValue:   diag.CurrentValue,
			SuggestedValue: diag.SuggestedValue,
			Reasoning:      diag.Reasoning,
		},
	}

	var resp fixResponse
	if err := s.post(ctx, "/fix", reqBody, &resp); err != nil {
		return nil, fmt.Errorf("remote fix: %w", err)
	}

	return &skill.FixResult{
		NewContent: resp.NewContent,
		FilePath:   resp.FilePath,
		Summary:    resp.Summary,
		NextSkills: resp.NextSkills,
	}, nil
}

func (s *Skill) post(ctx context.Context, path string, reqBody, respBody interface{}) error {
	data, err := json.Marshal(reqBody)
	if err != nil {
		return err
	}

	url := s.reg.Endpoint + path
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close() //nolint:errcheck

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
	}

	return json.Unmarshal(body, respBody)
}

// Manager keeps track of registered remote skills.
type Manager struct {
	mu       sync.RWMutex
	skills   map[string]*Skill
	registry *skill.Registry
}

// NewManager creates a remote skill manager.
func NewManager(registry *skill.Registry) *Manager {
	return &Manager{
		skills:   make(map[string]*Skill),
		registry: registry,
	}
}

// Register adds a remote skill from a registration payload.
func (m *Manager) Register(reg Registration) error {
	if reg.Name == "" {
		return fmt.Errorf("skill name is required")
	}
	if reg.Endpoint == "" {
		return fmt.Errorf("skill endpoint is required")
	}
	if reg.Version == "" {
		reg.Version = "1.0.0"
	}

	s := New(reg)

	m.mu.Lock()
	m.skills[reg.Name] = s
	m.mu.Unlock()

	m.registry.Register(s)
	slog.Info("remote skill registered", "name", reg.Name, "endpoint", reg.Endpoint)
	return nil
}

// Unregister removes a remote skill.
func (m *Manager) Unregister(name string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, ok := m.skills[name]; !ok {
		return false
	}
	delete(m.skills, name)
	m.registry.Disable(name)
	return true
}

// List returns all registered remote skills.
func (m *Manager) List() []Registration {
	m.mu.RLock()
	defer m.mu.RUnlock()

	regs := make([]Registration, 0, len(m.skills))
	for _, s := range m.skills {
		regs = append(regs, s.reg)
	}
	return regs
}
