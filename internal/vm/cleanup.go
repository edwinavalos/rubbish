package vm

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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

func cleanupOrphansExcept(maxSlots int, skipSlots map[int]bool, cmd commander) error {
	slotsToCheck := slotsForCleanup(maxSlots)

	var errs []string

	for _, slot := range slotsToCheck {
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

	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

// slotsForCleanup returns the slot indices to scan. When maxSlots is 0
// (unlimited), it discovers slots by globbing existing socket files.
func slotsForCleanup(maxSlots int) []int {
	if maxSlots > 0 {
		slots := make([]int, maxSlots)
		for i := range slots {
			slots[i] = i
		}
		return slots
	}
	// Unlimited: discover slots from existing socket files.
	matches, _ := filepath.Glob("/tmp/rubbish-fc-*.sock")
	var slots []int
	for _, match := range matches {
		var slot int
		if _, err := fmt.Sscanf(filepath.Base(match), "rubbish-fc-%d.sock", &slot); err == nil {
			slots = append(slots, slot)
		}
	}
	return slots
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
