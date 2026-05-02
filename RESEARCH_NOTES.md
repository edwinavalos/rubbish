# Research Notes

## Testing & Debugging

### macOS Local Network Access blocks browser connections to private IPs

**Symptom:** Firefox (and potentially other browsers) shows `NS_ERROR_CONNECTION_REFUSED`
when connecting to a local/private IP (e.g. `192.168.1.x`) over plain HTTP. Zero TCP
packets appear on the wire — no SYN is ever sent.

**Root cause:** macOS Sequoia (and Ventura+) enforces a per-app **Local Network Access**
permission. If an app (Firefox, Chrome, etc.) has been denied this permission — silently or
via a dismissed prompt — the OS blocks outgoing connections to RFC-1918 addresses at the
socket layer, returning `ECONNREFUSED` before any packet is sent.

CLI tools (`curl`, `wget`) and headless automation (Playwright) are typically exempt or
granted access by default, which is why those work while the browser does not.

**Fix:** System Settings → Privacy & Security → Local Network → toggle the browser on.
If the browser isn't listed, loading any local-IP URL in a fresh tab should trigger the
permission prompt.

**Implications for rubbish:** Any user running the control-plane UI from their local machine
against a self-hosted node on the same LAN will need to grant Local Network Access to their
browser. Worth documenting in setup instructions and/or detecting client-side (the WebSocket
`onerror` is indistinguishable from a real server error, so a clear setup guide is the only
mitigation).

---

### Clipboard operations from VM don't reach host OS clipboard

**Symptom:** Claude Code prompts the user to press a key to copy something to clipboard;
the VM-side copy appears to succeed but nothing appears in the host OS clipboard.

**Root cause:** The VM has no display server (X11/Wayland) and no access to the host
clipboard. Commands like `xclip`, `xsel`, or `pbcopy` either fail silently or operate on a
clipboard that doesn't exist. The WebSocket → SSH → PTY bridge doesn't relay clipboard
operations.

**Proper fix (deferred):** Implement OSC 52 escape sequence support. OSC 52 is the standard
terminal protocol for clipboard access — the application writes a base64-encoded payload in
a specific escape sequence, and the terminal (xterm.js) intercepts it and calls
`navigator.clipboard.writeText()`. xterm.js supports this natively with
`allowProposedApi: true`. The VM's shell profile would alias clipboard commands to emit OSC
52 sequences instead of calling xclip/pbcopy.

**Status:** Known limitation, deferred. Fine for initial PoC testing.

---

### VM rootfs: devpts must be mounted for SSH PTY allocation

**Symptom:** WebSocket connects (HTTP 101), server immediately sends
`PTY request failed: ssh: pty-req failed` (45-byte WS frame) and RSTs the connection.
SSH dial and session creation succeed; only PTY allocation fails.

**Root cause:** Alpine minirootfs does not mount `devpts` at boot by default. Without
`/dev/pts` (devpts filesystem), sshd cannot allocate pseudo-terminals.

**Fix:** Add to `/etc/init.d/rcS` before starting sshd:
```sh
mkdir -p /dev/pts
mount -t devpts devpts /dev/pts
mkdir -p /proc
mount -t proc proc /proc
```

Already fixed in `scripts/build-rootfs.sh`.
