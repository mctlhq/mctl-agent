package mcp

import (
	"context"
	"fmt"

	"github.com/mctlhq/mctl-agent/internal/skill"
)

// registerTools registers all MCP tools for skill management.
func (s *Server) registerTools() {
	s.registerListSkills()
	s.registerSkillStatus()
	s.registerDisableSkill()
	s.registerEnableSkill()
	s.registerTriggerSkill()
	s.registerSkillMetricsAll()
}

func (s *Server) registerListSkills() {
	s.register(ToolDef{
		Name:        "mctl_agent_list_skills",
		Description: "List all registered skills in the mctl-agent with their status (enabled/disabled), version, and description.",
		InputSchema: InputSchema{Type: "object"},
	}, func(params map[string]interface{}) (*ToolResult, error) {
		skills := s.pipe.Registry().List()
		type skillEntry struct {
			Name         string              `json:"name"`
			Version      string              `json:"version"`
			Description  string              `json:"description"`
			Enabled      bool                `json:"enabled"`
			Capabilities []skill.CapabilityID `json:"capabilities"`
		}
		entries := make([]skillEntry, len(skills))
		for i, sk := range skills {
			entries[i] = skillEntry{
				Name:         sk.Name,
				Version:      sk.Version,
				Description:  sk.Description,
				Enabled:      sk.Enabled,
				Capabilities: sk.Capabilities,
			}
		}
		return jsonResult(map[string]interface{}{
			"skills": entries,
			"count":  len(entries),
		})
	})
}

func (s *Server) registerSkillStatus() {
	s.register(ToolDef{
		Name:        "mctl_agent_skill_status",
		Description: "Get detailed metrics for a specific skill: match count, diagnoses, fixes, success rate, average fix time, and circuit breaker status.",
		InputSchema: InputSchema{
			Type: "object",
			Properties: map[string]SchemaField{
				"skill_name": {Type: "string", Description: "Name of the skill to query"},
			},
			Required: []string{"skill_name"},
		},
	}, func(params map[string]interface{}) (*ToolResult, error) {
		name, _ := params["skill_name"].(string)
		if name == "" {
			return nil, fmt.Errorf("skill_name is required")
		}

		m := s.pipe.Metrics()
		if m == nil {
			return nil, fmt.Errorf("metrics not enabled")
		}

		snap := m.GetSnapshot(name)
		return jsonResult(snap)
	})
}

func (s *Server) registerDisableSkill() {
	s.register(ToolDef{
		Name:        "mctl_agent_disable_skill",
		Description: "Disable a skill so it will not be matched against future tickets. The skill remains registered and can be re-enabled.",
		InputSchema: InputSchema{
			Type: "object",
			Properties: map[string]SchemaField{
				"skill_name": {Type: "string", Description: "Name of the skill to disable"},
			},
			Required: []string{"skill_name"},
		},
	}, func(params map[string]interface{}) (*ToolResult, error) {
		name, _ := params["skill_name"].(string)
		if name == "" {
			return nil, fmt.Errorf("skill_name is required")
		}

		if ok := s.pipe.Registry().Disable(name); !ok {
			return nil, fmt.Errorf("skill %q not found", name)
		}

		return textResult("Skill %q disabled. It will not match future tickets.", name), nil
	})
}

func (s *Server) registerEnableSkill() {
	s.register(ToolDef{
		Name:        "mctl_agent_enable_skill",
		Description: "Re-enable a previously disabled skill.",
		InputSchema: InputSchema{
			Type: "object",
			Properties: map[string]SchemaField{
				"skill_name": {Type: "string", Description: "Name of the skill to enable"},
			},
			Required: []string{"skill_name"},
		},
	}, func(params map[string]interface{}) (*ToolResult, error) {
		name, _ := params["skill_name"].(string)
		if name == "" {
			return nil, fmt.Errorf("skill_name is required")
		}

		if ok := s.pipe.Registry().Enable(name); !ok {
			return textResult("Skill %q is not disabled or not found.", name), nil
		}

		return textResult("Skill %q enabled.", name), nil
	})
}

func (s *Server) registerTriggerSkill() {
	s.register(ToolDef{
		Name:        "mctl_agent_trigger_skill",
		Description: "Manually trigger skill analysis for a specific team/service combination. Creates a synthetic ticket and runs the pipeline.",
		InputSchema: InputSchema{
			Type: "object",
			Properties: map[string]SchemaField{
				"team":    {Type: "string", Description: "Team (tenant) name"},
				"service": {Type: "string", Description: "Service name"},
				"reason":  {Type: "string", Description: "Reason for manual trigger (optional)"},
			},
			Required: []string{"team", "service"},
		},
	}, func(params map[string]interface{}) (*ToolResult, error) {
		team, _ := params["team"].(string)
		service, _ := params["service"].(string)
		reason, _ := params["reason"].(string)
		if team == "" || service == "" {
			return nil, fmt.Errorf("team and service are required")
		}

		if reason == "" {
			reason = "Manual trigger via MCP tool"
		}

		t, err := s.pipe.TriggerAnalysis(context.Background(), team, service, reason)
		if err != nil {
			return nil, fmt.Errorf("trigger failed: %w", err)
		}

		return textResult("Analysis triggered. Ticket ID: %s\nTeam: %s, Service: %s\nReason: %s",
			t.ID, team, service, reason), nil
	})
}

func (s *Server) registerSkillMetricsAll() {
	s.register(ToolDef{
		Name:        "mctl_agent_all_skill_metrics",
		Description: "Get aggregated metrics for all skills: matches, diagnoses, fixes, success rates.",
		InputSchema: InputSchema{Type: "object"},
	}, func(params map[string]interface{}) (*ToolResult, error) {
		m := s.pipe.Metrics()
		if m == nil {
			return nil, fmt.Errorf("metrics not enabled")
		}

		snaps := m.GetAllSnapshots()
		return jsonResult(map[string]interface{}{
			"skills": snaps,
			"count":  len(snaps),
		})
	})
}
