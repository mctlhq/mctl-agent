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
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// mockPipeline creates a Server with a nil pipeline for testing tool defs.
func TestToolDefinitions(t *testing.T) {
	// Server with nil pipeline — only tests tool registration, not execution.
	s := &Server{
		tools: make(map[string]ToolHandler),
	}
	s.registerTools()

	defs := s.ToolDefs()
	expectedTools := []string{
		"mctl_agent_list_skills",
		"mctl_agent_skill_status",
		"mctl_agent_disable_skill",
		"mctl_agent_enable_skill",
		"mctl_agent_trigger_skill",
		"mctl_agent_all_skill_metrics",
		"mctl_agent_list_webhooks",
		"mctl_agent_register_webhook",
		"mctl_agent_delete_webhook",
	}

	if len(defs) != len(expectedTools) {
		t.Errorf("expected %d tools, got %d", len(expectedTools), len(defs))
	}

	names := make(map[string]bool)
	for _, d := range defs {
		names[d.Name] = true
	}
	for _, expected := range expectedTools {
		if !names[expected] {
			t.Errorf("missing tool: %s", expected)
		}
	}
}

func TestInitializeMethod(t *testing.T) {
	s := &Server{tools: make(map[string]ToolHandler)}
	s.registerTools()

	body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()

	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	var resp jsonRPCResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if resp.Error != nil {
		t.Fatalf("unexpected error: %v", resp.Error)
	}

	result, ok := resp.Result.(map[string]interface{})
	if !ok {
		t.Fatal("result is not a map")
	}
	if result["protocolVersion"] != "2024-11-05" {
		t.Errorf("unexpected protocol version: %v", result["protocolVersion"])
	}
}

func TestToolsListMethod(t *testing.T) {
	s := &Server{tools: make(map[string]ToolHandler)}
	s.registerTools()

	body := `{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()

	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	var resp jsonRPCResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if resp.Error != nil {
		t.Fatalf("unexpected error: %v", resp.Error)
	}

	result, ok := resp.Result.(map[string]interface{})
	if !ok {
		t.Fatal("result is not a map")
	}
	tools, ok := result["tools"].([]interface{})
	if !ok {
		t.Fatal("tools is not an array")
	}
	if len(tools) != 9 {
		t.Errorf("expected 9 tools, got %d", len(tools))
	}
}

func TestUnknownMethod(t *testing.T) {
	s := &Server{tools: make(map[string]ToolHandler)}

	body := `{"jsonrpc":"2.0","id":3,"method":"unknown/method","params":{}}`
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()

	s.ServeHTTP(rec, req)

	var resp jsonRPCResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if resp.Error == nil {
		t.Fatal("expected error for unknown method")
	}
	if resp.Error.Code != -32601 {
		t.Errorf("expected error code -32601, got %d", resp.Error.Code)
	}
}

func TestUnknownTool(t *testing.T) {
	s := &Server{tools: make(map[string]ToolHandler)}

	body := `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"nonexistent","arguments":{}}}`
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()

	s.ServeHTTP(rec, req)

	var resp jsonRPCResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if resp.Error == nil {
		t.Fatal("expected error for unknown tool")
	}
}
