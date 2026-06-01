package workflow

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// --- fakeSessionStarter ---

type fakeCall struct {
	role, prompt, repoURL, branch, sessionID string
}

type fakeSessionStarter struct {
	mu      sync.Mutex
	calls   []fakeCall
	results map[string]string // sessionID → result text
	errs    map[string]error  // sessionID → wait error
	counter atomic.Int32
	createErr error // if set, CreateSession returns this
}

func (f *fakeSessionStarter) CreateSession(_ context.Context, role, prompt, repoURL, branch string) (string, error) {
	if f.createErr != nil {
		return "", f.createErr
	}
	id := fmt.Sprintf("fake-session-%d", f.counter.Add(1))
	f.mu.Lock()
	f.calls = append(f.calls, fakeCall{role: role, prompt: prompt, repoURL: repoURL, branch: branch, sessionID: id})
	f.mu.Unlock()
	return id, nil
}

func (f *fakeSessionStarter) WaitForSession(_ context.Context, id string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err, ok := f.errs[id]; ok {
		return "", err
	}
	if result, ok := f.results[id]; ok {
		return result, nil
	}
	return "ok", nil
}

// callsForRole returns all recorded calls with the given role.
func (f *fakeSessionStarter) callsForRole(role string) []fakeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []fakeCall
	for _, c := range f.calls {
		if c.role == role {
			out = append(out, c)
		}
	}
	return out
}

// planResult returns a valid plan JSON for use as a research or plan stage result.
func planResult(tasks ...string) string {
	var items []string
	for i, desc := range tasks {
		items = append(items, fmt.Sprintf(`{"id":"task-%d","description":%q}`, i+1, desc))
	}
	return "[" + strings.Join(items, ",") + "]"
}

// newTestEngine builds an Engine with the given fake and a stub mergeFn that
// always succeeds. Tests can override eng.mergeFn after construction.
func newTestEngine(t *testing.T, sessions *fakeSessionStarter) *Engine {
	t.Helper()
	store := newInMemStore()
	eng := NewEngine(context.Background(), store, sessions, 4, "test-token")
	eng.mergeFn = func(_ context.Context, _, _, _ string, branches []string) ([]string, []string, error) {
		return branches, nil, nil
	}
	return eng
}

// newInMemStore returns a Store backed by a temp file.
func newInMemStore() *Store {
	dir := mustTempDir()
	s, err := Open(dir + "/workflows.json")
	if err != nil {
		panic(err)
	}
	return s
}

// mustTempDir creates a temp dir or panics.
func mustTempDir() string {
	d, err := os.MkdirTemp("", "rubbish-test-*")
	if err != nil {
		panic(err)
	}
	return d
}

// runWorkflow submits a workflow and blocks until it reaches a terminal state.
func runWorkflow(t *testing.T, eng *Engine, sessions *fakeSessionStarter, planJSON string) (wfID string) {
	t.Helper()
	// The research stage result feeds into plan; plan stage needs to return JSON tasks.
	// We configure WaitForSession to return planJSON for any plan-role session.
	// Research stage returns a plain string; plan stage returns task JSON.
	sessions.mu.Lock()
	if sessions.results == nil {
		sessions.results = map[string]string{}
	}
	sessions.mu.Unlock()

	// Override WaitForSession to return planJSON for plan sessions.
	// We wrap the fake to intercept plan calls.
	origResults := sessions.results

	// sessions auto-return "ok" for research; we need plan sessions to return planJSON.
	// Track plan session IDs by hooking into CreateSession via calls inspection.
	// Simpler: set a sentinel that WaitForSession checks by role-recording.
	sessions.mu.Lock()
	sessions.results = origResults
	sessions.mu.Unlock()

	wfID, err := eng.Submit(context.Background(), "test goal", "https://github.com/test/repo.git", "main")
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}

	// Poll until terminal.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		wf, ok := eng.store.Get(wfID)
		if !ok {
			t.Fatalf("workflow %s not found", wfID)
		}
		if wf.Status == StatusDone || wf.Status == StatusFailed {
			return wfID
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("workflow %s did not reach terminal state within timeout", wfID)
	return ""
}

