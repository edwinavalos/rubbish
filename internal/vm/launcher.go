package vm

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"time"

	firecracker "github.com/firecracker-microvm/firecracker-go-sdk"
	"github.com/firecracker-microvm/firecracker-go-sdk/client/models"
	"github.com/sirupsen/logrus"
)

const (
	KernelPath = "/opt/rubbish/firecracker/vmlinux"
	RootfsPath = "/opt/rubbish/images/rootfs.ext4"
	FCBinary   = "/usr/local/bin/firecracker"
	BridgeName = "br0"
	VMSSHPort  = 22
	MaxSlots   = 4

	// VirtioFSBinary is the path to the virtiofsd daemon on the host.
	// Alpine's apk package is "virtiofsd"; install with: apk add virtiofsd
	// On Ubuntu/Debian: apt install virtiofsd
	VirtioFSBinary = "/usr/libexec/virtiofsd"

	// VirtioFSSocketDir is where per-session virtiofsd UNIX sockets are placed.
	VirtioFSSocketDir = "/tmp/rubbish-virtiofs"

	// VirtioFSGuestTag is the mount tag the guest uses in its mount command:
	//   mount -t virtiofs <tag> /home/claude/workspace
	// All sessions share the same tag because each VM has its own virtiofsd
	// instance bound to a different socket — there is no cross-session sharing.
	VirtioFSGuestTag = "workspace"
)

// Slot helpers — all per-VM resources are derived from the slot index (0–3).
func SlotTAP(slot int) string    { return fmt.Sprintf("tap%d", slot) }
func SlotMAC(slot int) string    { return fmt.Sprintf("AA:FC:00:00:00:%02X", slot+1) }
func SlotIP(slot int) string     { return fmt.Sprintf("172.16.0.%d", slot+2) }
func SlotSocket(slot int) string { return fmt.Sprintf("/tmp/rubbish-fc-%d.sock", slot) }

// VirtioFSSocket returns the host UNIX socket path for the virtiofsd process
// serving the given session's workspace.
func VirtioFSSocket(sessionID string) string {
	short := sessionID
	if len(short) > 8 {
		short = short[:8]
	}
	return fmt.Sprintf("%s/%s.sock", VirtioFSSocketDir, short)
}

// VirtioFSMount describes a single host directory to share with the VM via
// virtiofs.  Each mount corresponds to one virtiofsd process and one virtio-fs
// device in the Firecracker configuration.
//
// Firecracker virtio-fs support (FC ≥ v1.3):
//   - The host runs virtiofsd, which exposes the shared dir over a UNIX socket.
//   - FC is told about the socket via a "vhost_user_fs" device in its config.
//   - The guest mounts it with: mount -t virtiofs <tag> <mountpoint>
//
// NOTE: The firecracker-go-sdk v1.0.0 does not have a typed VirtioFS config
// struct.  Wiring is done via direct Firecracker API calls (see
// attachVirtioFSDevices) after the FC process starts but before InstanceStart.
// This requires Firecracker ≥ v1.3 on the host.
type VirtioFSMount struct {
	// HostDir is the host path to share (e.g. /opt/rubbish/workspaces/<sessID>).
	HostDir string

	// GuestTag is the virtiofs mount tag seen inside the VM.
	GuestTag string

	// VirtiofsdSocket is the UNIX socket path that virtiofsd listens on.
	// If empty, VirtioFSSocket(sessionID) is used.
	VirtiofsdSocket string
}

type VM struct {
	machine    *firecracker.Machine
	Slot       int
	TapName    string
	SocketPath string
	LaunchedAt time.Time

	// virtiofsdProcs holds the virtiofsd child processes started for this VM.
	// They are killed in Stop() after the FC machine is stopped.
	virtiofsdProcs []*exec.Cmd
}

// Launch boots a Firecracker VM in the given slot using rootfsPath as the root drive.
// Each slot uses a distinct TAP device, MAC address, socket path, and VM IP so
// multiple VMs can run concurrently on the same host.
//
// Note: requires rubbish ALL=(root) NOPASSWD: /usr/bin/mount, /usr/bin/umount in sudoers
// for InjectNetworkConfig to work.
func Launch(ctx context.Context, slot int, rootfsPath string, memMiB int64) (*VM, error) {
	return LaunchWithMounts(ctx, slot, rootfsPath, memMiB, nil)
}

