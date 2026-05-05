package vm

import (
	"context"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// DetachedVM wraps a Firecracker process that was started in a previous server
// run. It can be stopped but WaitForSSH is a no-op (the VM is already running).
type DetachedVM struct {
	Slot       int
	socketPath string
	tapName    string
}

// NewDetachedVM returns a DetachedVM for the given slot.
func NewDetachedVM(slot int) *DetachedVM {
	return &DetachedVM{
		Slot:       slot,
		socketPath: SlotSocket(slot),
		tapName:    SlotTAP(slot),
	}
}

// WaitForSSH is a no-op: the VM was already running before this process started.
func (d *DetachedVM) WaitForSSH(_ context.Context) error { return nil }

// Stop kills the Firecracker process and tears down networking, mirroring what
// CleanupOrphans does for unknown processes.
func (d *DetachedVM) Stop(_ context.Context) error {
	out, err := exec.Command("lsof", "-t", d.socketPath).Output()
	if err == nil {
		if pid, err := strconv.Atoi(strings.TrimSpace(string(out))); err == nil {
			killProcess(pid)
		}
	}
	teardownTAP(d.tapName)
	os.Remove(d.socketPath) //nolint:errcheck
	return nil
}
