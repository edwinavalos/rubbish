# Design: Control Plane

## Status
Design — not yet implemented.

## Problem

The current `rubbish-poc` binary is a single-session process: it launches one VM, serves
one WebSocket terminal, and exits. There is no concept of multiple concurrent sessions,
users, teams, persistent workspaces, or base image management. Everything is hardcoded.

The control plane is the long-running daemon that owns all of this. It runs on the host,
manages the full lifecycle of sessions and resources, and exposes a typed API that the web
UI and CLI talk to.

## Goals

- Single long-running daemon per host node
- gRPC API for all control operations (session management, workspace management, auth)
- WebSocket endpoint for terminal streams (one per active session)
- Clean abstraction boundary between the orchestration layer and the compute provider,
  so that the Firecracker implementation can be swapped for another provider without
  touching orchestration logic
- Same abstraction for base images and networking — provider-specific details stay inside
  the provider implementation
- No general-purpose compute scheduling; rubbish is not Kubernetes and should not become
  one

## Non-Goals

- Multi-host scheduling and VM placement (single node for MVP)
- General-purpose container or workload orchestration
- High availability of the daemon itself (single node; host reboot kills the daemon)

---

## API Layer

### Protocol choices

| Interface | Protocol | Rationale |
|---|---|---|
| Management API | gRPC (HTTP/2) | Typed contracts, streaming support, works for both web UI and CLI |
| Browser (web UI) | gRPC-Web via Envoy proxy or grpc-gateway | Browsers cannot speak native gRPC HTTP/2 |
| Terminal stream | WebSocket (HTTP/1.1 upgrade) | xterm.js expects WebSocket; multiplexed on same port via path routing |
| Health check | HTTP/1.1 GET `/healthz` | Simple; used by systemd watchdog and future load balancers |

All traffic on a single port (default: `8080` for HTTP, `8443` for TLS). The Go HTTP/2
server can multiplex gRPC, gRPC-Web, WebSocket upgrades, and plain HTTP on the same
listener.

### Service decomposition

```
rubbish daemon
├── AuthService          — identity, tokens, team/user management
├── SessionService       — orchestrates a unit of work; coordinates all layers below
├── WorkspaceService     — persistent layer: repos, worktrees, NFS exports, retention
├── ComputeService       — ephemeral layer: interface over concrete compute providers
└── ImageService         — base image management: build, import, version, distribute
```

Each service is a gRPC service definition (`.proto` file). The daemon implements all of
them in a single process. There is no inter-service network traffic — they are Go packages
calling each other directly, with the gRPC layer only at the API boundary.

---

## Service Definitions

### AuthService

Handles identity and access. All other service RPCs require a valid token in metadata.

```protobuf
service AuthService {
  rpc Login(LoginRequest) returns (LoginResponse);
  rpc Logout(LogoutRequest) returns (LogoutResponse);
  rpc CreateAPIKey(CreateAPIKeyRequest) returns (CreateAPIKeyResponse);
  rpc ListAPIKeys(ListAPIKeysRequest) returns (ListAPIKeysResponse);
  rpc RevokeAPIKey(RevokeAPIKeyRequest) returns (RevokeAPIKeyResponse);
  rpc CreateTeam(CreateTeamRequest) returns (CreateTeamResponse);
  rpc AddTeamMember(AddTeamMemberRequest) returns (AddTeamMemberResponse);
  rpc RemoveTeamMember(RemoveTeamMemberRequest) returns (RemoveTeamMemberResponse);
}
```

Auth is token-based. Two token types:
- **Session token**: short-lived (24h), issued on `Login`, used by interactive web UI
- **API key**: long-lived, issued on `CreateAPIKey`, used by CLI and automation

Tokens are passed as gRPC metadata: `authorization: Bearer <token>`.

For MVP, user/password credentials are stored locally (bcrypt hashed) in a SQLite database
on the host. No external identity provider required.

---

### SessionService

The central orchestrating service. A session represents one unit of work: one agent, one
worktree, one VM. `SessionService` does not directly manage VMs or filesystems — it
delegates to `ComputeService`, `WorkspaceService`, and `ImageService` and holds the
assembled state.

