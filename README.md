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

## Project overview

rubbish gives you a browser-accessible terminal where Claude Code runs inside a
Firecracker microVM. You point it at a git repo and branch, the system boots a
fresh VM (CoW snapshot of a shared base image, ~36 ms), SSH is ready in ~1.1 s,
and you have a full Claude Code session in your browser without anything touching
your local machine.

Each session gets its own isolated VM with a copy-on-write rootfs snapshot,
NFS-mounted persistent workspace directories, and full internet access via NAT.

---

## Current status

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
| Git bare-repo cache + in-VM clone | ✅ working |
| Session persistence across restarts (SQLite store) | ✅ working |
| 4 concurrent sessions with slot management | ✅ working |
| MCP JSON-RPC 2.0 server for orchestrating agents | ✅ working |
| Non-interactive agent sessions (research / plan / implement roles) | ✅ working |
| 3-stage workflow engine (research → plan → parallel implement) | ✅ working |
| Usage-limit detection and workflow resume | ✅ working |
| Orchestrator UI (pipeline visualization, pagination, history) | ✅ working |
| virtiofs workspace share | ❌ stub only — Firecracker rejected virtiofs (PR #1351) |
| Running as non-root | ❌ udev resets dm-thin device ownership; under investigation |
| gRPC control plane daemon (`rubishd`) | 🔲 designed, not wired |
| Shared workspace NFS storage (full design) | 🔲 designed, not implemented |
| Multi-tenancy / auth | 🔲 not started |
| Secrets management | 🔲 not started |

---

## Architecture

### Running components

Two Go binaries run on the host as systemd services:

```
Browser (xterm.js)
    │  HTTP REST  :8080
    ├─────────────────────────────────────────────────────┐
    │  WebSocket  :8081                                   │
    ▼                                                     ▼
rubbish-poc :8080                           rubbish-terminal :8081
  — VM lifecycle, session management          — standalone WebSocket → SSH → PTY
  — dm-thin snapshot creation                   proxy; restarts independently
  — NFS mount setup                             of running VMs
  — in-VM git clone
  — SQLite session store
    │  SSH (golang.org/x/crypto/ssh)
    ▼
Firecracker microVM (172.16.0.x)
    PTY → tmux → bash → claude
    ▼
Claude Code process
```

#### rubbish-poc (:8080)

Owns VM lifecycle end-to-end: creates per-session dm-thin CoW snapshots, injects
per-slot network config, launches Firecracker via `firecracker-go-sdk`, waits for
SSH, runs in-VM setup commands (`claude` user, NFS mounts, repo clone), and
persists session state to SQLite (`/opt/rubbish/sessions.db`). Recovers sessions
on restart.

#### rubbish-terminal (:8081)

Standalone WebSocket terminal proxy. Can restart independently of running VMs —
active sessions reconnect automatically via a `{"type":"reconnect"}` signal so
the browser auto-reconnects silently.

### Internal packages

| Package | Files | Responsibility |
|---|---|---|
| `internal/vm` | `launcher.go`, `cleanup.go`, `detached.go`, `snapshot.go` | Firecracker VM launch, TAP device creation, network config injection, cleanup |
| `internal/compute/firecracker` | `snapshot.go` | dm-thin pool management: create/delete per-session CoW snapshots |
| `internal/terminal` | `bridge.go` | WebSocket → SSH → PTY bridge; `RunCapture` for separate stdout/stderr capture |
| `internal/session` | `statemachine.go` | Session state machine (creating → ready → stopped → …) |
| `internal/sessionstore` | `store.go` | SQLite-backed session and favorites persistence |
| `internal/gitutil` | `gitutil.go`, `repocache.go` | Repo name parsing, host-side bare-repo cache for fast in-VM clones |
| `internal/profile` | `store.go` | Claude OAuth and GitHub token persistence |
| `internal/workflow` | `engine.go`, `run.go`, `store.go` | 3-stage pipeline engine (research → plan → parallel implement), usage-limit detection, workflow resume |
| `internal/mcp` | `server.go`, `tools.go` | JSON-RPC 2.0 MCP server exposing workflow and session tools to orchestrating agents |

### Planned control plane

See [scripts/design-control-plane.md](scripts/design-control-plane.md) for the full
design. The `rubishd` daemon will expose:

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

## Networking

```
Host
  br0  172.16.0.1/24  (Linux bridge)
    ├── tap0  →  VM slot 0  172.16.0.2
    ├── tap1  →  VM slot 1  172.16.0.3
    ├── tap2  →  VM slot 2  172.16.0.4
    └── tap3  →  VM slot 3  172.16.0.5

iptables -t nat -A POSTROUTING -s 172.16.0.0/24 -o <uplink> -j MASQUERADE
```

- Each VM slot gets a dedicated TAP device (`tap0`–`tap3`) attached to the `br0`
  Linux bridge.
- The static IP for each slot is injected into the rootfs ext4 image by
  `vm.InjectNetworkConfig` before Firecracker boots — no DHCP needed.
- iptables MASQUERADE gives VMs full internet access through the host's uplink.
- NFS workspace mounts use the bridge IP (`172.16.0.1`) as the NFS server address.

Set up with `scripts/setup-network.sh` (idempotent; run on first install and each
host boot, or managed via systemd).

---

## Storage

### dm-thin pool

```
/opt/rubbish/dm/
  pool-data.img     — sparse data backing file  (100 GB)
  pool-meta.img     — thin pool metadata
  next-volume-id    — monotonic snapshot ID counter

/dev/mapper/rubbish-pool         — active thin pool device
/dev/mapper/rubbish-base-ro      — read-only base volume (imported from rootfs.ext4)
/dev/mapper/rubbish-session-<id> — per-session writable CoW snapshot
```

The two image files back a device-mapper thin pool. Set up by
`scripts/setup-storage.sh` and kept alive across reboots by
`rubbish-storage.service` (`Type=oneshot`).

### Per-session CoW snapshots

`internal/compute/firecracker.SnapshotManager` creates a writable thin snapshot
of `rubbish-base-ro` for each new session:

```
rubbish-base-ro  (read-only, never modified)
  └── rubbish-session-<uuid>  (writable CoW — mounted as rootfs by Firecracker)
```

Snapshots are deleted when the session is stopped. The base volume is untouched.

### NFS workspace mounts (dev-mode sessions)

| Guest path | Host source |
|---|---|
| `~claude/.claude/memory/` | `/opt/rubbish/dev-seed/claude/.claude/memory/` |
| `~claude/.claude/projects/` | `/opt/rubbish/dev-seed/claude/.claude/projects/` |

Mounted at boot via NFS over the bridge (`172.16.0.1`). Claude Code memory and
project context persist across VM restarts.

---

## VM image

The VM image is an Alpine 3.21 ext4 filesystem (4 GB) built by
`scripts/build-rootfs.sh`.

**Contents:**

- Alpine 3.21 (minirootfs base)
- Go 1.24.3 at `/usr/local/go`
- Node.js 22 (LTS)
- Claude Code 2.1.119 (installed globally via npm)
- openssh, bash, curl, git, nfs-utils
- `claude` user (uid=1000, passwordless sudo, SSH pubkey pre-baked)

**Build:**

```bash
# Run as root on a Linux host with loop-mount support
sudo SSH_PUBKEY="$(cat ~/.ssh/id_ed25519.pub)" bash scripts/build-rootfs.sh
# Output: /opt/rubbish/images/rootfs.ext4
```

`scripts/setup-storage.sh` then imports that ext4 image into the dm-thin pool as
the read-only base volume `rubbish-base-ro`.

---

## REST API (:8080) and WebSocket (:8081)

### rubbish-poc — REST API on :8080

| Method | Path | Description |
|---|---|---|
| `GET` | `/` | Session management UI |
| `GET` | `/orchestrator` | Workflow orchestrator UI (pipeline visualization) |
| `GET` | `/profile` | Profile management UI |
| `GET` | `/api/saved-token` | Check whether a Claude OAuth token is saved |
| `GET` | `/api/profile` | Return masked Claude OAuth and GitHub tokens |
| `PUT` | `/api/profile` | Update Claude OAuth and/or GitHub tokens |
| `GET` | `/api/sessions` | List sessions |
| `POST` | `/api/sessions` | Create a session (boots a VM) |
| `DELETE` | `/api/sessions/{id}` | Stop and destroy a session |
| `POST` | `/api/sessions/{id}/start` | Restart a stopped session |
| `POST` | `/api/sessions/{id}/favorite` | Save a session's repo/branch as a favorite |
| `GET` | `/api/favorites` | List favorites |
| `POST` | `/api/favorites` | Create a favorite |
| `DELETE` | `/api/favorites/{id}` | Delete a favorite |
| `GET` | `/api/workflows/` | List all workflows (sorted newest-first) |
| `POST` | `/api/workflows/{id}/resume` | Resume a failed workflow from its failed stages |
| `POST` | `/api/dev/test-workflow` | Inject a simulated demo workflow (no Claude usage) |
| `POST` | `/mcp` | MCP JSON-RPC 2.0 endpoint for agent orchestration |

**POST /api/sessions body:**

```json
{
  "repo_url": "https://github.com/owner/repo",
  "branch": "main",
  "github_token": "ghp_...",
  "dev_mode": true
}
```

### rubbish-terminal — WebSocket on :8081

| Path | Description |
|---|---|
| `GET /terminal/{session-id}` | Terminal HTML page for a session |
| `GET /ws/{session-id}` | WebSocket endpoint — proxies to the VM over SSH |

The WebSocket carries raw PTY bytes in each direction, plus a
`{"type":"reconnect"}` JSON frame sent by the server before a graceful restart.

---

## Build and deploy

### Prerequisites

- Linux host with `/dev/kvm` (bare-metal or cloud VM with nested virt)
- `dmsetup`, `thin-provisioning-tools` (`setup-storage.sh` installs these if absent)
- `nfs-kernel-server` for dev-mode NFS mounts
- Go 1.24+

### Build binaries

```bash
go build -o rubbish-poc      ./cmd/poc
go build -o rubbish-terminal ./cmd/terminal
```

### First-time host setup

```bash
# 1. Bridge + NAT networking
sudo bash scripts/setup-network.sh

# 2. dm-thin pool + base volume (idempotent — safe to re-run on reboot)
sudo bash scripts/setup-storage.sh

# 3. Build VM rootfs image
sudo SSH_PUBKEY="$(cat ~/.ssh/id_ed25519.pub)" bash scripts/build-rootfs.sh

# 4. Generate SSH key for VM access
sudo ssh-keygen -t ed25519 -f /opt/rubbish/ssh/id_ed25519 -N ""

# (Optional) full host setup including users, paths, systemd units:
sudo bash scripts/setup-host.sh
```

### Deploy binaries

```bash
sudo cp rubbish-poc      /usr/local/bin/rubbish-poc
sudo cp rubbish-terminal /usr/local/bin/rubbish-terminal
# Or use the deploy helper (on the test box):
sudo rubbish-deploy rubbish-poc
sudo rubbish-deploy rubbish-terminal
```

### systemd services

Three unit files are in `scripts/`:

| Unit file | Binary | Description |
|---|---|---|
| `rubbish-storage.service` | `rubbish-setup-storage` | Sets up dm-thin pool at boot (`Type=oneshot`) |
| `rubbish-poc.service` | `rubbish-poc --key /opt/rubbish/ssh/id_ed25519` | VM manager on :8080 |
| `rubbish-terminal.service` | `rubbish-terminal --key … --db /opt/rubbish/sessions.db` | Terminal proxy on :8081 |

Install:

```bash
sudo cp scripts/rubbish-storage.service  /etc/systemd/system/
sudo cp scripts/rubbish-poc.service      /etc/systemd/system/
sudo cp scripts/rubbish-terminal.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now rubbish-storage rubbish-poc rubbish-terminal
```

Logs:

```bash
tail -f /var/log/rubbish.log           # rubbish-poc
tail -f /var/log/rubbish-terminal.log  # rubbish-terminal
```

---

## Roadmap

### Phase 1 — PoC (complete)
- [x] Firecracker VM boots; SSH reachable
- [x] Browser terminal end-to-end (xterm.js → WS → SSH → PTY)
- [x] Claude Code authenticated and running in VM
- [x] Per-session dm-thin CoW rootfs snapshots
- [x] Persistent dev-mode mounts (`~/.claude/memory/`, `~/.claude/projects/`)
- [x] Bare-repo cache + in-VM git clone
- [x] Multi-session slot management (4 slots)

### Phase 2 — Shared workspace storage
See [scripts/design-shared-workspace.md](scripts/design-shared-workspace.md).

- [ ] `/<team>/<user>/` workspace layout with NFS exports
- [ ] `WorkspaceManager`: repo clone, worktree create/prune, serialised fetch
- [ ] `RetentionReaper`: 30-day expiry from last VM shutdown

### Phase 3 — Ephemeral per-session rootfs
See [scripts/design-ephemeral-rootfs.md](scripts/design-ephemeral-rootfs.md).

- [x] dm-thin thin pool + per-session CoW snapshots
- [ ] Pool utilisation monitoring; refuse launches above 80%
- [ ] Base image update without affecting running sessions

### Phase 4 — Control plane daemon
See [scripts/design-control-plane.md](scripts/design-control-plane.md).

- [ ] `rubishd` entry point; replaces `rubbish-poc`
- [ ] gRPC services wired up (Auth, Session, Workspace, Compute, Image)
- [ ] grpc-gateway JSON transcoding for browser UI
- [ ] YAML config file

### Phase 5 — Harden
- [ ] Permissions: run Firecracker as dedicated `firecracker` user (udev rule for dm-thin ownership)
- [ ] Secrets: agentsecrets integration for env-var injection at VM boot
- [ ] Multi-tenancy: team model, auth layer, web UI beyond raw xterm.js

---

## Repo layout

```
cmd/
  poc/           — PoC orchestrator binary (session lifecycle, REST API, workflow engine, MCP server)
  terminal/      — Standalone WebSocket terminal proxy
  rubishd/       — Control plane daemon entry point (not yet wired)
internal/
  compute/firecracker/   — dm-thin snapshot manager
  gitutil/               — bare-repo cache, repo cloning helpers
  mcp/                   — MCP JSON-RPC 2.0 server (agent orchestration tools)
  profile/               — Claude OAuth / GitHub token store
  session/               — session state machine
  sessionstore/          — SQLite session + favorites store
  terminal/              — WebSocket → SSH → PTY bridge
  vm/                    — Firecracker VM launcher + cleanup
  workflow/              — 3-stage pipeline engine; usage-limit detection; resume
proto/                   — gRPC service definitions (future control plane)
gen/                     — generated protobuf Go code
scripts/                 — host setup, rootfs build, networking, storage, systemd units, design docs
PROJECT.md               — vision and full roadmap detail
CHECKPOINT.md            — agent handoff context (current state, known bugs)
RESEARCH_NOTES.md        — debugging findings
```

---

## Key dependencies

- [`github.com/firecracker-microvm/firecracker-go-sdk`](https://github.com/firecracker-microvm/firecracker-go-sdk) — Firecracker VM management
- [`github.com/gorilla/websocket`](https://github.com/gorilla/websocket) — WebSocket server
- `golang.org/x/crypto/ssh` — SSH client for VM bridge
- `log/slog` (stdlib) — structured logging throughout; `logrus` is kept as a transitive dependency of `firecracker-go-sdk` but is silenced with `io.Discard`
