//go:build integration

package terminal_test

// TestRunCapture_SeparatesStreams verifies that RunCapture returns stdout and
// stderr in separate strings. It requires a live SSH target (same env vars as
// the other integration tests).
//
// Run with: go test -tags integration ./internal/terminal/ -v -run TestRunCapture
func TestRunCapture_SeparatesStreams(t *testing.T) {
	keyPath := envOrDefault("RUBBISH_SSH_KEY", "/opt/rubbish/ssh/id_ed25519")
	vmAddr := envOrDefault("RUBBISH_VM_ADDR", "172.16.0.2:22")

	keyBytes, err := os.ReadFile(keyPath)
	if err != nil {
		t.Skipf("SSH key not found at %s: %v", keyPath, err)
	}
	signer, err := ssh.ParsePrivateKey(keyBytes)
	if err != nil {
		t.Fatalf("parse key: %v", err)
	}

	b := terminal.NewBridge(vmAddr, "root", signer)

	// Write distinct strings to stdout and stderr in one command.
	stdout, stderr, err := b.RunCapture(`echo "to-stdout"; echo "to-stderr" >&2`)
	if err != nil {
		t.Fatalf("RunCapture: %v", err)
	}

	if !strings.Contains(stdout, "to-stdout") {
		t.Errorf("stdout = %q, want to contain %q", stdout, "to-stdout")
	}
	if strings.Contains(stdout, "to-stderr") {
		t.Errorf("stdout = %q, should NOT contain stderr content", stdout)
	}
	if !strings.Contains(stderr, "to-stderr") {
		t.Errorf("stderr = %q, want to contain %q", stderr, "to-stderr")
	}
	if strings.Contains(stderr, "to-stdout") {
		t.Errorf("stderr = %q, should NOT contain stdout content", stderr)
	}
}

// TestRunCapture_NonZeroExit verifies that RunCapture returns the output even
// when the command exits non-zero.
func TestRunCapture_NonZeroExit(t *testing.T) {
	keyPath := envOrDefault("RUBBISH_SSH_KEY", "/opt/rubbish/ssh/id_ed25519")
	vmAddr := envOrDefault("RUBBISH_VM_ADDR", "172.16.0.2:22")

	keyBytes, err := os.ReadFile(keyPath)
	if err != nil {
		t.Skipf("SSH key not found at %s: %v", keyPath, err)
	}
	signer, err := ssh.ParsePrivateKey(keyBytes)
	if err != nil {
		t.Fatalf("parse key: %v", err)
	}

	b := terminal.NewBridge(vmAddr, "root", signer)

	stdout, stderr, cmdErr := b.RunCapture(`echo "output before failure"; exit 1`)
	_ = stderr // may or may not be empty

	if cmdErr == nil {
		t.Error("expected non-nil error for exit 1")
	}
	if !strings.Contains(stdout, "output before failure") {
		t.Errorf("stdout = %q, want output captured even on non-zero exit", stdout)
	}
}

// envOrDefault mirrors the helper in ssh_integration_test.go.
func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// Ensure bytes import is satisfied (used in ssh_integration_test.go).
var _ = bytes.NewBuffer
var _ = fmt.Sprintf