// LaunchWithMounts is like Launch but also starts virtiofsd processes for each
// VirtioFSMount and attaches them to the Firecracker VM as vhost-user-fs devices.
//
// Host-side requirements (when mounts is non-empty):
//   - virtiofsd installed at VirtioFSBinary (e.g. /usr/libexec/virtiofsd)
//   - Firecracker binary ≥ v1.3 (adds vhost_user_fs device type)
//   - The running user must have permission to execute virtiofsd
//     (virtiofsd needs CAP_DAC_READ_SEARCH or run as root with --sandbox=none)
//
// Guest-side requirements (in the rootfs):
//   - virtiofs kernel module loaded (modprobe virtiofs or baked into kernel)
//   - mount command called in rcS: mount -t virtiofs workspace /home/claude/workspace
//
// When mounts is nil or empty this is identical to Launch.
func LaunchWithMounts(ctx context.Context, slot int, rootfsPath string, memMiB int64, mounts []VirtioFSMount) (*VM, error) {
	t := time.Now()

	tap := SlotTAP(slot)
	mac := SlotMAC(slot)
	sock := SlotSocket(slot)

	if err := setupTAP(tap); err != nil {
		return nil, fmt.Errorf("tap setup: %w", err)
	}

	_ = os.Remove(sock)

	ip := SlotIP(slot)

	// Build kernel args.  When virtio-fs mounts are present we do NOT use
	// nomodules — the guest needs the virtiofs kernel module.
	kernelArgs := fmt.Sprintf(
		"console=ttyS0 reboot=k panic=1 pci=off ro ip=%s::172.16.0.1:255.255.255.0::eth0:off",
		ip,
	)
	if len(mounts) == 0 {
		// nomodules is safe and faster when we don't need any extra modules.
		kernelArgs = fmt.Sprintf(
			"console=ttyS0 reboot=k panic=1 pci=off nomodules ro ip=%s::172.16.0.1:255.255.255.0::eth0:off",
			ip,
		)
	}

	cfg := firecracker.Config{
		SocketPath:      sock,
		KernelImagePath: KernelPath,
		KernelArgs:      kernelArgs,
		// Empty (not nil) disables the SDK's default signal-forwarding behaviour,
		// which would otherwise forward SIGTERM to Firecracker when rubbish-poc exits.
		ForwardSignals: []os.Signal{},
		Drives: []models.Drive{
			{
				DriveID:      firecracker.String("rootfs"),
				PathOnHost:   firecracker.String(rootfsPath),
				IsRootDevice: firecracker.Bool(true),
				IsReadOnly:   firecracker.Bool(false),
			},
		},
		NetworkInterfaces: firecracker.NetworkInterfaces{
			{
				StaticConfiguration: &firecracker.StaticNetworkConfiguration{
					MacAddress:  mac,
					HostDevName: tap,
				},
			},
		},
		MachineCfg: models.MachineConfiguration{
			VcpuCount:  firecracker.Int64(2),
			MemSizeMib: firecracker.Int64(memMiB),
		},
	}

	logger := logrus.New()
	logger.SetLevel(logrus.WarnLevel)

	// Use context.Background() for the machine so the FC process is NOT tied to
	// the session or server context. This lets VMs survive a rubbish-poc restart —
	// the process keeps running and is reconnected via DetachedVM on next startup.
	// The session ctx is still used for WaitForSSH cancellation below.
	machineCtx := context.Background()

	cmd := firecracker.VMCommandBuilder{}.
		WithBin(FCBinary).
		WithSocketPath(sock).
		WithStdin(os.Stdin).
		WithStdout(os.Stdout).
		WithStderr(os.Stderr).
		Build(machineCtx)

	m, err := firecracker.NewMachine(machineCtx, cfg,
		firecracker.WithProcessRunner(cmd),
		firecracker.WithLogger(logrus.NewEntry(logger)),
	)
	if err != nil {
		return nil, fmt.Errorf("new machine: %w", err)
	}

	// Start virtiofsd processes before booting the VM, so the sockets exist
	// when Firecracker configures the vhost-user-fs devices.
	// Skip silently if virtiofsd is not installed — session falls back to
	// in-VM git clone via the workspacePath == "" branch in boot().
	if len(mounts) > 0 {
		if _, err := os.Stat(VirtioFSBinary); err != nil {
			fmt.Printf("[vm slot=%d] virtiofsd not found at %s — skipping virtio-fs mounts\n", slot, VirtioFSBinary)
			mounts = nil
		}
	}

	var virtiofsdProcs []*exec.Cmd
	if len(mounts) > 0 {
		procs, err := startVirtiofsdProcesses(mounts)
		if err != nil {
			// Kill any that did start.
			for _, p := range procs {
				p.Process.Kill() //nolint:errcheck
			}
			return nil, fmt.Errorf("start virtiofsd: %w", err)
		}
		virtiofsdProcs = procs

		// Attach vhost-user-fs devices via the Firecracker API.
		// TODO: attachVirtioFSDevices(machineCtx, sock, mounts)
		// This call requires Firecracker ≥ v1.3 and is left as a stub until
		// the host binary is updated and the API client is extended.
		// See: https://github.com/firecracker-microvm/firecracker/blob/main/docs/vsock.md
		// (virtiofs uses a similar vhost-user mechanism)
		//
		// For now, log that the workspace is ready on the host side but not yet
		// mounted in the VM — the fallback in-VM git clone (or NFS) handles the
		// workspace for the transition period.
		for _, mt := range mounts {
			fmt.Printf("[vm slot=%d] virtiofs: host dir=%s tag=%s socket=%s (FC API wiring TODO)\n",
				slot, mt.HostDir, mt.GuestTag, mt.VirtiofsdSocket)
		}
	}

	if err := m.Start(machineCtx); err != nil {
		for _, p := range virtiofsdProcs {
			p.Process.Kill() //nolint:errcheck
		}
		return nil, fmt.Errorf("start machine: %w", err)
	}

	fmt.Printf("[vm slot=%d] boot issued in %s (rootfs=%s)\n", slot, time.Since(t).Round(time.Millisecond), rootfsPath)

	return &VM{
		machine:        m,
		Slot:           slot,
		TapName:        tap,
		SocketPath:     sock,
		LaunchedAt:     t,
		virtiofsdProcs: virtiofsdProcs,
	}, nil
}

