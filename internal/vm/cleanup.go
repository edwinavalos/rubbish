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

// IsSocketAlive returns true if a Firecracker socket exists at the given slot
// and a live process is holding it.
func IsSocketAlive(slot int) bool {
	sock := SlotSocket(slot)
	if _, err := os.Stat(sock); err != nil {
		return false
	}
	out, err := exec.Command("lsof", "-t", sock).Output()
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(out)) != ""
}

// CleanupOrphans kills stray Firecracker processes and tears down their TAP
// devices. Safe to call at startup even if no orphans exist.
func CleanupOrphans(maxSlots int) error {
	return cleanupOrphansExcept(maxSlots, nil, realCommander{})
}

// CleanupOrphansExcept kills stray Firecracker processes for all slots except
// those in skipSlots, which hold intentionally recovered sessions.
func CleanupOrphansExcept(maxSlots int, skipSlots map[int]bool) error {
	return cleanupOrphansExcept(maxSlots, skipSlots, realCommander{})
}

func cleanupOrphans(maxSlots int, cmd commander) error {
	return cleanupOrphansExcept(maxSlots, nil, cmd)
}

func cleanupOrphansExcept(maxSlots int, skipSlots map[int]bool, cmd commander) error {
	var errs []string

	for slot := 0; slot < maxSlots; slot++ {
		if skipSlots[slot] {
			continue
		}

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

	// Belt-and-suspenders: kill stray FC processes that may still hold a socket
	// on non-skipped slots. Skips slots with no socket file to avoid spurious lsof
	// calls and to avoid killing recovered sessions on live slots.
	for slot := 0; slot < maxSlots; slot++ {
		if skipSlots[slot] {
			continue
		}
		sock := fmt.Sprintf("/tmp/rubbish-fc-%d.sock", slot)
		if _, err := os.Stat(sock); os.IsNotExist(err) {
			continue
		}
		out, err := cmd.command("lsof", "-t", sock).Output()
		if err == nil {
			pidStr := strings.TrimSpace(string(out))
			if pidStr != "" {
				if pid, parseErr := strconv.Atoi(pidStr); parseErr == nil {
					killProcess(pid)
				}
			}
		}
	}

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
