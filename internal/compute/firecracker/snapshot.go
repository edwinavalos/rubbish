package firecracker

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
)

// deviceUID is the UID that owns created snapshot devices.
// Resolved once at startup to avoid repeated syscalls.
var deviceUID = strconv.Itoa(os.Getuid())

type SnapshotManager struct {
	poolDevice   string
	baseVolumeID int
	idPath       string
	mu           sync.Mutex
	nextVolumeID int
	sessions     map[string]int // sessionID → volumeID
}

func NewSnapshotManager(poolDevice, idPath string) (*SnapshotManager, error) {
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

	return &SnapshotManager{
		poolDevice:   poolDevice,
		baseVolumeID: 1,
		idPath:       idPath,
		nextVolumeID: next,
		sessions:     make(map[string]int),
	}, nil
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
	if out, err := exec.Command("sudo", "dmsetup", "message", s.poolDevice, "0", snapMsg).CombinedOutput(); err != nil {
		return "", fmt.Errorf("dmsetup create_snap (vol %d): %s: %w", volID, out, err)
	}

	sectors, err := s.BaseSize()
	if err != nil {
		exec.Command("sudo", "dmsetup", "message", s.poolDevice, "0", fmt.Sprintf("delete %d", volID)).Run() //nolint:errcheck
		return "", fmt.Errorf("base size: %w", err)
	}

	devName := "rubbish-session-" + sessionID
	// Remove any stale device with the same name (can linger after a rapid stop+restart
	// because firecracker briefly holds an fd to the device after SIGTERM).
	exec.Command("sudo", "dmsetup", "remove", devName).Run() //nolint:errcheck
	table := fmt.Sprintf("0 %d thin %s %d", sectors, s.poolDevice, volID)
	if out, err := exec.Command("sudo", "dmsetup", "create", devName, "--uid", deviceUID, "--table", table).CombinedOutput(); err != nil {
		exec.Command("sudo", "dmsetup", "message", s.poolDevice, "0", fmt.Sprintf("delete %d", volID)).Run() //nolint:errcheck
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

	if out, err := exec.Command("sudo", "dmsetup", "remove", devName).CombinedOutput(); err != nil {
		errs = append(errs, fmt.Sprintf("dmsetup remove %s: %s: %v", devName, out, err))
	}

	delMsg := fmt.Sprintf("delete %d", volID)
	if out, err := exec.Command("sudo", "dmsetup", "message", s.poolDevice, "0", delMsg).CombinedOutput(); err != nil {
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
	out, err := exec.Command("sudo", "blockdev", "--getsz", "/dev/mapper/rubbish-base-ro").CombinedOutput()
	if err != nil {
		return 0, fmt.Errorf("blockdev --getsz: %s: %w", out, err)
	}
	sectors, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse sector count %q: %w", strings.TrimSpace(string(out)), err)
	}
	return sectors, nil
}
