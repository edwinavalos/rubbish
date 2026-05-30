package mcp

import (
	"context"
	"encoding/json"
	"net/http"
)

// Server handles MCP streamable-HTTP transport.
type Server struct {
	tools map[string]Tool
	wh    WorkflowHandler // may be nil (stub mode)
	sh    SessionHandler  // may be nil (stub mode)
}

// Tool represents a registered MCP tool.
type Tool struct {
	Name        string
	Description string
	InputSchema map[string]any // JSON Schema object
	Handler     func(ctx context.Context, args map[string]any) (string, error)
}

// NewServer creates a new MCP server. wh and sh may be nil for stub mode.
func NewServer(wh WorkflowHandler, sh SessionHandler) *Server {
	s := &Server{
		tools: make(map[string]Tool),
		wh:    wh,
		sh:    sh,
	}
	registerTools(s)
	return s
}

// register adds a tool to the server.
func (s *Server) register(t Tool) {
	s.tools[t.Name] = t
}

// ---- JSON-RPC 2.0 types -----------------------------------------------------

type rpcRequest struct {
	JSONRPC string         `json:"jsonrpc"`
	ID      any            `json:"id"`
	Method  string         `json:"method"`
	Params  map[string]any `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string `json:"jsonrpc"`
	ID      any    `json:"id"`
	Result  any    `json:"result,omitempty"`
	Error   *rpcError `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func errorResponse(id any, code int, msg string) rpcResponse {
	return rpcResponse{
		JSONRPC: "2.0",
		ID:      id,
		Error:   &rpcError{Code: code, Message: msg},
	}
}

func okResponse(id any, result any) rpcResponse {
	return rpcResponse{
		JSONRPC: "2.0",
		ID:      id,
		Result:  result,
	}
}

// ---- http.Handler -----------------------------------------------------------

// ServeHTTP makes Server an http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req rpcRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		resp := errorResponse(nil, -32700, "parse error: "+err.Error())
		writeJSON(w, resp)
		return
	}

	var resp rpcResponse
	switch req.Method {
	case "initialize":
		resp = s.handleInitialize(req)
	case "notifications/initialized":
		resp = okResponse(req.ID, map[string]any{})
	case "tools/list":
		resp = s.handleToolsList(req)
	case "tools/call":
		resp = s.handleToolsCall(r.Context(), req)
	default:
		resp = errorResponse(req.ID, -32601, "method not found: "+req.Method)
	}

	writeJSON(w, resp)
}

func (s *Server) handleInitialize(req rpcRequest) rpcResponse {
	result := map[string]any{
		"protocolVersion": "2024-11-05",
		"capabilities": map[string]any{
			"tools": map[string]any{},
		},
		"serverInfo": map[string]any{
			"name":    "rubbish",
			"version": "0.1.0",
		},
	}
	return okResponse(req.ID, result)
}

func (s *Server) handleToolsList(req rpcRequest) rpcResponse {
	tools := make([]map[string]any, 0, len(s.tools))
	for _, t := range s.tools {
		tools = append(tools, map[string]any{
			"name":        t.Name,
			"description": t.Description,
			"inputSchema": t.InputSchema,
		})
	}
	return okResponse(req.ID, map[string]any{"tools": tools})
}

func (s *Server) handleToolsCall(ctx context.Context, req rpcRequest) rpcResponse {
	name, _ := req.Params["name"].(string)
	if name == "" {
		return errorResponse(req.ID, -32602, "params.name is required")
	}

	t, ok := s.tools[name]
	if !ok {
		return errorResponse(req.ID, -32601, "tool not found: "+name)
	}

	var args map[string]any
	if a, ok := req.Params["arguments"]; ok {
		args, _ = a.(map[string]any)
	}
	if args == nil {
		args = map[string]any{}
	}

	text, err := t.Handler(ctx, args)
	if err != nil {
		return errorResponse(req.ID, -32603, err.Error())
	}

	result := map[string]any{
		"content": []map[string]any{
			{"type": "text", "text": text},
		},
	}
	return okResponse(req.ID, result)
}

// writeJSON serialises v as JSON to w with Content-Type set.
func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v) //nolint:errcheck
}
