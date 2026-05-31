package vm

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"strconv"
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
)

// Slot helpers — all per-VM resources are derived from the slot index (0–3).
func SlotTAP(slot int) string    { return fmt.Sprintf("tap%d", slot) }
func SlotMAC(slot int) string    { return fmt.Sprintf("AA:FC:00:00:00:%02X", slot+1) }
func SlotIP(slot int) string     { return fmt.Sprintf("172.16.0.%d", slot+2) }
func SlotSocket(slot int) string { return fmt.Sprintf("/tmp/rubbish-fc-%d.sock", slot) }

type VM struct {
	machine    *firecracker.Machine
	Slot       int
	TapName    string
	SocketPath string
	LaunchedAt time.Time
}

// Launch boots a Firecracker VM in the given slot using rootfsPath as the root drive.
// Each slot uses a distinct TAP device, MAC address, socket path, and VM IP so
// multiple VMs can run concurrently on the same host.
func Launch(ctx context.Context, slot int, rootfsPath string, memMiB int64) (*VM, error) {
	t := time.Now()

	tap := SlotTAP(slot)
	mac := SlotMAC(slot)
	sock := SlotSocket(slot)

	if err := setupTAP(tap); err != nil {
		return nil, fmt.Errorf("tap setup: %w", err)
	}

	_ = os.Remove(sock)

	ip := SlotIP(slot)

	kernelArgs := fmt.Sprintf(
		"console=ttyS0 reboot=k panic=1 pci=off nomodules ro ip=%s::172.16.0.1:255.255.255.0::eth0:off",
		ip,
	)

	cfg := firecracker.Config{
		SocketPath:      sock,
		KernelImagePath: KernelPath,
		KernelArgs:      kernelArgs,
		ForwardSignals:  []os.Signal{},
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
	logger.SetOutput(io.Discard)

	// Use context.Background() so the FC process is NOT tied to the session or
	// server context — VMs survive a rubbish-poc restart and are reconnected via
	// DetachedVM on next startup.
	machineCtx := context.Background()

	cmd := firecracker.VMCommandBuilder{}.
		WithBin(FCBinary).
		WithSocketPath(sock).
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

	if err := m.Start(machineCtx); err != nil {
		return nil, fmt.Errorf("start machine: %w", err)
	}

	slog.Info("vm boot issued", "slot", slot, "duration", time.Since(t).Round(time.Millisecond), "rootfs", rootfsPath)

	return &VM{
		machine:    m,
		Slot:       slot,
		TapName:    tap,
		SocketPath: sock,
		LaunchedAt: t,
	}, nil
}

// InjectNetworkConfig mounts the snapshot device and writes a static /etc/network/interfaces
// so the VM comes up with the correct per-slot IP instead of the base image's default.
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
	addr := net.JoinHostPort(SlotIP(v.Slot), strconv.Itoa(VMSSHPort))
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := tryDial(addr)
		if err == nil {
			conn.Close()
			slog.Info("vm ssh ready", "slot", v.Slot, "duration", time.Since(v.LaunchedAt).Round(time.Millisecond))
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	return fmt.Errorf("ssh on %s did not open within 60s", addr)
}

// WaitForVMDead blocks until port 22 at ip stops accepting connections, or
// until 5 seconds elapse. Called after Stop() to ensure the slot is not
// reused while the dying VM's SSH is still answering.
func WaitForVMDead(ip string) {
	addr := net.JoinHostPort(ip, strconv.Itoa(VMSSHPort))
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err != nil {
			return
		}
		conn.Close()
		time.Sleep(100 * time.Millisecond)
	}
	slog.Warn("vm still reachable after stop", "addr", addr)
}

func (v *VM) Stop(ctx context.Context) error {
	v.machine.Shutdown(ctx) //nolint:errcheck
	err := v.machine.StopVMM()
	teardownTAP(v.TapName)
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
