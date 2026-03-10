// Package mcp implements a lightweight MCP (Model Context Protocol) server
// that exposes mctl-agent skill management as MCP tools.
//
// The server supports SSE transport over HTTP, compatible with Claude Desktop,
// Cursor, and other MCP-aware clients.
package mcp

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"

	"github.com/mctlhq/mctl-agent/internal/pipeline"
)

// jsonRPCRequest represents a JSON-RPC 2.0 request.
type jsonRPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// jsonRPCResponse represents a JSON-RPC 2.0 response.
type jsonRPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  interface{}     `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// ToolDef defines an MCP tool schema.
type ToolDef struct {
	Name        string      `json:"name"`
	Description string      `json:"description"`
	InputSchema InputSchema `json:"inputSchema"`
}

// InputSchema defines the JSON Schema for tool input.
type InputSchema struct {
	Type       string                 `json:"type"`
	Properties map[string]SchemaField `json:"properties,omitempty"`
	Required   []string               `json:"required,omitempty"`
}

// SchemaField defines a single property in the input schema.
type SchemaField struct {
	Type        string `json:"type"`
	Description string `json:"description"`
}

// ToolResult is the return value from a tool call.
type ToolResult struct {
	Content []ContentBlock `json:"content"`
	IsError bool           `json:"isError,omitempty"`
}

// ContentBlock represents a block of content in a tool result.
type ContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// Server is the MCP server that exposes agent skills as tools.
type Server struct {
	pipe  *pipeline.Pipeline
	tools map[string]ToolHandler
	defs  []ToolDef
	mu    sync.RWMutex
}

// ToolHandler processes a tool call and returns a result.
type ToolHandler func(params map[string]interface{}) (*ToolResult, error)

// NewServer creates a new MCP server backed by the given pipeline.
func NewServer(pipe *pipeline.Pipeline) *Server {
	s := &Server{
		pipe:  pipe,
		tools: make(map[string]ToolHandler),
	}
	s.registerTools()
	return s
}

// ServeHTTP handles MCP protocol messages over HTTP POST.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var req jsonRPCRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeRPCError(w, nil, -32700, "parse error: "+err.Error())
		return
	}

	switch req.Method {
	case "initialize":
		s.handleInitialize(w, req)
	case "tools/list":
		s.handleToolsList(w, req)
	case "tools/call":
		s.handleToolsCall(w, req)
	default:
		writeRPCError(w, req.ID, -32601, "method not found: "+req.Method)
	}
}

func (s *Server) handleInitialize(w http.ResponseWriter, req jsonRPCRequest) {
	writeRPCResult(w, req.ID, map[string]interface{}{
		"protocolVersion": "2024-11-05",
		"serverInfo": map[string]string{
			"name":    "mctl-agent",
			"version": "1.0.0",
		},
		"capabilities": map[string]interface{}{
			"tools": map[string]interface{}{},
		},
	})
}

func (s *Server) handleToolsList(w http.ResponseWriter, req jsonRPCRequest) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	writeRPCResult(w, req.ID, map[string]interface{}{
		"tools": s.defs,
	})
}

func (s *Server) handleToolsCall(w http.ResponseWriter, req jsonRPCRequest) {
	var params struct {
		Name      string                 `json:"name"`
		Arguments map[string]interface{} `json:"arguments"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		writeRPCError(w, req.ID, -32602, "invalid params: "+err.Error())
		return
	}

	s.mu.RLock()
	handler, ok := s.tools[params.Name]
	s.mu.RUnlock()

	if !ok {
		writeRPCError(w, req.ID, -32602, "unknown tool: "+params.Name)
		return
	}

	result, err := handler(params.Arguments)
	if err != nil {
		writeRPCResult(w, req.ID, &ToolResult{
			Content: []ContentBlock{{Type: "text", Text: "Error: " + err.Error()}},
			IsError: true,
		})
		return
	}

	writeRPCResult(w, req.ID, result)
}

func writeRPCResult(w http.ResponseWriter, id json.RawMessage, result interface{}) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(jsonRPCResponse{
		JSONRPC: "2.0",
		ID:      id,
		Result:  result,
	})
}

func writeRPCError(w http.ResponseWriter, id json.RawMessage, code int, message string) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(jsonRPCResponse{
		JSONRPC: "2.0",
		ID:      id,
		Error:   &rpcError{Code: code, Message: message},
	})
}

func textResult(format string, args ...interface{}) *ToolResult {
	return &ToolResult{
		Content: []ContentBlock{
			{Type: "text", Text: fmt.Sprintf(format, args...)},
		},
	}
}

func jsonResult(v interface{}) (*ToolResult, error) {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, err
	}
	return &ToolResult{
		Content: []ContentBlock{
			{Type: "text", Text: string(data)},
		},
	}, nil
}

// ToolDefs returns the registered tool definitions (for testing/introspection).
func (s *Server) ToolDefs() []ToolDef {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]ToolDef(nil), s.defs...)
}

func (s *Server) register(def ToolDef, handler ToolHandler) {
	s.tools[def.Name] = handler
	s.defs = append(s.defs, def)
	slog.Debug("mcp tool registered", "name", def.Name)
}
