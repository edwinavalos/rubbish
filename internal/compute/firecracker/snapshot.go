package firecracker

import (
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// deviceUID is the UID that owns created snapshot devices.
// Resolved once at startup to avoid repeated syscalls.
var deviceUID = strconv.Itoa(os.Getuid())

// dmRunner abstracts dmsetup/blockdev calls for testability.
type dmRunner interface {
	run(name string, arg ...string) ([]byte, error)
}

type realDMRunner struct{}

func (realDMRunner) run(name string, arg ...string) ([]byte, error) {
	return exec.Command(name, arg...).CombinedOutput()
}

type SnapshotManager struct {
	poolDevice   string
	baseVolumeID int
	idPath       string
	dm           dmRunner
	mu           sync.Mutex
	nextVolumeID int
	sessions     map[string]int // sessionID → volumeID
}

func NewSnapshotManager(poolDevice, idPath string) (*SnapshotManager, error) {
	return newSnapshotManager(poolDevice, idPath, realDMRunner{})
}

func newSnapshotManager(poolDevice, idPath string, dm dmRunner) (*SnapshotManager, error) {
	if _, err := os.Stat(poolDevice); err != nil {
		return nil, fmt.Errorf("pool device %s not found: %w", poolDevice, err)
	}

	next := 2
	if data, err := os.ReadFile(idPath); err == nil {
		n, err := strconv.Atoi(strings.TrimSpace(string(data)))
		if err == nil && n >= 2 {
			next = n
		}
	}

	sm := &SnapshotManager{
		poolDevice:   poolDevice,
		baseVolumeID: -1,
		idPath:       idPath,
		dm:           dm,
		nextVolumeID: next,
		sessions:     make(map[string]int),
	}

	if err := sm.discoverState(); err != nil {
		return nil, err
	}
	if sm.baseVolumeID < 0 {
		return nil, fmt.Errorf("rubbish-base-ro not found — run setup-storage.sh to import the base image")
	}
	return sm, nil
}

// discoverState reads rubbish-base's thin vol ID and rebuilds the sessions map
// from any existing rubbish-session-* dm devices. Both are best-effort on
// individual device reads; only the base vol ID is fatal.
func (s *SnapshotManager) discoverState() error {
	// Discover base vol ID from the live device table.
	out, err := s.dm.run("sudo", "dmsetup", "table", "rubbish-base-ro")
	if err != nil {
		return fmt.Errorf("dmsetup table rubbish-base: %s: %w", out, err)
	}
	// Table format: "0 <sectors> thin <pool-dev> <vol-id>"
	fields := strings.Fields(string(out))
	if len(fields) < 5 || fields[2] != "thin" {
		return fmt.Errorf("unexpected rubbish-base-ro table: %q", strings.TrimSpace(string(out)))
	}
	baseVolID, err := strconv.Atoi(fields[4])
	if err != nil {
		return fmt.Errorf("parse base vol ID from %q: %w", fields[4], err)
	}
	s.baseVolumeID = baseVolID

	// Rebuild sessions map from existing rubbish-session-* devices so that
	// DeleteSnapshot works correctly after a service restart.
	lsOut, err := s.dm.run("sudo", "dmsetup", "ls")
	if err != nil {
		return nil // best-effort; missing sessions just means they can't be deleted
	}
	for _, line := range strings.Split(string(lsOut), "\n") {
		devName := strings.Fields(line)
		if len(devName) == 0 || !strings.HasPrefix(devName[0], "rubbish-session-") {
			continue
		}
		name := devName[0]
		sessionID := strings.TrimPrefix(name, "rubbish-session-")
		tbl, err := s.dm.run("sudo", "dmsetup", "table", name)
		if err != nil {
			continue
		}
		tFields := strings.Fields(string(tbl))
		if len(tFields) < 5 {
			continue
		}
		volID, err := strconv.Atoi(tFields[4])
		if err != nil {
			continue
		}
		s.sessions[sessionID] = volID
	}

	// Reconcile nextVolumeID against discovered sessions so that a counter
	// file that fell behind (e.g. after manual pool surgery) never causes a
	// create_snap collision with an existing thin volume.
	for _, volID := range s.sessions {
		if volID >= s.nextVolumeID {
			s.nextVolumeID = volID + 1
			if err := os.WriteFile(s.idPath, []byte(strconv.Itoa(s.nextVolumeID)), 0644); err != nil {
				slog.Warn("snapshot: persist reconciled next-volume-id", "next_volume_id", s.nextVolumeID, "err", err)
			} else {
				slog.Info("snapshot: reconciled next-volume-id", "next_volume_id", s.nextVolumeID)
			}
		}
	}

	return nil
}

func (s *SnapshotManager) CreateSnapshot(sessionID string) (string, error) {
	s.mu.Lock()
	volID := s.nextVolumeID
	s.nextVolumeID++
	if err := os.WriteFile(s.idPath, []byte(strconv.Itoa(s.nextVolumeID)), 0644); err != nil {
		s.nextVolumeID--
		s.mu.Unlock()
		return "", fmt.Errorf("persist next-volume-id: %w", err)
	}
	s.mu.Unlock()

	snapMsg := fmt.Sprintf("create_snap %d %d", volID, s.baseVolumeID)
	if out, err := s.dm.run("sudo", "dmsetup", "message", s.poolDevice, "0", snapMsg); err != nil {
		return "", fmt.Errorf("dmsetup create_snap (vol %d): %s: %w", volID, out, err)
	}

	sectors, err := s.BaseSize()
	if err != nil {
		s.dm.run("sudo", "dmsetup", "message", s.poolDevice, "0", fmt.Sprintf("delete %d", volID)) //nolint:errcheck
		return "", fmt.Errorf("base size: %w", err)
	}

	devName := "rubbish-session-" + sessionID
	// Remove any stale device with the same name (can linger after a rapid stop+restart
	// because firecracker briefly holds an fd to the device after SIGTERM).
	s.dm.run("sudo", "dmsetup", "remove", devName) //nolint:errcheck
	table := fmt.Sprintf("0 %d thin %s %d", sectors, s.poolDevice, volID)
	if out, err := s.dm.run("sudo", "dmsetup", "create", devName, "--uid", deviceUID, "--table", table); err != nil {
		s.dm.run("sudo", "dmsetup", "message", s.poolDevice, "0", fmt.Sprintf("delete %d", volID)) //nolint:errcheck
		return "", fmt.Errorf("dmsetup create %s: %s: %w", devName, out, err)
	}

	s.mu.Lock()
	s.sessions[sessionID] = volID
	s.mu.Unlock()

	return "/dev/mapper/" + devName, nil
}

func (s *SnapshotManager) DeleteSnapshot(sessionID string) error {
	s.mu.Lock()
	volID, ok := s.sessions[sessionID]
	s.mu.Unlock()
	if !ok {
		return fmt.Errorf("no snapshot for session %s", sessionID)
	}

	devName := "rubbish-session-" + sessionID
	var errs []string

	if out, err := s.dm.run("sudo", "dmsetup", "remove", devName); err != nil {
		// The FC process may still briefly hold an fd to the device after SIGTERM.
		// Wait 500ms and retry with --force before giving up.
		time.Sleep(500 * time.Millisecond)
		if out2, err2 := s.dm.run("sudo", "dmsetup", "remove", "--force", devName); err2 != nil {
			errs = append(errs, fmt.Sprintf("dmsetup remove %s: %s / force: %s: %v", devName, out, out2, err2))
		}
	}

	delMsg := fmt.Sprintf("delete %d", volID)
	if out, err := s.dm.run("sudo", "dmsetup", "message", s.poolDevice, "0", delMsg); err != nil {
		errs = append(errs, fmt.Sprintf("dmsetup delete vol %d: %s: %v", volID, out, err))
	}

	s.mu.Lock()
	delete(s.sessions, sessionID)
	s.mu.Unlock()

	if len(errs) > 0 {
		return fmt.Errorf("delete snapshot %s: %s", sessionID, strings.Join(errs, "; "))
	}
	return nil
}

func (s *SnapshotManager) BaseSize() (int64, error) {
	out, err := s.dm.run("sudo", "blockdev", "--getsz", "/dev/mapper/rubbish-base-ro")
	if err != nil {
		return 0, fmt.Errorf("blockdev --getsz: %s: %w", out, err)
	}
	sectors, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse sector count %q: %w", strings.TrimSpace(string(out)), err)
	}
	return sectors, nil
}
