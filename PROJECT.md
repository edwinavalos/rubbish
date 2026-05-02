# rubbish

A self-hosted platform for running coding agents (Claude Code etc.) in isolated Firecracker
microVMs, with multi-tenancy, secrets management, and a browser-based terminal interface.

The core idea: give coding agents a safe, auditable, tenant-isolated execution environment
that feels like a normal local coding session to the user.

---

## Vision

1. **MicroVM isolation** — each coding session runs in its own Firecracker VM; strong
   isolation boundary between tenants and between sessions
2. **Multi-tenancy** — users log in, create teams, assign members; team resources are
   invisible to other teams
3. **Agent access** — VM seeded with the user's git repo at a specific branch; agent
   exposed via a browser-based terminal (xterm.js over WebSocket → SSH → PTY)
4. **Secrets management** — secrets injected into the VM workload with attestation; the
   agent process should not be able to exfiltrate secrets it wasn't explicitly granted;
   full audit trail
5. **UX** — working with the agent feels the same as a normal local Claude Code session

**Deployment model:** self-hosted on the user's own infrastructure (on-prem datacenter,
nearby bare-metal, or cloud VMs with KVM available). Not a SaaS offering.

---

## Current Status

### PoC — browser terminal over Firecracker microVM ✓

**What works:**
- Firecracker VM boots in ~36ms; SSH ready in ~1.1s
- TAP networking (Linux bridge + iptables NAT) gives the VM full internet access
- WebSocket → SSH → PTY bridge at ~69ms session setup time
- xterm.js browser terminal renders and accepts input
- Claude Code 2.1.119 installed in the VM (Alpine 3.21 + Node.js 22)

**What's next:** authenticate a real Claude Code session through the browser terminal and
evaluate latency under actual agent workload.

---

## Rough Roadmap

### Phase 1 — Validate the core experience (now)
- [x] Firecracker VM launches and is reachable over SSH
- [x] Browser terminal connects end-to-end (xterm.js → WS → SSH → PTY)
- [x] Claude Code installed in the VM image
- [ ] Authenticate and run a real Claude Code session through the browser terminal
- [ ] Evaluate perceived latency; instrument keypress round-trip timing
- [ ] Clipboard bridge: implement OSC 52 so VM clipboard ops reach the host browser

### Phase 2 — Shared workspace storage
See [docs/design-shared-workspace.md](docs/design-shared-workspace.md) for the full design.

- [ ] Host NFS server setup (`nfs-kernel-server` on the bridge at `172.16.0.1`)
- [ ] `/<team>/<user>/` workspace directory structure on the host
- [ ] `WorkspaceManager`: repo clone, worktree create/prune, serialised git fetch
- [ ] `ExportManager`: dynamic NFS exports per session (ro default, rw opt-in)
- [ ] MMDS session metadata: inject team/user/repo/worktree into VM at boot
- [ ] VM `rcS` changes: NFS mount + worktree bind-mount at `/workspace`
- [ ] `RetentionReaper`: time-based expiry (default 30 days from last VM shutdown)

### Phase 3 — Ephemeral per-session rootfs
See [docs/design-ephemeral-rootfs.md](docs/design-ephemeral-rootfs.md) for the full design.

- [ ] Device mapper thin pool setup (`scripts/setup-storage.sh`)
- [ ] Base image import into pool as read-only snapshot
- [ ] `SnapshotManager`: create/delete thin snapshots per session
- [ ] Launcher accepts snapshot device path instead of hardcoded rootfs path
- [ ] Systemd unit for pool persistence across host reboots
- [ ] Pool utilisation monitoring; refuse launches above 80% threshold
- [ ] Base image update script without affecting running sessions

### Phase 4 — Control plane daemon
See [docs/design-control-plane.md](docs/design-control-plane.md) for the full design.

- [ ] `rubishd` daemon entry point; replaces `rubbish-poc` binary
- [ ] Proto definitions for `AuthService`, `SessionService`, `WorkspaceService`,
      `ImageService`
- [ ] gRPC server + grpc-gateway JSON transcoding for web UI
- [ ] `ComputeProvider` interface + Firecracker implementation
- [ ] `NetworkAttachment` abstraction
- [ ] `ImageService` with `provider_refs` for cross-provider image references
- [ ] SQLite state persistence; session reconciliation on restart
- [ ] WebSocket terminal handler wired to session lifecycle
- [ ] YAML config file

### Phase 5 — Harden the PoC infrastructure
- [ ] Per-session ephemeral rootfs (copy-on-write overlay so sessions don't share state)
- [ ] Permissions reconciliation: run firecracker as dedicated `firecracker` user via
      `CAP_SETUID`/`CAP_SETGID` on the rubbish binary; remove PoC sudoers shortcuts
- [ ] Clean VM lifecycle: teardown on WebSocket disconnect, not just on binary exit
- [ ] Repo seeding at VM boot: clone a git repo + branch into the VM before exposing shell

### Phase 3 — Secrets
- [ ] Integrate [The-17/agentsecrets](https://github.com/The-17/agentsecrets) for env-var
      injection into the VM workload
- [ ] VM boot flow: secrets injected before agent process starts; not readable after
- [ ] Audit log of secret access events

### Phase 4 — Multi-tenancy
- [ ] User auth and session management (control plane)
- [ ] Team model: users, teams, resource ownership
- [ ] VM pool per tenant; networking isolation between tenants
- [ ] Web UI beyond raw xterm.js

---

## Architecture

```
Browser (xterm.js)
    │  WebSocket (ws://host/ws)
    ▼
rubbish-poc (Go binary, :8080)
    │  SSH (golang.org/x/crypto/ssh)
    ▼
Firecracker microVM (172.16.0.2)
    │  PTY → bash → claude
    ▼
Claude Code process
```

**Host networking:**
- `br0` bridge at `172.16.0.1/24` on the host
- `tap0` TAP device attached to the bridge, owned by the VM
- iptables MASQUERADE rule: VM subnet → host uplink (internet access for the agent)

**VM image:** Alpine 3.21 (ext4, 2GB), built by `scripts/build-rootfs.sh`. Contains:
openssh, bash, curl, git, Node.js 22, Claude Code.

---

## Development Setup

See `scripts/build-rootfs.sh` to build a new rootfs image and `scripts/setup-network.sh`
for one-time bridge + NAT setup on the host.

```
# Build rootfs (run as root on a Linux host with loop-mount support)
sudo SSH_PUBKEY="$(cat ~/.ssh/id_ed25519.pub)" bash scripts/build-rootfs.sh

# One-time network setup
sudo bash scripts/setup-network.sh

# Run the PoC (as rubbish user, with SSH key for VM access)
./rubbish-poc /path/to/ssh/private/key
```

---

## Research Notes

See [RESEARCH_NOTES.md](RESEARCH_NOTES.md) for debugging findings, including:
- macOS Local Network Access permission blocking browser connections to private IPs
- Alpine VM requires devpts mounted at boot for SSH PTY allocation
