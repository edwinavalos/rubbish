package main

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/edwinavalos/rubbish/internal/session"
	"github.com/edwinavalos/rubbish/internal/vm"
	"golang.org/x/crypto/ssh"
)

// ---- Fakes ------------------------------------------------------------------

type fakeSnapshotter struct {
	createErr  error
	deleteErr  error
	createPath string
	createN    atomic.Int32
	deleteN    atomic.Int32
}

func (f *fakeSnapshotter) CreateSnapshot(_ string) (string, error) {
	f.createN.Add(1)
	if f.createErr != nil {
		return "", f.createErr
	}
	p := f.createPath
	if p == "" {
		p = "/dev/fake"
	}
	return p, nil
}
func (f *fakeSnapshotter) DeleteSnapshot(_ string) error {
	f.deleteN.Add(1)
	return f.deleteErr
}
func (f *fakeSnapshotter) InjectNetworkConfig(_, _, _ string) error { return nil }

type fakeVMHandle struct {
	sshErr  error
	stopN   atomic.Int32
}

func (f *fakeVMHandle) WaitForSSH(_ context.Context) error { return f.sshErr }
func (f *fakeVMHandle) Stop(_ context.Context) error {
	f.stopN.Add(1)
	return nil
}

type fakeLauncher struct {
	launchErr error
	handle    *fakeVMHandle
	launchN   atomic.Int32
}

func (f *fakeLauncher) Launch(_ context.Context, _ int, _ string, _ int64, _ []vm.VirtioFSMount) (vmHandle, error) {
	f.launchN.Add(1)
	if f.launchErr != nil {
		return nil, f.launchErr
	}
	return f.handle, nil
}

type fakeSetupRunner struct{ runN atomic.Int32 }

func (f *fakeSetupRunner) RunSetup(_ []string) error {
	f.runN.Add(1)
	return nil
}

type fakeBridgeFactory struct{ runner setupRunner }

func (f *fakeBridgeFactory) NewBridge(_ string, _ ssh.Signer) setupRunner { return f.runner }

// newTestManager builds a SessionManager with all fakes wired in.
func newTestManager(snap *fakeSnapshotter, launcher *fakeLauncher, bridge *fakeBridgeFactory) *SessionManager {
	return newSessionManager(snap, launcher, bridge, nil, "", nil, nil, 4, "", nil)
}

// waitState polls until the session reaches the target state or times out.
func waitState(t *testing.T, mgr *SessionManager, id string, want session.State, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		sess, ok := mgr.Get(id)
		if ok && sess.SessionStatus() == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	sess, _ := mgr.Get(id)
	var got session.State
	if sess != nil {
		got = sess.SessionStatus()
	}
	t.Fatalf("timed out waiting for state %s; got %s", want, got)
}

// ---- Tests ------------------------------------------------------------------

func TestBoot_HappyPath(t *testing.T) {
	snap := &fakeSnapshotter{}
	handle := &fakeVMHandle{}
	launcher := &fakeLauncher{handle: handle}
	runner := &fakeSetupRunner{}
	mgr := newTestManager(snap, launcher, &fakeBridgeFactory{runner})

	sess, err := mgr.Create(context.Background(), "", "", "", false)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	waitState(t, mgr, sess.ID, session.StateReady, 3*time.Second)

	if snap.createN.Load() != 1 {
		t.Errorf("CreateSnapshot called %d times, want 1", snap.createN.Load())
	}
	if launcher.launchN.Load() != 1 {
		t.Errorf("Launch called %d times, want 1", launcher.launchN.Load())
	}
}

func TestBoot_SnapshotFailure(t *testing.T) {
	snap := &fakeSnapshotter{createErr: errors.New("disk full")}
	launcher := &fakeLauncher{handle: &fakeVMHandle{}}
	mgr := newTestManager(snap, launcher, &fakeBridgeFactory{&fakeSetupRunner{}})

	sess, err := mgr.Create(context.Background(), "", "", "", false)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	waitState(t, mgr, sess.ID, session.StateFailed, 3*time.Second)

	if launcher.launchN.Load() != 0 {
		t.Error("Launch should not be called after snapshot failure")
	}
	// Slot should be freed.
	if mgr.usedSlots[sess.Slot] {
		t.Error("slot should be freed after failure")
	}
}

func TestBoot_LaunchFailure(t *testing.T) {
	snap := &fakeSnapshotter{}
	launcher := &fakeLauncher{launchErr: errors.New("tap busy"), handle: &fakeVMHandle{}}
	mgr := newTestManager(snap, launcher, &fakeBridgeFactory{&fakeSetupRunner{}})

	sess, err := mgr.Create(context.Background(), "", "", "", false)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	waitState(t, mgr, sess.ID, session.StateFailed, 3*time.Second)

	if snap.deleteN.Load() != 1 {
		t.Errorf("DeleteSnapshot called %d times after launch failure, want 1", snap.deleteN.Load())
	}
	if mgr.usedSlots[sess.Slot] {
		t.Error("slot should be freed after failure")
	}
}