// --- Tests ---

func TestIsUsageLimitError(t *testing.T) {
	trueCases := []string{
		"You've hit your org's monthly usage limit",
		"You've hit your weekly limit · resets Mon 12:00am",
		"You've hit your session limit · resets 3:45pm",
		"You've hit your Opus limit · resets tomorrow",
		// case-insensitive lower variant
		"you've hit your monthly limit",
	}
	for _, s := range trueCases {
		if !IsUsageLimitError(s) {
			t.Errorf("IsUsageLimitError(%q) = false, want true", s)
		}
	}

	falseCases := []string{
		"",
		"Task complete. All files updated.",
		// Contains "usage" and "limit" but not the Claude-specific phrase
		"I updated the usage limit handler in engine.go",
		"The rate limit for this endpoint is 100 req/s",
		"Build failed: exit status 1",
	}
	for _, s := range falseCases {
		if IsUsageLimitError(s) {
			t.Errorf("IsUsageLimitError(%q) = true, want false", s)
		}
	}
}

func TestImplementBranchName(t *testing.T) {
	cases := []struct {
		wfID     string
		stageIdx int
		want     string
	}{
		{"abcdef1234567890", 0, "implement/abcdef12-0"},
		{"abcdef1234567890", 3, "implement/abcdef12-3"},
		{"ab", 1, "implement/ab-1"},
		{"", 0, "implement/-0"},
	}
	for _, tc := range cases {
		got := implementBranchName(tc.wfID, tc.stageIdx)
		if got != tc.want {
			t.Errorf("implementBranchName(%q, %d) = %q, want %q", tc.wfID, tc.stageIdx, got, tc.want)
		}
	}
}

func TestNewEngine_TokenStored(t *testing.T) {
	store := newInMemStore()
	eng := NewEngine(context.Background(), store, &fakeSessionStarter{}, 3, "my-token")
	if eng.token != "my-token" {
		t.Errorf("token = %q, want %q", eng.token, "my-token")
	}
}