```protobuf
service SessionService {
  rpc CreateSession(CreateSessionRequest) returns (CreateSessionResponse);
  rpc GetSession(GetSessionRequest) returns (Session);
  rpc ListSessions(ListSessionsRequest) returns (ListSessionsResponse);
  rpc StopSession(StopSessionRequest) returns (StopSessionResponse);
  rpc GetSessionTerminalURL(GetSessionTerminalURLRequest) returns (GetSessionTerminalURLResponse);
  rpc WatchSession(WatchSessionRequest) returns (stream SessionEvent);
}

message CreateSessionRequest {
  string team_slug    = 1;
  string user_slug    = 2;
  string repo_url     = 3;  // cloned if not already present in workspace
  string branch       = 4;  // worktree branch name
  string image_id     = 5;  // base image to boot from (from ImageService)
  SessionResources resources = 6;
  bool   write_enabled = 7;  // allow write access to sibling worktrees
}

message Session {
  string id           = 1;
  string team_slug    = 2;
  string user_slug    = 3;
  string repo         = 4;
  string worktree     = 5;
  SessionState state  = 6;  // STARTING | READY | STOPPING | STOPPED | FAILED
  string terminal_url = 7;  // ws://host/terminal/<session-id>
  google.protobuf.Timestamp created_at  = 8;
  google.protobuf.Timestamp ready_at    = 9;
  google.protobuf.Timestamp stopped_at  = 10;
}

message SessionResources {
  int32 vcpus      = 1;
  int32 memory_mib = 2;
}
```

**Session launch sequence:**
1. Validate request; check auth
2. `WorkspaceService.EnsureRepo(team, user, repoURL)` — clone if not present
3. `WorkspaceService.CreateWorktree(team, user, repo, branch)` — returns worktree path
4. `ImageService.ResolveImage(imageID)` — returns provider-specific image reference
5. `ComputeService.CreateSandbox(imageRef, resources, worktreePath)` — returns
   `SandboxHandle`
6. `ComputeService.StartSandbox(handle)` — returns `NetworkAttachment`
7. `WorkspaceService.GrantAccess(team, user, vmIP, writeEnabled)` — configure NFS exports
8. Wait for SSH ready (polled by daemon, not exposed to caller)
9. Establish SSH connection + PTY session; register WebSocket handler at
   `/terminal/<session-id>`
10. Set session state to `READY`; return session to caller

**Session stop sequence:**
1. Close WebSocket connection (if active)
2. Close SSH session
3. `ComputeService.StopSandbox(handle)`
4. `WorkspaceService.RevokeAccess(vmIP)` — remove NFS exports
5. `ComputeService.DestroySandbox(handle)` — delete snapshot, release resources
6. Record `stopped_at`; set state to `STOPPED`

---

### WorkspaceService

Manages the persistent filesystem layer. Owns the NFS server configuration, git repo
lifecycle, worktree lifecycle, and retention policy.

```protobuf
service WorkspaceService {
  rpc EnsureRepo(EnsureRepoRequest) returns (EnsureRepoResponse);
  rpc ListRepos(ListReposRequest) returns (ListReposResponse);
  rpc DeleteRepo(DeleteRepoRequest) returns (DeleteRepoResponse);

  rpc CreateWorktree(CreateWorktreeRequest) returns (CreateWorktreeResponse);
  rpc ListWorktrees(ListWorktreesRequest) returns (ListWorktreesResponse);
  rpc DeleteWorktree(DeleteWorktreeRequest) returns (DeleteWorktreeResponse);

  rpc FetchRepo(FetchRepoRequest) returns (FetchRepoResponse);

  rpc SetRetentionPolicy(SetRetentionPolicyRequest) returns (SetRetentionPolicyResponse);
  rpc ListExpiredWorktrees(ListExpiredWorktreesRequest) returns (ListExpiredWorktreesResponse);
}
```

Internal components (Go, not exposed over gRPC):
- **`WorkspaceManager`**: git operations, directory management
- **`ExportManager`**: manages `/etc/exports.d/` and calls `exportfs -r`
- **`RetentionReaper`**: background goroutine; prunes/archives expired worktrees

See [design-shared-workspace.md](design-shared-workspace.md) for full detail.

---

### ImageService

Manages base images — the read-only rootfs that every VM session snapshots from.
Abstracts over the fact that a "base image" means different things to different compute
providers (a device mapper thin volume for Firecracker, an AMI for EC2, a container image
for a Docker provider).

```protobuf
service ImageService {
  rpc ListImages(ListImagesRequest) returns (ListImagesResponse);
  rpc GetImage(GetImageRequest) returns (Image);
  rpc ImportImage(ImportImageRequest) returns (stream ImportImageProgress);
  rpc DeleteImage(DeleteImageRequest) returns (DeleteImageResponse);
  rpc SetDefaultImage(SetDefaultImageRequest) returns (SetDefaultImageResponse);
}

message Image {
  string id           = 1;  // opaque, assigned on import
  string name         = 2;  // human-readable, e.g. "alpine-claude-2.1.119"
  string version      = 3;
  repeated string tags = 4;
  bool   is_default   = 5;
  google.protobuf.Timestamp imported_at = 6;
  map<string, string> provider_refs = 7;
  // provider_refs maps provider name to provider-specific image reference
  // e.g. {"firecracker": "/dev/mapper/rubbish-base-ro-v3", "ec2": "ami-0abc123"}
}
```

