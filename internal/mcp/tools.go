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

package mcp

import (
	"context"
	"fmt"

	"github.com/mctlhq/mctl-agent/internal/skill"
	"github.com/mctlhq/mctl-agent/internal/webhook"
)

// registerTools registers all MCP tools for skill management.
func (s *Server) registerTools() {
	s.registerListSkills()
	s.registerSkillStatus()
	s.registerDisableSkill()
	s.registerEnableSkill()
	s.registerTriggerSkill()
	s.registerSkillMetricsAll()
	s.registerListWebhooks()
	s.registerRegisterWebhook()
	s.registerDeleteWebhook()
}

func (s *Server) registerListSkills() {
	s.register(ToolDef{
		Name:        "mctl_agent_list_skills",
		Description: "List all registered skills in the mctl-agent with their status (enabled/disabled), version, and description.",
		InputSchema: InputSchema{Type: "object"},
	}, func(params map[string]interface{}) (*ToolResult, error) {
		skills := s.pipe.Registry().List()
		type skillEntry struct {
			Name         string               `json:"name"`
			Version      string               `json:"version"`
			Description  string               `json:"description"`
			Enabled      bool                 `json:"enabled"`
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

func (s *Server) registerListWebhooks() {
	s.register(ToolDef{
		Name:        "mctl_agent_list_webhooks",
		Description: "List registered external webhook endpoints for mctl-agent.",
		InputSchema: InputSchema{Type: "object"},
	}, func(params map[string]interface{}) (*ToolResult, error) {
		if s.webhooks == nil {
			return nil, fmt.Errorf("webhooks not enabled")
		}
		items, err := s.webhooks.ListEndpoints()
		if err != nil {
			return nil, err
		}
		for i := range items {
			if items[i].Secret != "" {
				items[i].Secret = "redacted"
			}
			if items[i].AuthHeaderValue != "" {
				items[i].AuthHeaderValue = "redacted"
			}
		}
		return jsonResult(map[string]interface{}{"items": items, "count": len(items)})
	})
}

func (s *Server) registerRegisterWebhook() {
	s.register(ToolDef{
		Name:        "mctl_agent_register_webhook",
		Description: "Register an external webhook endpoint for selected event types.",
		InputSchema: InputSchema{
			Type: "object",
			Properties: map[string]SchemaField{
				"agent_id":          {Type: "string", Description: "External agent identifier"},
				"url":               {Type: "string", Description: "Webhook URL"},
				"secret":            {Type: "string", Description: "Shared secret for HMAC signing"},
				"auth_header_name":  {Type: "string", Description: "Optional delivery auth header name"},
				"auth_header_value": {Type: "string", Description: "Optional delivery auth header value"},
				"event_types":       {Type: "array", Description: "Subscribed event types"},
			},
			Required: []string{"agent_id", "url", "secret", "event_types"},
		},
	}, func(params map[string]interface{}) (*ToolResult, error) {
		if s.webhooks == nil {
			return nil, fmt.Errorf("webhooks not enabled")
		}
		agentID, _ := params["agent_id"].(string)
		url, _ := params["url"].(string)
		secret, _ := params["secret"].(string)
		authHeaderName, _ := params["auth_header_name"].(string)
		authHeaderValue, _ := params["auth_header_value"].(string)
		rawEvents, _ := params["event_types"].([]interface{})
		eventTypes := make([]string, 0, len(rawEvents))
		for _, item := range rawEvents {
			eventTypes = append(eventTypes, fmt.Sprintf("%v", item))
		}
		ep := &webhook.WebhookEndpoint{
			AgentID:         agentID,
			URL:             url,
			Secret:          secret,
			AuthHeaderName:  authHeaderName,
			AuthHeaderValue: authHeaderValue,
			EventTypes:      eventTypes,
			Active:          true,
		}
		if err := s.webhooks.CreateEndpoint(ep); err != nil {
			return nil, err
		}
		return jsonResult(map[string]interface{}{
			"id": ep.ID, "agent_id": ep.AgentID, "url": ep.URL, "event_types": ep.EventTypes, "active": ep.Active,
			"auth_header_name": ep.AuthHeaderName,
		})
	})
}

func (s *Server) registerDeleteWebhook() {
	s.register(ToolDef{
		Name:        "mctl_agent_delete_webhook",
		Description: "Delete a registered external webhook endpoint by id.",
		InputSchema: InputSchema{
			Type: "object",
			Properties: map[string]SchemaField{
				"id": {Type: "string", Description: "Webhook id"},
			},
			Required: []string{"id"},
		},
	}, func(params map[string]interface{}) (*ToolResult, error) {
		if s.webhooks == nil {
			return nil, fmt.Errorf("webhooks not enabled")
		}
		id, _ := params["id"].(string)
		if id == "" {
			return nil, fmt.Errorf("id is required")
		}
		if err := s.webhooks.DeleteEndpoint(id); err != nil {
			return nil, err
		}
		return textResult("Webhook %s deleted.", id), nil
	})
}