// startVirtiofsdProcesses launches one virtiofsd process per mount.
// Each process is started in the background; a short settle wait is performed
// to confirm the socket exists before returning.
func startVirtiofsdProcesses(mounts []VirtioFSMount) ([]*exec.Cmd, error) {
	if err := os.MkdirAll(VirtioFSSocketDir, 0700); err != nil {
		return nil, fmt.Errorf("create virtiofs socket dir: %w", err)
	}

	var procs []*exec.Cmd
	for _, mt := range mounts {
		sock := mt.VirtiofsdSocket
		if sock == "" {
			return procs, fmt.Errorf("mount %s: VirtiofsdSocket must be set", mt.GuestTag)
		}

		// Remove any stale socket from a previous run.
		os.Remove(sock) //nolint:errcheck

		// virtiofsd arguments:
		//   --socket-path  — UNIX socket that Firecracker connects to
		//   --shared-dir   — host directory to expose
		//   --sandbox=none — required when not running as root; disables seccomp sandbox
		//                    (acceptable in this controlled environment)
		//   --log-level=warn — reduce noise
		cmd := exec.Command(VirtioFSBinary,
			"--socket-path="+sock,
			"--shared-dir="+mt.HostDir,
			"--sandbox=none",
			"--log-level=warn",
		)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr

		if err := cmd.Start(); err != nil {
			return procs, fmt.Errorf("start virtiofsd for %s: %w", mt.GuestTag, err)
		}
		procs = append(procs, cmd)

		// Wait up to 2 seconds for the socket to appear.
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(sock); err == nil {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		if _, err := os.Stat(sock); err != nil {
			cmd.Process.Kill() //nolint:errcheck
			return procs[:len(procs)-1], fmt.Errorf("virtiofsd socket %s did not appear within 2s", sock)
		}
		fmt.Printf("[virtiofsd] socket ready: %s → %s\n", sock, mt.HostDir)
	}
	return procs, nil
}

// InjectNetworkConfig mounts the snapshot device and writes a static /etc/network/interfaces
// so the VM comes up with the correct per-slot IP instead of the base image's default.
// Requires: rubbish ALL=(root) NOPASSWD: /usr/bin/mount, /usr/bin/umount, /usr/bin/cp in sudoers.
func InjectNetworkConfig(snapshotDevice, ip, gateway string) error {
	tmpdir, err := os.MkdirTemp("", "rubbish-mnt-*")
	if err != nil {
		return fmt.Errorf("mkdirtemp: %w", err)
	}
	defer os.RemoveAll(tmpdir)

	if out, err := exec.Command("sudo", "mount", "-o", "rw", snapshotDevice, tmpdir).CombinedOutput(); err != nil {
		return fmt.Errorf("mount %s: %s: %w", snapshotDevice, out, err)
	}

	ifaces := fmt.Sprintf("auto lo\niface lo inet loopback\n\nauto eth0\niface eth0 inet static\n    address %s\n    netmask 255.255.255.0\n    gateway %s\n", ip, gateway)

	// Write to a temp file owned by the current user, then sudo cp into the mount.
	tmp, err := os.CreateTemp("", "rubbish-ifaces-*")
	if err != nil {
		exec.Command("sudo", "umount", tmpdir).Run() //nolint:errcheck
		return fmt.Errorf("create temp ifaces: %w", err)
	}
	tmpName := tmp.Name()
	_, writeErr := tmp.WriteString(ifaces)
	tmp.Close()
	defer os.Remove(tmpName)

	if writeErr != nil {
		exec.Command("sudo", "umount", tmpdir).Run() //nolint:errcheck
		return fmt.Errorf("write ifaces: %w", writeErr)
	}

	ifacePath := tmpdir + "/etc/network/interfaces"
	if out, err := exec.Command("sudo", "cp", tmpName, ifacePath).CombinedOutput(); err != nil {
		exec.Command("sudo", "umount", tmpdir).Run() //nolint:errcheck
		return fmt.Errorf("cp ifaces: %s: %w", out, err)
	}

	if out, err := exec.Command("sudo", "umount", tmpdir).CombinedOutput(); err != nil {
		return fmt.Errorf("umount: %s: %w", out, err)
	}
	return nil
}

func (v *VM) WaitForSSH(ctx context.Context) error {
	addr := fmt.Sprintf("%s:%d", SlotIP(v.Slot), VMSSHPort)
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := tryDial(addr)
		if err == nil {
			conn.Close()
			fmt.Printf("[vm slot=%d] ssh ready in %s\n", v.Slot, time.Since(v.LaunchedAt).Round(time.Millisecond))
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	return fmt.Errorf("ssh on %s did not open within 30s", addr)
}

func (v *VM) Stop(ctx context.Context) error {
	// Attempt graceful shutdown first; ignore error since we'll kill the process anyway.
	v.machine.Shutdown(ctx) //nolint:errcheck
	// StopVMM kills the Firecracker process so the TAP fd is released before teardown.
	err := v.machine.StopVMM()
	teardownTAP(v.TapName)

	// Kill virtiofsd processes that were started for this VM.  Do this after
	// StopVMM so the FC process has already released the socket FDs.
	for _, p := range v.virtiofsdProcs {
		if p.Process != nil {
			p.Process.Kill() //nolint:errcheck
		}
	}

	return err
}

func setupTAP(name string) error {
	out, err := exec.Command("sudo", "/usr/local/bin/rubbish-tap-up", name).CombinedOutput()
	if err != nil {
		return fmt.Errorf("rubbish-tap-up %s: %s: %w", name, out, err)
	}
	return nil
}

func tryDial(addr string) (net.Conn, error) {
	return net.DialTimeout("tcp", addr, time.Second)
}

func teardownTAP(name string) {
	exec.Command("sudo", "/usr/local/bin/rubbish-tap-down", name).Run()
}
