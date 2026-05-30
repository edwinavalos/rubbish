# rubbish

A self-hosted platform for running coding agents (Claude Code) in isolated
Firecracker microVMs, with multi-tenancy, secrets management, and a
browser-based terminal interface.

The core idea: give coding agents a safe, auditable, tenant-isolated execution
environment that feels like a normal local coding session to the user.

**Deployment model:** self-hosted on the user's own infrastructure (on-prem
datacenter, nearby bare-metal, or cloud VMs with KVM available). Not a SaaS
offering.

---

## Current Status

The PoC phase is complete and running. A full end-to-end session flow is
functional:

| Capability | Status |
|---|---|
| Firecracker VM boot (~36ms) | ✅ working |
| SSH ready in VM (~1.1s) | ✅ working |
| WebSocket → SSH → PTY bridge (~69ms setup) | ✅ working |
| xterm.js browser terminal | ✅ working |
| Claude Code authenticated and running in VM | ✅ working |
| Per-session dm-thin CoW rootfs snapshots | ✅ working |
| NFS-mounted persistent `~/.claude/` (memory, projects) | ✅ working |
| Git bare-repo cache + in-VM clone (Workflow C) | ✅ working |
| Session persistence across restarts (JSON store) | ✅ working |
| Multi-session with slot management | ✅ working |
| gRPC control plane daemon (`rubishd`) | 🔲 designed, not wired |
| Shared workspace NFS storage | 🔲 designed, not implemented |
| Multi-tenancy / auth | 🔲 not started |
| Secrets management | 🔲 not started |

---

## Architecture

### Running components (as of 2026-05)

Two binaries run on the host as systemd services:

```
Browser (xterm.js)
    │  WebSocket /ws
    ▼
rubbish-poc :8080          — orchestrator: session lifecycle, VM launch, terminal proxy
    ├── SessionStore        — JSON-backed session state (internal/sessionstore)
    ├── SnapshotManager     — dm-thin CoW snapshots per session (internal/compute/firecracker)
    ├── RepoCacheManager    — bare-repo cache + local clone (internal/gitutil)
    ├── VM Launcher         — Firecracker via go-sdk (internal/vm)
    └── Terminal Bridge     — WebSocket → SSH → PTY (internal/terminal)

rubbish-terminal :8081     — standalone WebSocket terminal proxy (can restart independently)
```

```
Host networking
    br0  172.16.0.1/24  (Linux bridge)
    tapN  per-session TAP device attached to bridge
    iptables MASQUERADE: VM subnet → host uplink (VM gets full internet access)

Storage
    /dev/mapper/rubbish-pool       dm-thin sparse pool (100GB)
    /dev/mapper/rubbish-base-ro    read-only base image snapshot
    /dev/mapper/rubbish-session-N  per-session CoW snapshot (deleted on session end)

NFS mounts (dev-mode sessions)
    host /opt/rubbish/dev-seed/claude/ → VM ~/.claude/
        memory/    — persistent Claude memory across sessions
        projects/  — persistent Claude project context
```

```
VM image: Alpine 3.21 (ext4, 4GB)
    Go 1.26.2, Node.js 22, Claude Code
    claude user (uid=1000, sudo NOPASSWD, SSH pubkey injected at boot)
```

### Planned architecture (control plane)

See [docs/design-control-plane.md](docs/design-control-plane.md) for the full
design. The daemon (`rubishd`) will expose:

```
rubishd
├── AuthService          gRPC — identity, tokens, team/user management
├── SessionService       gRPC — session lifecycle (coordinates all layers)
├── WorkspaceService     gRPC — repos, worktrees, NFS exports, retention
├── ComputeService       gRPC — interface over Firecracker (swappable provider)
└── ImageService         gRPC — base image build, import, versioning
```

All on a single port; gRPC multiplexed with WebSocket terminal streams.

---

## Roadmap

