package workflow

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"time"
)

// ErrUsageLimit is returned when an agent session result indicates the Claude
// API usage limit was reached. The stage is failed but its Input is preserved
// so Resume() can re-run it once limits reset.
var ErrUsageLimit = errors.New("usage limit reached")

// isUsageLimitResult reports whether a session result string signals that the
// Claude API usage limit was hit rather than a real agent output.
func isUsageLimitResult(result string) bool {
	return strings.Contains(result, "monthly usage limit") ||
		strings.Contains(result, "usage limit")
}

// SessionStarter is the subset of SessionManager the engine needs.
// The real implementation lives in cmd/poc; Agent E will wire it up.
type SessionStarter interface {
	// CreateSession creates a non-interactive session and returns its ID.
	CreateSession(ctx context.Context, role, prompt, repoURL, branch string) (string, error)
	// WaitForSession polls until the session reaches a terminal state (stopped or
	// failed), then returns the Result field. Returns an error if the session
	// failed or if ctx expires before the session terminates.
	WaitForSession(ctx context.Context, id string) (result string, err error)
}

// Engine drives the rigid research→plan→implement pipeline.
type Engine struct {
	store      *Store
	sessions   SessionStarter
	maxWorkers int     // limits concurrent implement-stage sessions; default 3
	serverCtx  context.Context // lifetime context for background goroutines
}

// NewEngine constructs an Engine. maxWorkers controls the implement fan-out
// concurrency; pass 0 or negative to use the default of 3. serverCtx should
// be the server's lifetime context so workflow goroutines aren't canceled when
// the HTTP request that triggered Submit finishes.
func NewEngine(serverCtx context.Context, store *Store, sessions SessionStarter, maxWorkers int) *Engine {
	if maxWorkers <= 0 {
		maxWorkers = 3
	}
	if serverCtx == nil {
		serverCtx = context.Background()
	}
	return &Engine{
		store:      store,
		sessions:   sessions,
		maxWorkers: maxWorkers,
		serverCtx:  serverCtx,
	}
}

// newID generates a random 32-character hex workflow ID.
func newID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate workflow id: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// Submit creates a new workflow run and starts driving it asynchronously.
// Returns the workflow ID immediately.
func (e *Engine) Submit(ctx context.Context, input, repoURL, branch string) (string, error) {
	id, err := newID()
	if err != nil {
		return "", err
	}

	now := time.Now()
	wf := &Workflow{
		ID:        id,
		Input:     input,
		RepoURL:   repoURL,
		Branch:    branch,
		Status:    StatusQueued,
		CreatedAt: now,
		Stages: []Stage{
			{ID: id + "-research", Kind: KindResearch, Status: StagePending},
			{ID: id + "-plan", Kind: KindPlan, Status: StagePending},
		},
	}

	if err := e.store.Upsert(wf); err != nil {
		return "", fmt.Errorf("persist queued workflow: %w", err)
	}

	wf.Status = StatusRunning
	if err := e.store.Upsert(wf); err != nil {
		return "", fmt.Errorf("persist running workflow: %w", err)
	}

	go e.run(e.serverCtx, id)
	return id, nil
}

