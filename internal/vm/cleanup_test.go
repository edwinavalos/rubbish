package vm

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// fakeCommander records calls and can return canned output.
type fakeCommander struct {
	calls  []string
	output map[string]string // key = "cmd arg0 arg1...", value = stdout
}

func (f *fakeCommander) command(name string, arg ...string) *exec.Cmd {
	key := strings.Join(append([]string{name}, arg...), " ")
	f.calls = append(f.calls, key)
	if out, ok := f.output[key]; ok {
		// Return a cmd that echoes the canned output.
		return exec.Command("echo", "-n", out)
	}
	// Return a cmd that exits 0 with no output.
	return exec.Command("true")
}

func (f *fakeCommander) called(prefix string) bool {
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

func TestCleanupOrphans_OccupiedSlot(t *testing.T) {
	// Create a temp socket file to simulate an occupied slot.
	dir := t.TempDir()
	sock := filepath.Join(dir, "rubbish-fc-0.sock")
	if err := os.WriteFile(sock, []byte{}, 0600); err != nil {
		t.Fatal(err)
	}

	// Patch SlotSocket for slot 0 by temporarily overriding via the test helper.
	// We can't monkey-patch SlotSocket, so instead we test cleanupOrphans with
	// a real socket file matching the real SlotSocket path by creating it.
	realSock := SlotSocket(0)
	realDir := filepath.Dir(realSock)
	if err := os.MkdirAll(realDir, 0755); err != nil {
		t.Fatalf("mkdirall: %v", err)
	}
	if err := os.WriteFile(realSock, []byte{}, 0600); err != nil {
		t.Fatalf("write socket: %v", err)
	}
	defer os.Remove(realSock)

	fc := &fakeCommander{
		output: map[string]string{
			"lsof -t " + realSock: "99999",
		},
	}

	// cleanupOrphans will try to kill PID 99999 (doesn't exist — killProcess handles gracefully)
	// and call teardownTAP (which runs a real shell command that may fail — that's OK).
	// We just verify lsof was called and the socket is cleaned up.
	cleanupOrphans(1, fc) //nolint:errcheck

	if !fc.called("lsof -t") {
		t.Error("expected lsof to be called for occupied slot")
	}
	if _, err := os.Stat(realSock); !os.IsNotExist(err) {
		t.Error("expected socket file to be removed after cleanup")
	}
}

func TestCleanupOrphans_EmptySlot(t *testing.T) {
	// Ensure socket does not exist.
	sock := SlotSocket(0)
	os.Remove(sock)

	fc := &fakeCommander{}
	cleanupOrphans(1, fc) //nolint:errcheck

	for _, c := range fc.calls {
		if strings.HasPrefix(c, "lsof") {
			t.Errorf("expected no lsof call for empty slot, got: %s", c)
		}
	}
}

func TestCleanupOrphans_NoOrphans(t *testing.T) {
	// Make sure none of the slot sockets exist.
	for i := 0; i < MaxSlots; i++ {
		os.Remove(SlotSocket(i))
	}

	fc := &fakeCommander{}
	err := cleanupOrphans(MaxSlots, fc)
	if err != nil {
		t.Errorf("expected nil error with no orphans, got: %v", err)
	}
	for _, c := range fc.calls {
		if strings.HasPrefix(c, "lsof") {
			t.Errorf("unexpected lsof call with no orphans: %s", c)
		}
	}
}

func TestCleanupOrphans_PartialFailure_ContinuesOtherSlots(t *testing.T) {
	// Create sockets for slots 0 and 1.
	for _, slot := range []int{0, 1} {
		sock := SlotSocket(slot)
		os.MkdirAll(filepath.Dir(sock), 0755) //nolint:errcheck
		os.WriteFile(sock, []byte{}, 0600)    //nolint:errcheck
	}
	defer func() {
		os.Remove(SlotSocket(0))
		os.Remove(SlotSocket(1))
	}()

	fc := &fakeCommander{
		// lsof returns no PID for either slot — that's fine, just tests continuation.
		output: map[string]string{},
	}

	// Even if slot 0 teardown has issues, slot 1 should still be processed.
	cleanupOrphans(2, fc) //nolint:errcheck

	// Both sockets should be removed (or attempted).
	lsofCalls := 0
	for _, c := range fc.calls {
		if strings.HasPrefix(c, "lsof") {
			lsofCalls++
		}
	}
	if lsofCalls != 2 {
		t.Errorf("expected lsof called for both slots, got %d calls", lsofCalls)
	}
}