`provider_refs` is the key field — when `SessionService` asks `ImageService` to resolve an
image for a specific compute provider, `ImageService` looks up the provider-specific
reference and returns it. This means the same logical image (`alpine-claude-2.1.119`) can
have a Firecracker thin volume reference and an EC2 AMI reference simultaneously.

`ImportImage` takes a local ext4 file path, imports it into the active compute provider's
image store (for Firecracker: loads into device mapper thin pool), and records the
`provider_refs` entry.

---

### ComputeService (Interface)

This is the abstraction boundary. `ComputeService` is a Go interface, not a gRPC service.
It is not exposed over the API — it is an internal dependency of `SessionService`.

```go
// ComputeProvider is the interface every compute backend must implement.
type ComputeProvider interface {
    // CreateSandbox provisions a sandbox from a base image but does not start it.
    // imageRef is the provider-specific image reference from ImageService.
    // worktreePath is the host path to mount as the session workspace.
    CreateSandbox(ctx context.Context, req CreateSandboxRequest) (SandboxHandle, error)

    // StartSandbox boots the sandbox and returns its network attachment.
    StartSandbox(ctx context.Context, handle SandboxHandle) (NetworkAttachment, error)

    // StopSandbox gracefully shuts down the sandbox.
    StopSandbox(ctx context.Context, handle SandboxHandle) error

    // DestroySandbox releases all resources associated with the sandbox.
    // Always called after StopSandbox, even if StopSandbox failed.
    DestroySandbox(ctx context.Context, handle SandboxHandle) error

    // DescribeSandbox returns current state of a sandbox.
    DescribeSandbox(ctx context.Context, handle SandboxHandle) (SandboxStatus, error)

    // Name returns the provider identifier, e.g. "firecracker", "ec2".
    Name() string
}

type CreateSandboxRequest struct {
    SessionID    string
    ImageRef     string            // provider-specific; from ImageService.provider_refs
    Resources    SandboxResources
    WorktreePath string            // host path; provider mounts or exposes this to sandbox
    Metadata     map[string]string // arbitrary provider config (kernel path, etc.)
}

type SandboxHandle struct {
    ID           string
    ProviderName string
    ProviderData any  // opaque provider-specific state (e.g. Firecracker Machine struct)
}

type SandboxResources struct {
    VCPUs     int64
    MemoryMiB int64
}

type SandboxStatus struct {
    State   SandboxState // CREATING | RUNNING | STOPPING | STOPPED | FAILED
    Message string       // human-readable detail on failure
}
```

---

## NetworkAttachment

Returned by `ComputeProvider.StartSandbox`. Describes how the sandbox is reachable and
what network it sees. Provider-specific details are opaque to the rest of the system.

```go
type NetworkAttachment struct {
    // ReachableAt is how the control plane dials into the sandbox.
    // For Firecracker: "172.16.0.2:22"
    // For a cloud VM: "10.0.1.47:22" or a public DNS name
    ReachableAt string

    // GatewayIP is the default route the sandbox should use.
    // For Firecracker: "172.16.0.1"
    GatewayIP string

    // DNS resolvers to inject into the sandbox.
    DNS []string

    // ProviderMeta holds provider-specific detail.
    // Firecracker: TAPDevice string, BridgeName string
    // EC2: VPCID string, SubnetID string, ENIID string
    // Kept opaque to everything outside the provider implementation.
    ProviderMeta any
}
```

---

## Firecracker Provider (First Implementation)

The Firecracker provider implements `ComputeProvider`. All Firecracker-specific code lives
in `internal/compute/firecracker/`. Nothing outside this package knows about TAP devices,
device mapper, the go-sdk, or kernel paths.

**`CreateSandbox`:**
1. Call `SnapshotManager.CreateSnapshot(sessionID)` → `/dev/mapper/rubbish-session-<id>`
2. Call `TAP.Create(sessionID)` → `tap-<short-id>` device, attached to `br0`
3. Build Firecracker config (kernel, rootfs = snapshot device, TAP, vCPUs, RAM, MMDS data)
4. Return `SandboxHandle` with the Firecracker machine config embedded in `ProviderData`