### Phase 1 — PoC (complete)
- [x] Firecracker VM boots; SSH reachable
- [x] Browser terminal end-to-end (xterm.js → WS → SSH → PTY)
- [x] Claude Code authenticated and running in VM
- [x] Per-session dm-thin CoW rootfs snapshots
- [x] Persistent dev-mode mounts (`~/.claude/memory/`, `~/.claude/projects/`)
- [x] Bare-repo cache + in-VM git clone (Workflow C)
- [x] Multi-session slot management

### Phase 2 — Shared workspace storage
See [docs/design-shared-workspace.md](docs/design-shared-workspace.md).

- [ ] `/<team>/<user>/` workspace layout with NFS exports
- [ ] `WorkspaceManager`: repo clone, worktree create/prune, serialised fetch
- [ ] `RetentionReaper`: 30-day expiry from last VM shutdown

### Phase 3 — Ephemeral per-session rootfs
See [docs/design-ephemeral-rootfs.md](docs/design-ephemeral-rootfs.md).

- [x] dm-thin thin pool + per-session CoW snapshots
- [ ] Pool utilisation monitoring; refuse launches above 80%
- [ ] Base image update without affecting running sessions

### Phase 4 — Control plane daemon
See [docs/design-control-plane.md](docs/design-control-plane.md).

- [ ] `rubishd` entry point; replaces `rubbish-poc`
- [ ] gRPC services wired up (Auth, Session, Workspace, Compute, Image)
- [ ] grpc-gateway JSON transcoding for browser UI
- [ ] YAML config file

### Phase 5 — Harden
- [ ] Permissions: run Firecracker as dedicated `firecracker` user (udev rule for dm-thin)
- [ ] Secrets: agentsecrets integration for env-var injection at VM boot
- [ ] Multi-tenancy: team model, auth layer, web UI beyond raw xterm.js

---

## Development Setup

**Requirements:** Linux host with KVM support (bare-metal or cloud VM), Go 1.24+.

```bash
# One-time: build the VM rootfs image (requires root, loop-mount support)
sudo SSH_PUBKEY="$(cat ~/.ssh/id_ed25519.pub)" bash scripts/build-rootfs.sh

# One-time: set up bridge networking and iptables NAT
sudo bash scripts/setup-network.sh

# One-time: set up host tools, users, dm-thin pool, systemd units
sudo bash scripts/setup-host.sh

# Build and deploy the PoC binary
go build ./cmd/poc
sudo rubbish-deploy ./poc

# Run directly (as rubbish user, with SSH key for VM access)
sudo -u rubbish ./poc /opt/rubbish/ssh/id_ed25519
```

Browse to `http://<host>:8080` for the terminal UI.

---

## Repo Layout

```
cmd/
  poc/           — PoC orchestrator binary (session lifecycle + terminal proxy)
  terminal/      — Standalone terminal WebSocket proxy
  rubishd/       — Control plane daemon entry point (not yet wired)
internal/
  compute/firecracker/   — dm-thin snapshot manager
  gitutil/               — bare-repo cache, repo cloning helpers
  profile/               — session profile store
  session/               — session state machine
  sessionstore/          — JSON-backed session persistence
  terminal/              — WebSocket → SSH → PTY bridge
  vm/                    — Firecracker VM launcher + cleanup
proto/                   — gRPC service definitions
docs/                    — design documents (workspace, rootfs, control plane)
scripts/                 — host setup, rootfs build, networking
PROJECT.md               — vision and full roadmap detail
CHECKPOINT.md            — agent handoff context (current state, known issues)
RESEARCH_NOTES.md        — debugging findings
```

---

## Key Dependencies

- [`github.com/firecracker-microvm/firecracker-go-sdk`](https://github.com/firecracker-microvm/firecracker-go-sdk) — Firecracker VM management
- [`github.com/gorilla/websocket`](https://github.com/gorilla/websocket) — WebSocket server
- `golang.org/x/crypto/ssh` — SSH client for VM bridge
- [`github.com/sirupsen/logrus`](https://github.com/sirupsen/logrus) — structured logging