// run is the pipeline goroutine that drives a workflow forward stage by stage.
func (e *Engine) run(ctx context.Context, id string) {
	// ----- research stage -----
	researchOutput, err := e.executeNamedStage(ctx, id, KindResearch, 20*time.Minute, func(wf *Workflow) string {
		return "You are a research agent. Investigate the following goal and provide structured findings that will be used by a planning agent.\n\n" +
			"Goal: " + wf.Input + "\n\n" +
			"Repository: " + wf.RepoURL + " (branch: " + wf.Branch + ")\n\n" +
			"Provide thorough findings in plain text."
	})
	if err != nil {
		e.failWorkflow(id, err)
		return
	}

	// ----- plan stage -----
	planOutput, err := e.executeNamedStage(ctx, id, KindPlan, 10*time.Minute, func(wf *Workflow) string {
		return "You are a planning agent. Based on the research findings below, create a concrete implementation plan.\n\n" +
			"Goal: " + wf.Input + "\n\n" +
			"Research findings:\n" + researchOutput + "\n\n" +
			"Output ONLY a JSON array of task objects. Each object must have:\n" +
			"- \"id\": a short unique identifier (e.g. \"task-1\")\n" +
			"- \"description\": a clear, self-contained task description that a coding agent can execute independently\n\n" +
			"Example: [{\"id\":\"task-1\",\"description\":\"Add X to file Y\"},{\"id\":\"task-2\",\"description\":\"Update Z\"}]\n\n" +
			"Do not include any text before or after the JSON array."
	})
	if err != nil {
		e.failWorkflow(id, err)
		return
	}

	// Parse plan output into tasks.
	tasks, err := parsePlanOutput(planOutput)
	if err != nil {
		e.failWorkflow(id, fmt.Errorf("parse plan output: %w", err))
		return
	}

	// Add implement stages to the workflow.
	wf, ok := e.store.Get(id)
	if !ok {
		slog.Warn("workflow not found after plan stage", "workflow", id[:8])
		return
	}
	for i, task := range tasks {
		wf.Stages = append(wf.Stages, Stage{
			ID:     fmt.Sprintf("%s-implement-%d", id, i),
			Kind:   KindImplement,
			Status: StagePending,
			Input:  task.Description,
		})
	}
	if err := e.store.Upsert(wf); err != nil {
		e.failWorkflow(id, fmt.Errorf("persist implement stages: %w", err))
		return
	}

	// ----- implement fan-out -----
	// Reload to get the updated stage slice with stable indices.
	wf, ok = e.store.Get(id)
	if !ok {
		slog.Warn("workflow not found before implement fan-out", "workflow", id[:8])
		return
	}

	sem := make(chan struct{}, e.maxWorkers)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error

	for i := range wf.Stages {
		if wf.Stages[i].Kind != KindImplement {
			continue
		}
		stageIdx := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			stage := &wf.Stages[stageIdx]
			taskDesc := stage.Input // captured before any mutation

			implCtx, implCancel := context.WithTimeout(ctx, 30*time.Minute)
			defer implCancel()

			prompt := "You are a coding agent. Complete the following task.\n\n" +
				"Task: " + taskDesc + "\n\n" +
				"Repository: " + wf.RepoURL + " (branch: " + wf.Branch + ")\n\n" +
				"Implement the task completely. Commit your changes when done."

			if runErr := e.runStage(implCtx, id, stage.ID, KindImplement, prompt); runErr != nil {
				mu.Lock()
				if firstErr == nil {
					firstErr = runErr
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	// Mark workflow terminal.
	wf, ok = e.store.Get(id)
	if !ok {
		slog.Warn("workflow not found after implement fan-out", "workflow", id[:8])
		return
	}
	if firstErr != nil {
		wf.Status = StatusFailed
		wf.Error = firstErr.Error()
	} else {
		wf.Status = StatusDone
	}
	if err := e.store.Upsert(wf); err != nil {
		slog.Warn("workflow: persist final status", "workflow", id[:8], "err", err)
	}
	slog.Info("workflow: finished", "workflow", id[:8], "status", wf.Status)
}

// executeNamedStage finds the first pending stage of the given kind, executes
// it, and returns its output. The promptFn is called with a fresh copy of the
// workflow to build the prompt string.
func (e *Engine) executeNamedStage(
	ctx context.Context,
	wfID string,
	kind StageKind,
	timeout time.Duration,
	promptFn func(*Workflow) string,
) (string, error) {
	wf, ok := e.store.Get(wfID)
	if !ok {
		return "", fmt.Errorf("workflow %s not found", wfID)
	}

	// Find the stage index.
	stageIdx := -1
	for i := range wf.Stages {
		if wf.Stages[i].Kind == kind && wf.Stages[i].Status == StagePending {
			stageIdx = i
			break
		}
	}
	if stageIdx < 0 {
		return "", fmt.Errorf("no pending %s stage in workflow %s", kind, wfID)
	}

	prompt := promptFn(wf)
	stageID := wf.Stages[stageIdx].ID

	stageCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	if err := e.runStage(stageCtx, wfID, stageID, kind, prompt); err != nil {
		return "", err
	}

	// Reload to get the persisted output.
	wf, ok = e.store.Get(wfID)
	if !ok {
		return "", fmt.Errorf("workflow %s not found after %s stage", wfID, kind)
	}
	for i := range wf.Stages {
		if wf.Stages[i].ID == stageID {
			return wf.Stages[i].Output, nil
		}
	}
	return "", fmt.Errorf("stage %s not found after execution", stageID)
}

// runStage executes a single stage: sets it to running, creates the session,
// waits for it, and marks it done or failed. All mutations go through
// e.store.Upsert on a freshly loaded workflow copy to avoid stale-pointer races.
func (e *Engine) runStage(ctx context.Context, wfID, stageID string, kind StageKind, prompt string) error {
	// --- transition to running ---
	wf, ok := e.store.Get(wfID)
	if !ok {
		return fmt.Errorf("workflow %s not found", wfID)
	}
	idx := stageIndex(wf, stageID)
	if idx < 0 {
		return fmt.Errorf("stage %s not found in workflow %s", stageID, wfID)
	}
	wf.Stages[idx].Status = StageRunning
	wf.Stages[idx].Input = prompt
	if err := e.store.Upsert(wf); err != nil {
		return fmt.Errorf("persist stage %s running: %w", stageID, err)
	}

	// --- create session (retry until a slot is free) ---
	// Non-interactive stages spin down their VM when done, freeing slots for
	// the next stage or concurrent workers. Retry here so workers queue
	// naturally rather than failing immediately when all slots are busy.
	var sessionID string
	for {
		var cerr error
		sessionID, cerr = e.sessions.CreateSession(ctx, string(kind), prompt, wf.RepoURL, wf.Branch)
		if cerr == nil {
			break
		}
		if !strings.Contains(cerr.Error(), "no slots available") {
			return e.markStageFailed(wfID, stageID, fmt.Errorf("create session: %w", cerr))
		}
		slog.Info("workflow: waiting for free slot", "workflow", wfID, "stage", stageID)
		select {
		case <-ctx.Done():
			return e.markStageFailed(wfID, stageID, ctx.Err())
		case <-time.After(5 * time.Second):
		}
	}

	// Persist sessionID immediately.
	wf, ok = e.store.Get(wfID)
	if !ok {
		return fmt.Errorf("workflow %s not found after CreateSession", wfID)
	}
	idx = stageIndex(wf, stageID)
	if idx >= 0 {
		wf.Stages[idx].SessionID = sessionID
		e.store.Upsert(wf) //nolint:errcheck — best effort
	}

	// --- wait for result ---
	result, err := e.sessions.WaitForSession(ctx, sessionID)
	if err != nil {
		return e.markStageFailed(wfID, stageID, fmt.Errorf("session %s: %w", sessionID, err))
	}

	// Detect usage-limit responses — the session "succeeds" but the agent never ran.
	// Fail the stage so Resume() can re-run it once limits reset.
	if isUsageLimitResult(result) {
		slog.Warn("workflow: stage hit usage limit", "workflow", wfID[:8], "stage", stageID)
		return e.markStageFailed(wfID, stageID, fmt.Errorf("%w (session %s)", ErrUsageLimit, sessionID))
	}

	// --- mark done ---
	wf, ok = e.store.Get(wfID)
	if !ok {
		return fmt.Errorf("workflow %s not found after WaitForSession", wfID)
	}
	idx = stageIndex(wf, stageID)
	if idx < 0 {
		return fmt.Errorf("stage %s missing after WaitForSession", stageID)
	}
	wf.Stages[idx].Output = result
	wf.Stages[idx].Status = StageDone
	if err := e.store.Upsert(wf); err != nil {
		return fmt.Errorf("persist stage %s done: %w", stageID, err)
	}
	slog.Info("workflow: stage done", "workflow", wfID[:8], "stage", stageID, "bytes", len(result))
	return nil
}

// markStageFailed transitions a stage to StageFailed, records the cause in
// Stage.Error, and returns the original error so callers can propagate it.
func (e *Engine) markStageFailed(wfID, stageID string, cause error) error {
	wf, ok := e.store.Get(wfID)
	if !ok {
		slog.Warn("workflow: cannot mark stage failed, workflow not found", "workflow", wfID[:8], "stage", stageID)
		return cause
	}
	idx := stageIndex(wf, stageID)
	if idx >= 0 {
		wf.Stages[idx].Status = StageFailed
		wf.Stages[idx].Error = cause.Error()
		e.store.Upsert(wf) //nolint:errcheck
	}
	return cause
}

// failWorkflow marks the workflow as StatusFailed and logs.
func (e *Engine) failWorkflow(id string, cause error) {
	slog.Error("workflow: failed", "workflow", id[:8], "err", cause)
	wf, ok := e.store.Get(id)
	if !ok {
		return
	}
	wf.Status = StatusFailed
	wf.Error = cause.Error()
	e.store.Upsert(wf) //nolint:errcheck
}

// Resume re-runs all StageFailed stages in a workflow that have a saved Input
// (i.e. the prompt was already generated). It resets them to StagePending,
// marks the workflow StatusRunning, then fans out in a goroutine — returning
// immediately so the caller is not blocked.
//
// Typical use: after org usage limits reset, call Resume to continue a workflow
// that was partially completed.
func (e *Engine) Resume(ctx context.Context, wfID string) error {
	wf, ok := e.store.Get(wfID)
	if !ok {
		return fmt.Errorf("workflow %s not found", wfID)
	}

	resumable := 0
	for i := range wf.Stages {
		if wf.Stages[i].Status == StageFailed && wf.Stages[i].Input != "" {
			wf.Stages[i].Status = StagePending
			wf.Stages[i].Error = ""
			resumable++
		}
	}
	if resumable == 0 {
		return fmt.Errorf("workflow %s has no resumable (failed+input) stages", wfID)
	}

	wf.Status = StatusRunning
	wf.Error = ""
	if err := e.store.Upsert(wf); err != nil {
		return fmt.Errorf("persist resume: %w", err)
	}
	slog.Info("workflow: resuming", "workflow", wfID[:8], "stages", resumable)
	go e.resumeRun(e.serverCtx, wfID)
	return nil
}

// resumeRun fans out all StagePending stages in the workflow (the ones Reset
// set to Pending), runs them in parallel, then marks the workflow terminal.
func (e *Engine) resumeRun(ctx context.Context, wfID string) {
	wf, ok := e.store.Get(wfID)
	if !ok {
		slog.Warn("workflow: resumeRun — not found", "workflow", wfID[:8])
		return
	}

	sem := make(chan struct{}, e.maxWorkers)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error

	for i := range wf.Stages {
		if wf.Stages[i].Status != StagePending || wf.Stages[i].Input == "" {
			continue
		}
		stageID := wf.Stages[i].ID
		kind := wf.Stages[i].Kind
		prompt := wf.Stages[i].Input

		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			stageCtx, cancel := context.WithTimeout(ctx, 30*time.Minute)
			defer cancel()

			if runErr := e.runStage(stageCtx, wfID, stageID, kind, prompt); runErr != nil {
				mu.Lock()
				if firstErr == nil {
					firstErr = runErr
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	wf, ok = e.store.Get(wfID)
	if !ok {
		return
	}
	if firstErr != nil {
		wf.Status = StatusFailed
		wf.Error = firstErr.Error()
	} else {
		wf.Status = StatusDone
	}
	e.store.Upsert(wf) //nolint:errcheck
	slog.Info("workflow: resume finished", "workflow", wfID[:8], "status", wf.Status)
}

// stageIndex returns the index of the stage with the given ID, or -1.
func stageIndex(wf *Workflow, stageID string) int {
	for i := range wf.Stages {
		if wf.Stages[i].ID == stageID {
			return i
		}
	}
	return -1
}

// Task is the parsed representation of a single item from the plan output.
type Task struct {
	ID          string `json:"id"`
	Description string `json:"description"`
}

var jsonArrayRe = regexp.MustCompile(`(?s)\[.*\]`)

// parsePlanOutput extracts a JSON array of Task objects from the LLM output.
// It tolerates leading/trailing prose by scanning for the first '[' and last ']'.
func parsePlanOutput(output string) ([]Task, error) {
	loc := jsonArrayRe.FindStringIndex(output)
	if loc == nil {
		return nil, fmt.Errorf("no JSON array found in plan output")
	}
	raw := output[loc[0]:loc[1]]

	var tasks []Task
	if err := json.Unmarshal([]byte(raw), &tasks); err != nil {
		return nil, fmt.Errorf("unmarshal plan JSON: %w", err)
	}
	if len(tasks) == 0 {
		return nil, fmt.Errorf("plan produced an empty task list")
	}
	return tasks, nil
}

// RecoverInProgress re-spawns goroutines for any non-terminal workflows found
// in the store. Call this at startup after the session store is loaded.
func (e *Engine) RecoverInProgress(ctx context.Context) error {
	wfs := e.store.List()
	recovered := 0
	for _, wf := range wfs {
		if wf.Status == StatusQueued || wf.Status == StatusRunning {
			go e.run(e.serverCtx, wf.ID)
			recovered++
		}
	}
	slog.Info("workflow: RecoverInProgress", "recovered", recovered)
	return nil
}

// Get returns the workflow with the given ID cast to any, or false if not found.
// Implements the WorkflowHandler interface used by internal/mcp/tools.go.
func (e *Engine) Get(id string) (any, bool) {
	wf, ok := e.store.Get(id)
	if !ok {
		return nil, false
	}
	return wf, true
}

// List returns all workflows sorted by CreatedAt descending, cast to []any.
// Implements the WorkflowHandler interface used by internal/mcp/tools.go.
func (e *Engine) List() []any {
	wfs := e.store.List()
	out := make([]any, len(wfs))
	for i, wf := range wfs {
		out[i] = wf
	}
	return out
}
