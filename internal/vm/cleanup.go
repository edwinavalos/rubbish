package vm

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// commander is an interface around exec.Command for testability.
type commander interface {
	command(name string, arg ...string) *exec.Cmd
}

type realCommander struct{}

func (realCommander) command(name string, arg ...string) *exec.Cmd {
	return exec.Command(name, arg...)
}

// CleanupOrphans kills stray Firecracker processes and tears down their TAP
// devices. Safe to call at startup even if no orphans exist.
func CleanupOrphans(maxSlots int) error {
	return cleanupOrphans(maxSlots, realCommander{})
}

func cleanupOrphans(maxSlots int, cmd commander) error {
	var errs []string

	for slot := 0; slot < maxSlots; slot++ {
		sock := SlotSocket(slot)
		tap := SlotTAP(slot)

		if _, err := os.Stat(sock); os.IsNotExist(err) {
			continue
		}

		// Find PID holding the socket.
		out, err := cmd.command("lsof", "-t", sock).Output()
		if err == nil {
			pidStr := strings.TrimSpace(string(out))
			if pidStr != "" {
				pid, parseErr := strconv.Atoi(pidStr)
				if parseErr == nil {
					killProcess(pid)
				}
			}
		}

		// Tear down TAP (calls teardownTAP which runs rubbish-tap-down).
		teardownTAP(tap)

		if removeErr := os.Remove(sock); removeErr != nil && !os.IsNotExist(removeErr) {
			errs = append(errs, fmt.Sprintf("slot %d remove socket: %v", slot, removeErr))
		}
	}

	// Belt-and-suspenders: kill any stray FC processes not caught above.
	cmd.command("pkill", "-f", "firecracker --api-sock /tmp/rubbish-fc").Run() //nolint:errcheck

	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

// killProcess sends SIGTERM, waits up to 2 seconds, then SIGKILLs.
func killProcess(pid int) {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return
	}
	proc.Signal(syscall.SIGTERM) //nolint:errcheck
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if err := proc.Signal(syscall.Signal(0)); err != nil {
			return // process is gone
		}
		time.Sleep(100 * time.Millisecond)
	}
	proc.Signal(syscall.SIGKILL) //nolint:errcheck
}