func TestBoot_SSHTimeout(t *testing.T) {
	snap := &fakeSnapshotter{}
	handle := &fakeVMHandle{sshErr: errors.New("timeout")}
	launcher := &fakeLauncher{handle: handle}
	mgr := newTestManager(snap, launcher, &fakeBridgeFactory{&fakeSetupRunner{}})

	sess, err := mgr.Create(context.Background(), "", "", "", false)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	waitState(t, mgr, sess.ID, session.StateFailed, 3*time.Second)

	if handle.stopN.Load() != 1 {
		t.Errorf("VM Stop called %d times, want 1", handle.stopN.Load())
	}
	if snap.deleteN.Load() != 1 {
		t.Errorf("DeleteSnapshot called %d times after SSH timeout, want 1", snap.deleteN.Load())
	}
	if mgr.usedSlots[sess.Slot] {
		t.Error("slot should be freed after failure")
	}
}

func TestStop_ReadySession(t *testing.T) {
	snap := &fakeSnapshotter{}
	handle := &fakeVMHandle{}
	launcher := &fakeLauncher{handle: handle}
	mgr := newTestManager(snap, launcher, &fakeBridgeFactory{&fakeSetupRunner{}})

	sess, _ := mgr.Create(context.Background(), "", "", "", false)
	waitState(t, mgr, sess.ID, session.StateReady, 3*time.Second)

	if err := mgr.Stop(sess.ID); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	if sess.SessionStatus() != session.StateStopped {
		t.Errorf("state = %s, want Stopped", sess.SessionStatus())
	}
	if handle.stopN.Load() != 1 {
		t.Errorf("VM Stop called %d times, want 1", handle.stopN.Load())
	}
	if snap.deleteN.Load() != 1 {
		t.Errorf("DeleteSnapshot called %d times, want 1", snap.deleteN.Load())
	}
	if mgr.usedSlots[sess.Slot] {
		t.Error("slot should be freed after stop")
	}
}

func TestStop_AlreadyStopped(t *testing.T) {
	snap := &fakeSnapshotter{}
	handle := &fakeVMHandle{}
	launcher := &fakeLauncher{handle: handle}
	mgr := newTestManager(snap, launcher, &fakeBridgeFactory{&fakeSetupRunner{}})

	sess, _ := mgr.Create(context.Background(), "", "", "", false)
	waitState(t, mgr, sess.ID, session.StateReady, 3*time.Second)

	mgr.Stop(sess.ID) //nolint:errcheck — first stop

	// Second stop must be a no-op.
	if err := mgr.Stop(sess.ID); err != nil {
		t.Fatalf("second Stop returned error: %v", err)
	}
	// VM Stop and DeleteSnapshot should still only have been called once.
	if handle.stopN.Load() != 1 {
		t.Errorf("VM Stop called %d times, want 1", handle.stopN.Load())
	}
	if snap.deleteN.Load() != 1 {
		t.Errorf("DeleteSnapshot called %d times, want 1", snap.deleteN.Load())
	}
}

func TestStop_FailedSession(t *testing.T) {
	snap := &fakeSnapshotter{createErr: errors.New("fail")}
	launcher := &fakeLauncher{handle: &fakeVMHandle{}}
	mgr := newTestManager(snap, launcher, &fakeBridgeFactory{&fakeSetupRunner{}})

	sess, _ := mgr.Create(context.Background(), "", "", "", false)
	waitState(t, mgr, sess.ID, session.StateFailed, 3*time.Second)

	// Stop on a failed session should not panic and should clean up.
	if err := mgr.Stop(sess.ID); err != nil {
		t.Fatalf("Stop on failed session: %v", err)
	}
}

func TestStopAll(t *testing.T) {
	snap := &fakeSnapshotter{}
	handle := &fakeVMHandle{}
	launcher := &fakeLauncher{handle: handle}
	mgr := newTestManager(snap, launcher, &fakeBridgeFactory{&fakeSetupRunner{}})

	ids := make([]string, 3)
	for i := range ids {
		sess, err := mgr.Create(context.Background(), "", "", "", false)
		if err != nil {
			t.Fatalf("Create %d: %v", i, err)
		}
		ids[i] = sess.ID
	}

	// Wait for all to reach Ready.
	for _, id := range ids {
		waitState(t, mgr, id, session.StateReady, 5*time.Second)
	}

	mgr.StopAll()

	for _, id := range ids {
		sess, _ := mgr.Get(id)
		if sess.SessionStatus() != session.StateStopped {
			t.Errorf("session %s state = %s, want Stopped", id[:8], sess.SessionStatus())
		}
	}
}
