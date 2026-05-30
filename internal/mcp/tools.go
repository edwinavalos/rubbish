package mcp

import (
	"context"
	"encoding/json"
	"fmt"
)

// WorkflowHandler is implemented by workflow.Engine (wired in later).
// any is used in place of *workflow.Workflow to avoid import cycles.
type WorkflowHandler interface {
	Submit(ctx context.Context, input, repoURL, branch string) (string, error) // returns workflow ID
	Get(id string) (any, bool)                                                 // returns *workflow.Workflow
	List() []any                                                               // returns []*workflow.Workflow
}

// SessionHandler is implemented by SessionManager (wired in later).
// any is used in place of sessionstore.Row to avoid import cycles.
type SessionHandler interface {
	CreateSession(ctx context.Context, role, prompt, repoURL, branch string) (string, error) // returns session ID
	GetSession(id string) (any, bool)                                                        // returns sessionstore.Row
	ListSessions() ([]any, error)
	StopSession(id string) error
}

// registerTools wires all MCP tools into s. Called from NewServer.
func registerTools(s *Server) {
	// ---- workflow tools -------------------------------------------------------

	s.register(Tool{
		Name:        "submit_workflow",
		Description: "Start a new workflow run against a repository.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"input":    map[string]any{"type": "string", "description": "The goal or brief for this workflow"},
				"repo_url": map[string]any{"type": "string", "description": "Git repository URL"},
				"branch":   map[string]any{"type": "string", "description": "Branch name (optional)"},
			},
			"required": []string{"input"},
		},
		Handler: func(ctx context.Context, args map[string]any) (string, error) {
			if s.wh == nil {
				return "not implemented", nil
			}
			input, _ := args["input"].(string)
			repoURL, _ := args["repo_url"].(string)
			branch, _ := args["branch"].(string)
			id, err := s.wh.Submit(ctx, input, repoURL, branch)
			if err != nil {
				return "", fmt.Errorf("submit_workflow: %w", err)
			}
			return marshalAny(map[string]string{"workflow_id": id})
		},
	})

	s.register(Tool{
		Name:        "get_workflow",
		Description: "Get the status and stages of a workflow run.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"workflow_id": map[string]any{"type": "string", "description": "Workflow ID returned by submit_workflow"},
			},
			"required": []string{"workflow_id"},
		},
		Handler: func(ctx context.Context, args map[string]any) (string, error) {
			if s.wh == nil {
				return "not implemented", nil
			}
			id, _ := args["workflow_id"].(string)
			wf, ok := s.wh.Get(id)
			if !ok {
				return "not found", nil
			}
			return marshalAny(wf)
		},
	})

	s.register(Tool{
		Name:        "list_workflows",
		Description: "List all workflow runs.",
		InputSchema: map[string]any{
			"type":       "object",
			"properties": map[string]any{},
		},
		Handler: func(ctx context.Context, args map[string]any) (string, error) {
			if s.wh == nil {
				return "not implemented", nil
			}
			return marshalAny(s.wh.List())
		},
	})

	// ---- session tools --------------------------------------------------------

	s.register(Tool{
		Name:        "create_session",
		Description: "Create a new task VM session for a Claude agent.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"role":     map[string]any{"type": "string", "description": "Session role: interactive | research | plan | implement"},
				"prompt":   map[string]any{"type": "string", "description": "The claude -p prompt for non-interactive sessions"},
				"repo_url": map[string]any{"type": "string", "description": "Git repository URL"},
				"branch":   map[string]any{"type": "string", "description": "Branch name (optional)"},
			},
			"required": []string{"role"},
		},
		Handler: func(ctx context.Context, args map[string]any) (string, error) {
			if s.sh == nil {
				return "not implemented", nil
			}
			role, _ := args["role"].(string)
			prompt, _ := args["prompt"].(string)
			repoURL, _ := args["repo_url"].(string)
			branch, _ := args["branch"].(string)
			id, err := s.sh.CreateSession(ctx, role, prompt, repoURL, branch)
			if err != nil {
				return "", fmt.Errorf("create_session: %w", err)
			}
			return marshalAny(map[string]string{"session_id": id})
		},
	})

	s.register(Tool{
		Name:        "get_session",
		Description: "Get the status and result of a session.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"session_id": map[string]any{"type": "string", "description": "Session ID returned by create_session"},
			},
			"required": []string{"session_id"},
		},
		Handler: func(ctx context.Context, args map[string]any) (string, error) {
			if s.sh == nil {
				return "not implemented", nil
			}
			id, _ := args["session_id"].(string)
			sess, ok := s.sh.GetSession(id)
			if !ok {
				return "not found", nil
			}
			return marshalAny(sess)
		},
	})

	s.register(Tool{
		Name:        "list_sessions",
		Description: "List all sessions.",
		InputSchema: map[string]any{
			"type":       "object",
			"properties": map[string]any{},
		},
		Handler: func(ctx context.Context, args map[string]any) (string, error) {
			if s.sh == nil {
				return "not implemented", nil
			}
			sessions, err := s.sh.ListSessions()
			if err != nil {
				return "", fmt.Errorf("list_sessions: %w", err)
			}
			return marshalAny(sessions)
		},
	})

	s.register(Tool{
		Name:        "stop_session",
		Description: "Stop a running session.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"session_id": map[string]any{"type": "string", "description": "Session ID to stop"},
			},
			"required": []string{"session_id"},
		},
		Handler: func(ctx context.Context, args map[string]any) (string, error) {
			if s.sh == nil {
				return "not implemented", nil
			}
			id, _ := args["session_id"].(string)
			if err := s.sh.StopSession(id); err != nil {
				return "", fmt.Errorf("stop_session: %w", err)
			}
			return marshalAny(map[string]string{"status": "stopping", "session_id": id})
		},
	})
}

// marshalAny marshals v to a JSON string. On error it returns the error message
// as the result string (not an error) so Claude can see what went wrong.
func marshalAny(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("marshal error: %v", err), nil
	}
	return string(b), nil
}