func TestImplementPromptIncludesBranchName(t *testing.T) {
	sessions := &fakeSessionStarter{results: map[string]string{}}
	eng := newTestEngine(t, sessions)

	// Make plan sessions return valid task JSON.
	planCalled := false
	eng.sessions = &interceptingStarter{
		fakeSessionStarter: sessions,
		onWait: func(id string, calls []fakeCall) string {
			for _, c := range calls {
				if c.sessionID == id && c.role == "plan" {
					planCalled = true
					return planResult("Do something useful")
				}
			}
			return "ok"
		},
	}

	wfID, err := eng.Submit(context.Background(), "test", "https://github.com/test/repo.git", "main")
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		wf, _ := eng.store.Get(wfID)
		if wf.Status == StatusDone || wf.Status == StatusFailed {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !planCalled {
		t.Error("plan stage was never called")
	}

	implCalls := sessions.callsForRole("implement")
	if len(implCalls) == 0 {
		t.Fatal("no implement sessions created")
	}
	for _, c := range implCalls {
		if !strings.Contains(c.prompt, "implement/") {
			t.Errorf("implement prompt missing branch name; prompt = %q", c.prompt)
		}
	}
}

// interceptingStarter wraps fakeSessionStarter but routes WaitForSession through onWait.
type interceptingStarter struct {
	*fakeSessionStarter
	onWait func(id string, calls []fakeCall) string
}

func (s *interceptingStarter) WaitForSession(ctx context.Context, id string) (string, error) {
	s.fakeSessionStarter.mu.Lock()
	calls := make([]fakeCall, len(s.fakeSessionStarter.calls))
	copy(calls, s.fakeSessionStarter.calls)
	s.fakeSessionStarter.mu.Unlock()
	return s.onWait(id, calls), nil
}

func TestRunAddsKindMergeStage(t *testing.T) {
	sessions := &fakeSessionStarter{}
	eng := newTestEngine(t, sessions)
	eng.sessions = &interceptingStarter{
		fakeSessionStarter: sessions,
		onWait: func(id string, calls []fakeCall) string {
			for _, c := range calls {
				if c.sessionID == id && c.role == "plan" {
					return planResult("Task A", "Task B")
				}
			}
			return "ok"
		},
	}

	wfID, err := eng.Submit(context.Background(), "test", "https://github.com/test/repo.git", "main")
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	waitTerminal(t, eng, wfID, 10*time.Second)

	wf, ok := eng.store.Get(wfID)
	if !ok {
		t.Fatal("workflow not found")
	}

	var mergeStages []Stage
	for _, s := range wf.Stages {
		if s.Kind == KindMerge {
			mergeStages = append(mergeStages, s)
		}
	}
	if len(mergeStages) != 1 {
		t.Fatalf("expected 1 merge stage, got %d (stages: %v)", len(mergeStages), wf.Stages)
	}
	if mergeStages[0].Status != StageDone {
		t.Errorf("merge stage status = %q, want %q", mergeStages[0].Status, StageDone)
	}
}

func TestRunMergeStageAfterImplement(t *testing.T) {
	sessions := &fakeSessionStarter{}
	eng := newTestEngine(t, sessions)
	eng.sessions = &interceptingStarter{
		fakeSessionStarter: sessions,
		onWait: func(id string, calls []fakeCall) string {
			for _, c := range calls {
				if c.sessionID == id && c.role == "plan" {
					return planResult("Task A")
				}
			}
			return "ok"
		},
	}

	wfID, _ := eng.Submit(context.Background(), "test", "https://github.com/test/repo.git", "main")
	waitTerminal(t, eng, wfID, 10*time.Second)

	wf, _ := eng.store.Get(wfID)
	lastKind := wf.Stages[len(wf.Stages)-1].Kind
	if lastKind != KindMerge {
		t.Errorf("last stage kind = %q, want %q", lastKind, KindMerge)
	}
}

func TestRunMergeStageFailed(t *testing.T) {
	sessions := &fakeSessionStarter{}
	eng := newTestEngine(t, sessions)
	eng.sessions = &interceptingStarter{
		fakeSessionStarter: sessions,
		onWait: func(id string, calls []fakeCall) string {
			for _, c := range calls {
				if c.sessionID == id && c.role == "plan" {
					return planResult("Task A")
				}
			}
			return "ok"
		},
	}
	eng.mergeFn = func(_ context.Context, _, _, _ string, _ []string) ([]string, []string, error) {
		return nil, nil, fmt.Errorf("push rejected: not fast-forward")
	}

	wfID, _ := eng.Submit(context.Background(), "test", "https://github.com/test/repo.git", "main")
	waitTerminal(t, eng, wfID, 10*time.Second)

	wf, _ := eng.store.Get(wfID)
	if wf.Status != StatusFailed {
		t.Errorf("workflow status = %q, want %q", wf.Status, StatusFailed)
	}

	var mergeStage *Stage
	for i := range wf.Stages {
		if wf.Stages[i].Kind == KindMerge {
			mergeStage = &wf.Stages[i]
		}
	}
	if mergeStage == nil {
		t.Fatal("no merge stage found")
	}
	if mergeStage.Status != StageFailed {
		t.Errorf("merge stage status = %q, want %q", mergeStage.Status, StageFailed)
	}
	if mergeStage.Error == "" {
		t.Error("merge stage error should be non-empty")
	}
}

func TestRunPartialImplementSuccess(t *testing.T) {
	sessions := &fakeSessionStarter{}
	var mergeBranches []string
	var mergeMu sync.Mutex

	eng := newTestEngine(t, sessions)
	eng.sessions = &interceptingStarter{
		fakeSessionStarter: sessions,
		onWait: func(id string, calls []fakeCall) string {
			for _, c := range calls {
				if c.sessionID == id && c.role == "plan" {
					return planResult("Task A", "Task B")
				}
				// Make the second implement fail.
				if c.sessionID == id && c.role == "implement" {
					sessions.mu.Lock()
					var implCount int
					for _, cc := range sessions.calls {
						if cc.role == "implement" {
							implCount++
						}
					}
					sessions.mu.Unlock()
					if implCount >= 2 {
						return ""
					}
				}
			}
			return "ok"
		},
	}
	// Make the second implement session fail via errs map after creation.
	// We intercept CreateSession instead by making the second implement session return error on wait.
	eng.sessions = &failSecondImplement{
		interceptingStarter: &interceptingStarter{
			fakeSessionStarter: sessions,
			onWait: func(id string, calls []fakeCall) string {
				for _, c := range calls {
					if c.sessionID == id && c.role == "plan" {
						return planResult("Task A", "Task B")
					}
				}
				return "ok"
			},
		},
	}
	eng.mergeFn = func(_ context.Context, _, _, _ string, branches []string) ([]string, []string, error) {
		mergeMu.Lock()
		mergeBranches = append(mergeBranches, branches...)
		mergeMu.Unlock()
		return branches, nil, nil
	}

	wfID, _ := eng.Submit(context.Background(), "test", "https://github.com/test/repo.git", "main")
	waitTerminal(t, eng, wfID, 10*time.Second)

	wf, _ := eng.store.Get(wfID)
	// Workflow should be failed because one implement stage failed.
	if wf.Status != StatusFailed {
		t.Errorf("workflow status = %q, want %q", wf.Status, StatusFailed)
	}

	// Merge should only have received branches from succeeded implements.
	mergeMu.Lock()
	defer mergeMu.Unlock()
	for _, b := range mergeBranches {
		if strings.Contains(b, "-fail-") {
			t.Errorf("failed branch %q should not have been passed to merge", b)
		}
	}
}

// failSecondImplement makes the second implement session fail by returning an error from WaitForSession.
type failSecondImplement struct {
	*interceptingStarter
	implCount atomic.Int32
}

func (f *failSecondImplement) CreateSession(ctx context.Context, role, prompt, repoURL, branch string) (string, error) {
	id, err := f.interceptingStarter.CreateSession(ctx, role, prompt, repoURL, branch)
	if err == nil && role == "implement" {
		n := f.implCount.Add(1)
		if n == 2 {
			// Store a wait error for this session.
			f.interceptingStarter.fakeSessionStarter.mu.Lock()
			if f.interceptingStarter.fakeSessionStarter.errs == nil {
				f.interceptingStarter.fakeSessionStarter.errs = map[string]error{}
			}
			f.interceptingStarter.fakeSessionStarter.errs[id] = fmt.Errorf("implement failed")
			f.interceptingStarter.fakeSessionStarter.mu.Unlock()
		}
	}
	return id, err
}

func (f *failSecondImplement) WaitForSession(ctx context.Context, id string) (string, error) {
	return f.interceptingStarter.fakeSessionStarter.WaitForSession(ctx, id)
}

// waitTerminal polls until the workflow reaches a terminal state.
func waitTerminal(t *testing.T, eng *Engine, wfID string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		wf, ok := eng.store.Get(wfID)
		if !ok {
			t.Fatalf("workflow %s not found", wfID)
		}
		if wf.Status == StatusDone || wf.Status == StatusFailed {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("workflow %s did not reach terminal state within %v", wfID, timeout)
}

// --- Steering hook tests ---

// goodImplementOutput returns an implement stage output that passes structural validation.
func goodImplementOutput() string {
	return "I completed the task.\n\n" +
		`{"committed":true,"commit_sha":"abc123def456","build_ok":true,"notes":""}` +
		"\n\nAll tests pass."
}

// badImplementOutput returns implement output with no terminal block (structural failure).
func badImplementOutput() string {
	return "I tried but couldn't complete the task."
}

// verifyPass returns a verifier output indicating the implementation passed.
func verifyPass() string {
	return `{"pass":true,"issues":[]}`
}

// verifyFail returns a verifier output indicating the implementation failed.
func verifyFail(issues ...string) string {
	quoted := make([]string, len(issues))
	for i, s := range issues {
		quoted[i] = `"` + s + `"`
	}
	issueList := "[" + strings.Join(quoted, ",") + "]"
	return `{"pass":false,"issues":` + issueList + `}`
}

// routingStarter routes WaitForSession results by session role.
type routingStarter struct {
	*fakeSessionStarter
	resultByRole map[string]string // role → result text
}

func (r *routingStarter) WaitForSession(ctx context.Context, id string) (string, error) {
	r.fakeSessionStarter.mu.Lock()
	calls := make([]fakeCall, len(r.fakeSessionStarter.calls))
	copy(calls, r.fakeSessionStarter.calls)
	r.fakeSessionStarter.mu.Unlock()

	for _, c := range calls {
		if c.sessionID == id {
			if result, ok := r.resultByRole[c.role]; ok {
				return result, nil
			}
		}
	}
	return "ok", nil
}

func newRoutingEngine(t *testing.T, byRole map[string]string) (*Engine, *routingStarter) {
	t.Helper()
	fake := &fakeSessionStarter{}
	rs := &routingStarter{fakeSessionStarter: fake, resultByRole: byRole}
	store := newInMemStore()
	eng := NewEngine(context.Background(), store, rs, 4, "test-token")
	eng.mergeFn = func(_ context.Context, _, _, _ string, branches []string) ([]string, []string, error) {
		return branches, nil, nil
	}
	return eng, rs
}

func TestEngine_implementPassesStructuralAndVerifier(t *testing.T) {
	eng, rs := newRoutingEngine(t, map[string]string{
		"research":  "research findings",
		"plan":      planResult("Do something useful"),
		"implement": goodImplementOutput(),
		"verify":    verifyPass(),
	})

	wfID, err := eng.Submit(context.Background(), "test goal", "https://github.com/test/repo.git", "main")
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	waitTerminal(t, eng, wfID, 10*time.Second)

	wf, _ := eng.store.Get(wfID)
	if wf.Status != StatusDone {
		t.Errorf("workflow status = %q, want %q", wf.Status, StatusDone)
	}

	// Verify session must have been created.
	verifyCalls := rs.fakeSessionStarter.callsForRole("verify")
	if len(verifyCalls) != 1 {
		t.Errorf("expected 1 verify session, got %d", len(verifyCalls))
	}

	// Implement stage must be done, with VerifySessionID set.
	for _, s := range wf.Stages {
		if s.Kind == KindImplement {
			if s.Status != StageDone {
				t.Errorf("implement stage status = %q, want %q", s.Status, StageDone)
			}
			if s.VerifySessionID == "" {
				t.Error("implement stage VerifySessionID should be set")
			}
		}
	}
}

func TestEngine_implementFailsStructuralValidation(t *testing.T) {
	eng, rs := newRoutingEngine(t, map[string]string{
		"research":  "research findings",
		"plan":      planResult("Do something useful"),
		"implement": badImplementOutput(), // no terminal block
	})

	wfID, _ := eng.Submit(context.Background(), "test goal", "https://github.com/test/repo.git", "main")
	waitTerminal(t, eng, wfID, 10*time.Second)

	wf, _ := eng.store.Get(wfID)
	if wf.Status != StatusFailed {
		t.Errorf("workflow status = %q, want %q", wf.Status, StatusFailed)
	}

	// Verify VM must NOT have been created (structural failure short-circuits).
	verifyCalls := rs.fakeSessionStarter.callsForRole("verify")
	if len(verifyCalls) != 0 {
		t.Errorf("expected 0 verify sessions for structural failure, got %d", len(verifyCalls))
	}

	// Implement stage must be failed with a meaningful error.
	for _, s := range wf.Stages {
		if s.Kind == KindImplement {
			if s.Status != StageFailed {
				t.Errorf("implement stage status = %q, want %q", s.Status, StageFailed)
			}
			if s.Error == "" {
				t.Error("implement stage Error should be non-empty")
			}
		}
	}
}

func TestEngine_implementPassesStructuralButFailsVerifier(t *testing.T) {
	eng, rs := newRoutingEngine(t, map[string]string{
		"research":  "research findings",
		"plan":      planResult("Do something useful"),
		"implement": goodImplementOutput(),
		"verify":    verifyFail("go vet: undefined: Foo", "task not completed: missing rate limiting"),
	})

	wfID, _ := eng.Submit(context.Background(), "test goal", "https://github.com/test/repo.git", "main")
	waitTerminal(t, eng, wfID, 10*time.Second)

	wf, _ := eng.store.Get(wfID)
	if wf.Status != StatusFailed {
		t.Errorf("workflow status = %q, want %q", wf.Status, StatusFailed)
	}

	// Verify session must have been created.
	verifyCalls := rs.fakeSessionStarter.callsForRole("verify")
	if len(verifyCalls) != 1 {
		t.Errorf("expected 1 verify session, got %d", len(verifyCalls))
	}

	// Implement stage must be failed with verifier issues in the error.
	for _, s := range wf.Stages {
		if s.Kind == KindImplement {
			if s.Status != StageFailed {
				t.Errorf("implement stage status = %q, want %q", s.Status, StageFailed)
			}
			if !strings.Contains(s.Error, "go vet") {
				t.Errorf("implement stage Error = %q, want to contain verifier issue %q", s.Error, "go vet")
			}
		}
	}
}

func TestEngine_planFailsStructuralValidation(t *testing.T) {
	eng, rs := newRoutingEngine(t, map[string]string{
		"research": "research findings",
		"plan":     "I have a plan but it's not JSON",
	})

	wfID, _ := eng.Submit(context.Background(), "test goal", "https://github.com/test/repo.git", "main")
	waitTerminal(t, eng, wfID, 10*time.Second)

	wf, _ := eng.store.Get(wfID)
	if wf.Status != StatusFailed {
		t.Errorf("workflow status = %q, want %q", wf.Status, StatusFailed)
	}

	// No implement stages should have been created.
	for _, s := range wf.Stages {
		if s.Kind == KindImplement {
			t.Errorf("unexpected implement stage created despite plan failure")
		}
	}

	// Verify sessions: none.
	verifyCalls := rs.fakeSessionStarter.callsForRole("verify")
	if len(verifyCalls) != 0 {
		t.Errorf("expected 0 verify sessions, got %d", len(verifyCalls))
	}
}

func TestEngine_verifySessionIDRecorded(t *testing.T) {
	eng, rs := newRoutingEngine(t, map[string]string{
		"research":  "research findings",
		"plan":      planResult("Do something useful"),
		"implement": goodImplementOutput(),
		"verify":    verifyPass(),
	})

	wfID, _ := eng.Submit(context.Background(), "test goal", "https://github.com/test/repo.git", "main")
	waitTerminal(t, eng, wfID, 10*time.Second)

	wf, _ := eng.store.Get(wfID)

	verifyCalls := rs.fakeSessionStarter.callsForRole("verify")
	if len(verifyCalls) == 0 {
		t.Fatal("no verify sessions created")
	}
	verifySessionID := verifyCalls[0].sessionID

	for _, s := range wf.Stages {
		if s.Kind == KindImplement {
			if s.VerifySessionID != verifySessionID {
				t.Errorf("VerifySessionID = %q, want %q", s.VerifySessionID, verifySessionID)
			}
		}
	}
}

func TestEngine_customVerifierPromptPassedThrough(t *testing.T) {
	eng, rs := newRoutingEngine(t, map[string]string{
		"research":  "research findings",
		"plan":      planResult("Do something useful"),
		"implement": goodImplementOutput(),
		"verify":    verifyPass(),
	})

	// Submit with a custom verifier prompt via WorkflowConfig.
	wf := &Workflow{
		Input:   "test goal",
		RepoURL: "https://github.com/test/repo.git",
		Branch:  "main",
		Config: WorkflowConfig{
			VerifierPrompt: "also verify that error handling follows project conventions",
		},
	}
	wfID, err := eng.SubmitWorkflow(context.Background(), wf)
	if err != nil {
		t.Fatalf("SubmitWorkflow: %v", err)
	}
	waitTerminal(t, eng, wfID, 10*time.Second)

	verifyCalls := rs.fakeSessionStarter.callsForRole("verify")
	if len(verifyCalls) == 0 {
		t.Fatal("no verify sessions created")
	}
	if !strings.Contains(verifyCalls[0].prompt, "also verify that error handling follows project conventions") {
		t.Errorf("verify prompt missing custom verifier prompt; prompt = %q", verifyCalls[0].prompt)
	}
}