**`StartSandbox`:**
1. Call `firecracker.NewMachine()` + `machine.Start(ctx)` via go-sdk
2. Poll SSH port until ready
3. Return `NetworkAttachment{ReachableAt: "172.16.0.2:22", GatewayIP: "172.16.0.1", ...}`

**`StopSandbox`:**
1. `machine.Shutdown(ctx)` via go-sdk

**`DestroySandbox`:**
1. `TAP.Delete(tapName)`
2. `SnapshotManager.DeleteSnapshot(sessionID)`
3. Remove Firecracker socket file

---

## Internal Architecture

```
cmd/
  rubishd/
    main.go           ← daemon entry point; wires everything together

internal/
  api/
    grpc/             ← gRPC server; thin handlers that call internal services
    websocket/        ← WebSocket terminal handler (unchanged from PoC)
    middleware/       ← auth token validation, logging, rate limiting
  auth/               ← AuthService implementation; SQLite-backed user/token store
  session/            ← SessionService implementation; session state machine
  workspace/          ← WorkspaceService implementation; git + NFS management
  image/              ← ImageService implementation; image registry
  compute/
    provider.go       ← ComputeProvider interface + shared types
    firecracker/      ← Firecracker implementation of ComputeProvider
      launcher.go     ← VM launch (from PoC, refactored)
      snapshot.go     ← SnapshotManager (device mapper)
      tap.go          ← TAP device lifecycle
      provider.go     ← implements ComputeProvider interface
  storage/
    db.go             ← SQLite database (sessions, users, tokens, images)

proto/
  rubbish/v1/
    auth.proto
    session.proto
    workspace.proto
    image.proto

scripts/
  setup-storage.sh    ← device mapper thin pool setup
  setup-network.sh    ← bridge + NAT setup (existing)
  build-rootfs.sh     ← rootfs image build (existing)
```

---

## State Persistence

The daemon uses a single SQLite database at `/opt/rubbish/data/rubbish.db`. Tables:

- `users` — id, team_slug, user_slug, password_hash, created_at
- `teams` — id, slug, name, created_at
- `api_keys` — id, user_id, name, key_hash, last_used_at, expires_at
- `sessions` — id, team_slug, user_slug, repo, worktree, state, image_id,
  sandbox_handle (JSON), network_attachment (JSON), created_at, ready_at, stopped_at
- `images` — id, name, version, tags (JSON), provider_refs (JSON), is_default,
  imported_at
- `worktrees` — id, team_slug, user_slug, repo, branch, path, last_shutdown_at

`sandbox_handle` and `network_attachment` are stored as JSON blobs. On daemon restart,
active sessions are reconciled: any session in state `STARTING` or `RUNNING` is checked
against the actual Firecracker process list. Sessions whose VMs are gone are marked
`FAILED`. Sessions whose VMs are still running are reconnected.

---

## Configuration

The daemon is configured via a YAML file at `/etc/rubbish/config.yaml`:

```yaml
node:
  id: "node-1"
  listen_addr: ":8080"

compute:
  provider: "firecracker"
  firecracker:
    binary: "/usr/local/bin/firecracker"
    kernel: "/opt/rubbish/firecracker/vmlinux"
    bridge: "br0"
    bridge_ip: "172.16.0.1"
    vm_subnet: "172.16.0.0/24"
    socket_dir: "/run/rubbish/fc"

storage:
  workspace_root: "/opt/rubbish/workspaces"
  db_path: "/opt/rubbish/data/rubbish.db"
  dm_pool_device: "/dev/mapper/rubbish-pool"
  retention_days: 30

auth:
  token_ttl_hours: 24
```

---

## Known Limitations and Future Work

**No general-purpose scheduling**: The daemon assigns VMs to a fixed IP range
(`172.16.0.0/24`). This supports at most 253 concurrent VMs per node. A real multi-host
deployment would need a network allocation service. Explicitly out of scope.

**Single compute provider active at a time**: The config specifies one provider. Supporting
multiple providers simultaneously (e.g. some sessions on Firecracker, some on EC2) requires
a routing layer in `SessionService`. Deferred.

**gRPC-Web proxy**: The web UI cannot speak native gRPC. For MVP, `grpc-gateway` generates
a JSON/REST transcoding layer automatically from the `.proto` files. This avoids running a
separate Envoy proxy. Can be replaced with native gRPC-Web later.

**No we-invented-Kubernetes moment**: If you find yourself building a scheduler, a service
mesh, or a distributed state store — stop. Use Nomad or a thin Kubernetes layer instead.
Rubbish's value is in the workspace/agent/VM integration layer, not in generic compute
orchestration.
