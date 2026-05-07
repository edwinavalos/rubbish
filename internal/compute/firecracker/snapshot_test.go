package firecracker

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// fakeDM records calls and returns canned output per command key.
type fakeDM struct {
	calls  []string
	output map[string]string
	errors map[string]error
}

func (f *fakeDM) run(name string, arg ...string) ([]byte, error) {
	key := strings.Join(append([]string{name}, arg...), " ")
	f.calls = append(f.calls, key)
	return []byte(f.output[key]), f.errors[key]
}

func (f *fakeDM) called(prefix string) bool {
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

func (f *fakeDM) calledContaining(substr string) bool {
	for _, c := range f.calls {
		if strings.Contains(c, substr) {
			return true
		}
	}
	return false
}

// newFakeDM returns a fakeDM pre-loaded with the standard rubbish-base table
// (thin vol baseVolID) and an empty ls output (no existing session devices).
func newFakeDM(baseVolID int) *fakeDM {
	pool := "/dev/mapper/rubbish-pool"
	return &fakeDM{
		output: map[string]string{
			"sudo dmsetup table rubbish-base":                "0 8388608 thin " + pool + " " + strconv.Itoa(baseVolID),
			"sudo dmsetup ls":                                "",
			"sudo blockdev --getsz /dev/mapper/rubbish-base": "8388608",
		},
		errors: map[string]error{},
	}
}

// writeIDFile writes the next-volume-id file to a temp dir and returns the path.
func writeIDFile(t *testing.T, next int) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "next-volume-id")
	if err := os.WriteFile(p, []byte(strconv.Itoa(next)), 0644); err != nil {
		t.Fatal(err)
	}
	return p
}

// poolDevice creates a dummy pool device file (stat check only).
func poolDevice(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "rubbish-pool")
	if err := os.WriteFile(p, nil, 0644); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestDiscoverBaseVolID verifies that NewSnapshotManager reads the base vol ID
// from the live dmsetup table instead of using a hardcoded value.
func TestDiscoverBaseVolID(t *testing.T) {
	for _, baseVol := range []int{0, 1, 3} {
		t.Run(fmt.Sprintf("baseVol=%d", baseVol), func(t *testing.T) {
			dm := newFakeDM(baseVol)
			sm, err := newSnapshotManager(poolDevice(t), writeIDFile(t, 2), dm)
			if err != nil {
				t.Fatalf("newSnapshotManager: %v", err)
			}
			if sm.baseVolumeID != baseVol {
				t.Errorf("baseVolumeID = %d, want %d", sm.baseVolumeID, baseVol)
			}
		})
	}
}

// TestCreateSnapshot_UsesDiscoveredBase verifies that create_snap messages use
// the discovered base vol ID, not a hardcoded one.
func TestCreateSnapshot_UsesDiscoveredBase(t *testing.T) {
	dm := newFakeDM(0)
	sm, err := newSnapshotManager(poolDevice(t), writeIDFile(t, 10), dm)
	if err != nil {
		t.Fatalf("setup: %v", err)
	}

	sm.CreateSnapshot("sess-a") //nolint:errcheck

	// The pool device path varies (temp dir in tests), so check the essential parts.
	if !dm.calledContaining("create_snap 10 0") {
		t.Errorf("expected create_snap message with vol 10 snapping base vol 0; calls were:\n%s",
			strings.Join(dm.calls, "\n"))
	}
}

// TestDeleteSnapshot_AfterRestart reproduces the bug: before the fix, a new
// SnapshotManager had an empty sessions map and DeleteSnapshot would silently
// fail for sessions that existed before the restart, leaking dm devices and
// thin volumes on the host.
func TestDeleteSnapshot_AfterRestart(t *testing.T) {
	pool := poolDevice(t)
	idPath := writeIDFile(t, 10)

	// First manager: create a snapshot.
	dm1 := newFakeDM(0)
	sm1, err := newSnapshotManager(pool, idPath, dm1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sm1.CreateSnapshot("sess-abc"); err != nil {
		t.Fatalf("CreateSnapshot: %v", err)
	}

	// Simulate a service restart: new manager, same pool/idPath, BUT the dm
	// device rubbish-session-sess-abc still exists on the host.
	dm2 := newFakeDM(0)
	// Add the existing session device to the ls output so discoverState can
	// rebuild the sessions map.
	dm2.output["sudo dmsetup ls"] = "rubbish-session-sess-abc\t(252:3)"
	dm2.output["sudo dmsetup table rubbish-session-sess-abc"] = "0 8388608 thin /dev/mapper/rubbish-pool 10"

	sm2, err := newSnapshotManager(pool, idPath, dm2)
	if err != nil {
		t.Fatal(err)
	}

	// After restart, DeleteSnapshot must succeed and issue the remove + delete
	// commands — it must NOT silently skip cleanup.
	if err := sm2.DeleteSnapshot("sess-abc"); err != nil {
		t.Fatalf("DeleteSnapshot after restart: %v", err)
	}

	if !dm2.called("sudo dmsetup remove rubbish-session-sess-abc") {
		t.Error("expected dmsetup remove to be called for recovered session")
	}
	if !dm2.calledContaining("delete 10") {
		t.Error("expected thin vol delete message to be called for recovered session")
	}
}

// TestDeleteSnapshot_UnknownSession verifies that DeleteSnapshot returns an
// error for a session that was never created (not just silently no-ops).
func TestDeleteSnapshot_UnknownSession(t *testing.T) {
	dm := newFakeDM(0)
	sm, err := newSnapshotManager(poolDevice(t), writeIDFile(t, 2), dm)
	if err != nil {
		t.Fatal(err)
	}
	if err := sm.DeleteSnapshot("nonexistent"); err == nil {
		t.Error("expected error for unknown session, got nil")
	}
}

// TestNewSnapshotManager_BadBase verifies that initialization fails cleanly
// when the rubbish-base device is missing or has an unexpected table format.
func TestNewSnapshotManager_BadBase(t *testing.T) {
	dm := &fakeDM{
		output: map[string]string{
			"sudo dmsetup ls": "",
		},
		errors: map[string]error{
			"sudo dmsetup table rubbish-base": fmt.Errorf("no such device"),
		},
	}
	_, err := newSnapshotManager(poolDevice(t), writeIDFile(t, 2), dm)
	if err == nil {
		t.Error("expected error when rubbish-base is missing, got nil")
	}
}
