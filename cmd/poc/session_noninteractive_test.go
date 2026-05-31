package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/edwinavalos/rubbish/internal/session"
	"github.com/edwinavalos/rubbish/internal/workflow"
)

// TestNonInteractive_Success verifies that a non-interactive (implement role)
// session stores the parsed JSON result and reaches StateStopped.
func TestNonInteractive_Success(t *testing.T) {
	snap := &fakeSnapshotter{}
	handle := &fakeVMHandle{}
	launcher := &fakeLauncher{handle: handle}
	runner := &fakeSetupRunner{
		captureStdout: `{"type":"result","subtype":"success","result":"refactored 3 files","is_error":false}`,
		captureStderr: "",
	}
	mgr := newTestManager(snap, launcher, &fakeBridgeFactory{runner})

	sess, err := mgr.Create(context.Background(), "implement", "do the refactor", "", false, "", "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	waitState(t, mgr, sess.ID, session.StateStopped, 5*time.Second)

	mgr.mu.RLock()
	result := sess.Result
	mgr.mu.RUnlock()

	if result != "refactored 3 files" {
		t.Errorf("Result = %q, want %q", result, "refactored 3 files")
	}
}

// TestNonInteractive_UsageLimitInStderr verifies that when claude's stderr
// contains the usage-limit message, the session transitions to StateFailed
// with an ErrorMsg that wraps ErrUsageLimit.
func TestNonInteractive_UsageLimitInStderr(t *testing.T) {
	snap := &fakeSnapshotter{}
	handle := &fakeVMHandle{}
	launcher := &fakeLauncher{handle: handle}
	runner := &fakeSetupRunner{
		captureStdout: "",
		captureStderr: "You've hit your org's monthly usage limit",
	}
	mgr := newTestManager(snap, launcher, &fakeBridgeFactory{runner})

	sess, err := mgr.Create(context.Background(), "implement", "do the refactor", "", false, "", "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	waitState(t, mgr, sess.ID, session.StateFailed, 5*time.Second)

	mgr.mu.RLock()
	errMsg := sess.Error
	mgr.mu.RUnlock()

	if errMsg == "" {
		t.Error("session Error should be set after usage limit failure")
	}

	// WaitForSession should return an error wrapping ErrUsageLimit.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, waitErr := mgr.WaitForSession(ctx, sess.ID)
	if waitErr == nil {
		t.Fatal("WaitForSession should return error for failed session")
	}
	if !errors.Is(waitErr, workflow.ErrUsageLimit) {
		t.Errorf("WaitForSession error = %v, want to wrap workflow.ErrUsageLimit", waitErr)
	}
}

// TestNonInteractive_NonJSONFallback verifies that if claude outputs plain text
// (not JSON), the raw text is stored as the result unchanged.
func TestNonInteractive_NonJSONFallback(t *testing.T) {
	snap := &fakeSnapshotter{}
	handle := &fakeVMHandle{}
	launcher := &fakeLauncher{handle: handle}
	runner := &fakeSetupRunner{
		captureStdout: "plain text output from agent",
		captureStderr: "",
	}
	mgr := newTestManager(snap, launcher, &fakeBridgeFactory{runner})

	sess, err := mgr.Create(context.Background(), "research", "investigate the codebase", "", false, "", "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	waitState(t, mgr, sess.ID, session.StateStopped, 5*time.Second)

	mgr.mu.RLock()
	result := sess.Result
	mgr.mu.RUnlock()

	if result != "plain text output from agent" {
		t.Errorf("Result = %q, want raw text fallback", result)
	}
}
