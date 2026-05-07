//go:build integration

package terminal_test

import (
	"bytes"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/edwinavalos/rubbish/internal/terminal"
	"golang.org/x/crypto/ssh"
)

// Run with: go test -tags integration ./internal/terminal/ -v
//
// Required env vars:
//   RUBBISH_SSH_KEY  path to the rubbish private key (default: /opt/rubbish/ssh/id_ed25519)
//   RUBBISH_VM_ADDR  VM IP:port                      (default: 172.16.0.2:22)
//   RUBBISH_HOST_IP  host IP reachable from inside the VM (default: 172.16.0.1)

func sshKey(t *testing.T) ssh.Signer {
	t.Helper()
	path := os.Getenv("RUBBISH_SSH_KEY")
	if path == "" {
		path = "/opt/rubbish/ssh/id_ed25519"
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read SSH key %s: %v", path, err)
	}
	signer, err := ssh.ParsePrivateKey(b)
	if err != nil {
		t.Fatalf("parse SSH key: %v", err)
	}
	return signer
}

func vmAddr() string {
	if v := os.Getenv("RUBBISH_VM_ADDR"); v != "" {
		return v
	}
	return "172.16.0.2:22"
}

func hostIP() string {
	if v := os.Getenv("RUBBISH_HOST_IP"); v != "" {
		return v
	}
	return "172.16.0.1"
}

// runOne opens a single SSH session, runs cmd, returns combined stdout+stderr.
func runOne(t *testing.T, addr, user string, signer ssh.Signer, cmd string) string {
	t.Helper()
	b := terminal.NewBridge(addr, user, signer)
	var buf bytes.Buffer
	if err := b.RunSetupCapture([]string{cmd}, &buf); err != nil {
		t.Fatalf("SSH %s@%s %q: %v", user, addr, cmd, err)
	}
	return strings.TrimSpace(buf.String())
}

// TestSSHHostToVMRoot verifies the poc service can reach the VM as root.
func TestSSHHostToVMRoot(t *testing.T) {
	signer := sshKey(t)
	out := runOne(t, vmAddr(), "root", signer, "id")
	if !strings.Contains(out, "uid=0(root)") {
		t.Errorf("expected root uid, got: %q", out)
	}
	t.Logf("root@VM: %s", out)
}

// TestSSHHostToVMClaude verifies the terminal service can reach the VM as claude.
func TestSSHHostToVMClaude(t *testing.T) {
	signer := sshKey(t)
	out := runOne(t, vmAddr(), "claude", signer, "id")
	if !strings.Contains(out, "uid=1000(claude)") {
		t.Errorf("expected claude uid=1000, got: %q", out)
	}
	t.Logf("claude@VM: %s", out)
}

// TestSSHVMToHost verifies claude inside the VM can SSH back to the host,
// which is needed for deploying new rubbish binaries.
func TestSSHVMToHost(t *testing.T) {
	signer := sshKey(t)
	// From inside the VM, SSH to the host and run a no-op command.
	// The VM uses the same key for outbound auth.
	// Dev seed installs the deploy key as id_deploy (not id_ed25519).
	cmd := fmt.Sprintf(
		"ssh -o StrictHostKeyChecking=no -o BatchMode=yes -i ~/.ssh/id_deploy edwin@%s 'echo vm-to-host-ok'",
		hostIP(),
	)
	b := terminal.NewBridge(vmAddr(), "claude", signer)
	var buf bytes.Buffer
	err := b.RunSetupCapture([]string{
		// Ensure the host key is in the VM's known_hosts — seed it first.
		fmt.Sprintf("ssh-keyscan -H %s >> ~/.ssh/known_hosts 2>/dev/null || true", hostIP()),
		cmd,
	}, &buf)
	if err != nil {
		t.Fatalf("VM→host SSH: %v\noutput: %s", err, buf.String())
	}
	out := strings.TrimSpace(buf.String())
	if !strings.Contains(out, "vm-to-host-ok") {
		t.Errorf("expected vm-to-host-ok, got: %q", out)
	}
	t.Logf("VM→host: %s", out)
}
