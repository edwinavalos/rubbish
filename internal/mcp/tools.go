package mcp

import (
	"context"
	"encoding/json"
	"fmt"
)

// ---- slim projection helpers -------------------------------------------------
// The MCP handler receives workflow/session values as `any` to avoid import
// cycles. We marshal→map→project→re-marshal to strip large text bodies before
// returning responses to the LLM.

// projectWorkflow projects a workflow value into a map with stage Input/Output removed.
// If includeOutput is true, stage Output is kept. Returns the map for further composition.
func projectWorkflow(wf any, includeOutput bool) (map[string]any, error) {
	b, err := json.Marshal(wf)
	if err != nil {
		return nil, fmt.Errorf("marshal workflow: %w", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("unmarshal workflow: %w", err)
	}
	if stages, ok := m["Stages"].([]any); ok {
		for _, s := range stages {
			if sm, ok := s.(map[string]any); ok {
				delete(sm, "Input")
				if !includeOutput {
					delete(sm, "Output")
				}
			}
		}
	}
	return m, nil
}

// slimWorkflow marshals the projected workflow to a JSON string.
func slimWorkflow(wf any, includeOutput bool) (string, error) {
	m, err := projectWorkflow(wf, includeOutput)
	if err != nil {
		return fmt.Sprintf("error: %v", err), nil
	}
	out, err := json.Marshal(m)
	if err != nil {
		return fmt.Sprintf("marshal error: %v", err), nil
	}
	return string(out), nil
}

// slimSession returns a map with Prompt and Result removed.
// If includeResult is true, Result is kept but truncated to maxResultBytes.
// A ResultBytesFull field is added when truncation occurs.
func slimSession(sess any, includeResult bool, maxResultBytes int) (string, error) {
	b, err := json.Marshal(sess)
	if err != nil {
		return fmt.Sprintf("marshal error: %v", err), nil
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return string(b), nil
	}
	delete(m, "Prompt")
	if includeResult {
		if result, ok := m["Result"].(string); ok && len(result) > maxResultBytes {
			m["Result"] = result[:maxResultBytes]
			m["ResultBytesFull"] = len(result)
			m["ResultTruncated"] = true
		}
	} else {
		if result, ok := m["Result"].(string); ok && len(result) > 0 {
			m["ResultBytesFull"] = len(result)
		}
		delete(m, "Result")
	}
	out, err := json.Marshal(m)
	if err != nil {
		return fmt.Sprintf("marshal error: %v", err), nil
	}
	return string(out), nil
}

// isActiveSession returns true for sessions that are not in a terminal state.
func isActiveSession(m map[string]any) bool {
	status, _ := m["Status"].(string)
	return status != "stopped" && status != "failed"
}

// WorkflowHandler is implemented by workflow.Engine (wired in later).
// any is used in place of *workflow.Workflow to avoid import cycles.
type WorkflowHandler interface {
	Submit(ctx context.Context, input, repoURL, branch string) (string, error) // returns workflow ID
	Get(id string) (any, bool)                                                 // returns *workflow.Workflow
	List() []any                                                               // returns []*workflow.Workflow
	Resume(ctx context.Context, id string) error                               // re-runs failed stages
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
		Name: "get_workflow",
		Description: "Get the status and stages of a workflow run. " +
			"By default stage prompts and agent outputs are omitted to keep the response small — " +
			"set include_output=true to include full stage Output text (can be very large).",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"workflow_id":    map[string]any{"type": "string", "description": "Workflow ID returned by submit_workflow"},
				"include_output": map[string]any{"type": "boolean", "description": "Include full stage Output text (default false)"},
			},
			"required": []string{"workflow_id"},
		},
		Handler: func(ctx context.Context, args map[string]any) (string, error) {
			if s.wh == nil {
				return "not implemented", nil
			}
			id, _ := args["workflow_id"].(string)
			includeOutput, _ := args["include_output"].(bool)
			wf, ok := s.wh.Get(id)
			if !ok {
				return "not found", nil
			}
			return slimWorkflow(wf, includeOutput)
		},
	})

	s.register(Tool{
		Name:        "list_workflows",
		Description: "List all workflow runs. Stage prompts and outputs are omitted; use get_workflow for full stage detail.",
		InputSchema: map[string]any{
			"type":       "object",
			"properties": map[string]any{},
		},
		Handler: func(ctx context.Context, args map[string]any) (string, error) {
			if s.wh == nil {
				return "not implemented", nil
			}
			wfs := s.wh.List()
			slim := make([]map[string]any, 0, len(wfs))
			for _, wf := range wfs {
				m, err := projectWorkflow(wf, false)
				if err != nil {
					continue
				}
				slim = append(slim, m)
			}
			return marshalAny(slim)
		},
	})

	s.register(Tool{
		Name: "resume_workflow",
		Description: "Re-run all failed stages of a workflow, reusing the existing research and plan outputs. " +
			"Use this after Claude API usage limits reset to continue a partially-completed workflow. " +
			"Returns an error if the workflow has no resumable (failed + saved input) stages.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"workflow_id": map[string]any{"type": "string", "description": "Workflow ID to resume"},
			},
			"required": []string{"workflow_id"},
		},
		Handler: func(ctx context.Context, args map[string]any) (string, error) {
			if s.wh == nil {
				return "not implemented", nil
			}
			id, _ := args["workflow_id"].(string)
			if err := s.wh.Resume(ctx, id); err != nil {
				return "", fmt.Errorf("resume_workflow: %w", err)
			}
			return marshalAny(map[string]string{"status": "resuming", "workflow_id": id})
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
		Name: "get_session",
		Description: "Get session metadata (status, role, repo, timestamps). " +
			"Result content is never included — use get_session_result to fetch it. " +
			"If a result exists, ResultBytesFull shows its size.",
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
			return slimSession(sess, false, 0)
		},
	})

	s.register(Tool{
		Name: "get_session_result",
		Description: "Fetch the Result text of a completed session. " +
			"Truncated to 4000 characters by default; set include_full_result=true for the complete output (may be very large). " +
			"Check ResultBytesFull to see total size before requesting the full result.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"session_id":          map[string]any{"type": "string", "description": "Session ID"},
				"include_full_result": map[string]any{"type": "boolean", "description": "Return full Result text (default false, truncates at 4000 chars)"},
			},
			"required": []string{"session_id"},
		},
		Handler: func(ctx context.Context, args map[string]any) (string, error) {
			if s.sh == nil {
				return "not implemented", nil
			}
			id, _ := args["session_id"].(string)
			includeFullResult, _ := args["include_full_result"].(bool)
			sess, ok := s.sh.GetSession(id)
			if !ok {
				return "not found", nil
			}
			maxBytes := 4000
			if includeFullResult {
				maxBytes = 1<<31 - 1
			}
			return slimSession(sess, true, maxBytes)
		},
	})

	s.register(Tool{
		Name: "list_sessions",
		Description: "List sessions. By default returns only active sessions (booting, configuring, ready). " +
			"Set status=\"all\" to include stopped and failed sessions. " +
			"Prompt and Result are always omitted; use get_session for those fields.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"status": map[string]any{"type": "string", "description": "Filter: \"active\" (default) returns non-terminal sessions only; \"all\" returns every session"},
			},
		},
		Handler: func(ctx context.Context, args map[string]any) (string, error) {
			if s.sh == nil {
				return "not implemented", nil
			}
			statusFilter, _ := args["status"].(string)
			showAll := statusFilter == "all"

			sessions, err := s.sh.ListSessions()
			if err != nil {
				return "", fmt.Errorf("list_sessions: %w", err)
			}

			slim := make([]map[string]any, 0, len(sessions))
			for _, sess := range sessions {
				b, err := json.Marshal(sess)
				if err != nil {
					continue
				}
				var m map[string]any
				if err := json.Unmarshal(b, &m); err != nil {
					continue
				}
				if !showAll && !isActiveSession(m) {
					continue
				}
				delete(m, "Prompt")
				delete(m, "Result")
				slim = append(slim, m)
			}
			return marshalAny(slim)
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
