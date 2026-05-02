# Design: Shared Workspace Storage

## Status
Design — not yet implemented.

## Problem

Each Firecracker VM is ephemeral. When a VM is torn down, any git repos or worktrees it
checked out are gone. For a coding agent platform where a user might run multiple concurrent
agents across multiple repos (and want those agents to be able to see each other's work),
this means either:

- Re-cloning on every session launch (slow, wasteful), or
- Having no cross-session or cross-agent visibility at all

We need a persistent, shared filesystem that outlives individual VMs, is visible to all of
a user's VMs simultaneously, and survives workstream completion for a configurable
recovery window.

---

## Goals

- One persistent home directory per user, mounted into every VM they launch
- Git repos and worktrees inside that home directory persist across VM restarts and shutdowns
- Multiple concurrent VMs (agents) can read each other's worktrees
- Write access to another agent's worktree is opt-in, off by default
- Home directories are organised under `/<team>/<user>/` to allow future team-level shared
  resources without being opinionated about team structure
- Home directory is retained for a configurable period after the last VM shuts down
  (default: 30 days), then eligible for archival or deletion

## Non-Goals

- Shared build caches across VMs (deferred — too language/toolchain specific)
- Real-time file sync between VM working trees (agents coordinate via git, not inotify)
- Distributed locking infrastructure (handled by convention + rubbish serialising git
  operations)

---

## Decision: NFS over the VM Bridge Network

### Why not virtiofs

Virtiofs is the ideal technical solution — no network hop, better POSIX semantics, host
page cache shared directly with guests. However, Firecracker deliberately does not implement
it. A WIP implementation (PR #1351) was rejected in 2020 on security grounds (large new
attack surface in the VMM emulation layer) and there is no roadmap item to revisit this.
Firecracker's device model is intentionally minimal: virtio-net, virtio-blk, virtio-vsock,
virtio-rng, virtio-balloon, virtio-pmem. No vhost-user device backends.

### Why not a shared block device

Attaching the same ext4 image to multiple VMs read-write simultaneously causes filesystem
corruption — ext4 has no cluster-aware journal. Read-only shared block works, but then
agents cannot write to their worktrees without a per-VM overlay layer, which requires
rebuilding squashfs images to update shared repos. This does not fit a model where repos are
cloned live and updated continuously.

### NFS over virtio-net

The host runs an NFSv3 server. Each VM mounts the user's home directory over the TAP bridge
at `172.16.0.1` on startup. This is production-proven for this exact use case: Turso's
AgentFS (an AI agent filesystem) uses NFS over the bridge network with Firecracker VMs and
documents it as the recommended pattern.

**Tradeoffs accepted:**
- Each file open/stat crosses the virtio-net path (no kernel bypass — Firecracker does not
  support vhost-net). For a coding agent reading source files this is acceptable; it is not
  a latency-sensitive hot path.
- NFSv3 is preferred over NFSv4 for small-file workloads (fewer round-trips per open/close).
- NFS over a local bridge is not the same as loopback NFS — the VM has its own kernel, so
  the loopback deadlock under memory pressure does not apply.

**Recommended mount options inside VM:**
```
vers=3,proto=tcp,rsize=131072,wsize=131072,noatime,nodiratime,soft,timeo=30
```

---

## Directory Layout

### Host filesystem

```
/opt/rubbish/workspaces/
  <team-slug>/
    <user-slug>/
      <repo-name>/
        .git/             ← shared git internals and object store
        main/             ← main checkout (or bare clone working tree)
        <worktree-name>/  ← per-agent linked worktree
      <another-repo>/
        ...
    shared/               ← future: team-level repos (not per-user)
      <repo-name>/
        ...
```

### Inside every VM

```
/home/user/               ← NFS mount of /opt/rubbish/workspaces/<team>/<user>/
  my-api/
    .git/
    main/
    feature-auth/         ← this agent's active worktree (read-write)
    fix-login/            ← another agent's worktree (read-only by default)
  my-frontend/
    .git/
    main/
```

The active worktree for a session is bind-mounted (or symlinked) to `/workspace` inside
the VM as a convenience:

```
/workspace -> /home/user/my-api/feature-auth/
```

---

## Git Worktree Model

Each agent session corresponds to one git worktree. Rubbish manages worktree lifecycle:

**Session launch:**
1. If the repo does not exist under `/<team>/<user>/`, rubbish clones it:
   `git clone --no-checkout <url> /opt/rubbish/workspaces/<team>/<user>/<repo>/`
   and creates a `main` working tree.
2. Rubbish creates a linked worktree for the session:
   `git -C /opt/rubbish/workspaces/<team>/<user>/<repo> worktree add <branch-name>`
3. The worktree path is passed to the VM at boot as the active workspace.

**During session:**
- The agent works exclusively in its own worktree path.
- It can read other worktrees in the same user home (they appear as sibling directories).
- Write access to sibling worktrees is controlled by NFS export options (see below).

**Session teardown:**
- The VM is shut down; the worktree remains on the host.
- The worktree is not pruned until the user explicitly closes the workstream or the
  retention window expires.

**Retention and expiry:**
- A `last_shutdown_at` timestamp is recorded per worktree when a VM stops.
- After the configurable retention window (default: 30 days), the worktree is eligible
  for archival (tar + compress to cold storage) or deletion.
- The `main` worktree (and the `.git` object store) is retained as long as any linked
  worktree exists or the repo has been accessed within the window.

---

## Concurrency and File Locking

### What is safe

- **Concurrent reads from multiple VMs**: safe. NFS read operations are stateless at the
  server. Multiple agents reading the same source files simultaneously is fine.
- **Each agent writing to its own worktree**: safe. Linked worktrees have independent
  indexes and working directories. No two agents share a worktree path.
- **Git object store reads**: safe. The `.git/objects/` pack files are immutable once
  written; concurrent reads by multiple agents are safe.

### What requires coordination

- **`git fetch` / `git pull`**: writes to `.git/packed-refs` and the object store. If two
  agents run fetch on the same repo simultaneously, they can corrupt the pack index. Rubbish
  must serialise fetch operations per repo using a host-side lock (a simple advisory lock
  file or a queue in the control plane).
- **`git worktree add/remove`**: modifies `.git/worktrees/`. Rubbish runs these on the
  host (not inside the VM) so they are naturally serialised through the control plane.

### What the user owns

- Write access to sibling worktrees is off by default and requires explicit opt-in per
  session. If enabled, the user accepts responsibility for conflicts. Rubbish will warn at
  session start that write access to shared worktrees is active.
- If two agents with mutual write access edit the same file, last write wins. Rubbish does
  not implement distributed file locking beyond what NFS provides (advisory locks via
  `lockd`).

---

## NFS Export Configuration

The host exports per-user home directories. Each export is scoped to the bridge subnet
(`172.16.0.0/24`):

```
# /etc/exports

# Read-only export (default for all sessions)
/opt/rubbish/workspaces/<team>/<user>  172.16.0.0/24(ro,sync,no_subtree_check,root_squash)

# Read-write export (opt-in sessions)
/opt/rubbish/workspaces/<team>/<user>  172.16.0.0/24(rw,sync,no_subtree_check,root_squash)
```

**`root_squash`** is important: it maps root inside the VM to the `nobody` user on the
host, preventing a compromised VM from gaining host root privileges over the NFS mount.

In practice, rubbish manages exports dynamically:
- Default: the user's home is exported `ro` to all VMs.
- When a session is launched, rubbish re-exports the specific worktree path as `rw` to
  the VM's TAP IP address only. Other VMs continue to see `ro`.
- On session teardown, the `rw` export is removed.

Dynamic export management uses `exportfs -r` after updating `/etc/exports.d/<session-id>`.

---

## Host Setup

One-time setup on the rubbish host:

```bash
# Install NFS server
apt install -y nfs-kernel-server

# Create workspace root
mkdir -p /opt/rubbish/workspaces
chown rubbish:rubbish /opt/rubbish/workspaces

# Start NFS server
systemctl enable --now nfs-kernel-server
```

The bridge (`br0` at `172.16.0.1`) is already present from the VM networking setup. No
additional firewall rules are needed since NFS is only reachable over the bridge — VMs
cannot reach the host NFS port from outside the 172.16.0.0/24 subnet.

---

## VM Boot Changes

`/etc/init.d/rcS` inside the VM needs two additions:

```sh
# Mount NFS home directory
mkdir -p /home/user
mount -t nfs -o vers=3,proto=tcp,rsize=131072,wsize=131072,noatime,soft,timeo=30 \
  172.16.0.1:/opt/rubbish/workspaces/<team>/<user> /home/user

# Bind-mount the active worktree to /workspace
mkdir -p /workspace
mount --bind /home/user/<repo>/<worktree> /workspace
```

The `<team>`, `<user>`, `<repo>`, and `<worktree>` values are injected at VM boot time.
The mechanism for this is the Firecracker MMDS (instance metadata service) — the control
plane writes session metadata to MMDS before boot, and the rcS script reads it:

```sh
# Read session config from MMDS
MMDS_TOKEN=$(curl -s -X PUT "http://169.254.169.254/latest/api/token" \
  -H "X-metadata-token-ttl-seconds: 21600")
SESSION=$(curl -s -H "X-metadata-token: $MMDS_TOKEN" \
  http://169.254.169.254/latest/meta-data/session)

TEAM=$(echo $SESSION | grep -o '"team":"[^"]*"' | cut -d'"' -f4)
USER=$(echo $SESSION | grep -o '"user":"[^"]*"' | cut -d'"' -f4)
REPO=$(echo $SESSION | grep -o '"repo":"[^"]*"' | cut -d'"' -f4)
WORKTREE=$(echo $SESSION | grep -o '"worktree":"[^"]*"' | cut -d'"' -f4)
```

This avoids baking per-session config into the rootfs image and keeps the VM image generic.

---

## Control Plane Changes (Go)

The following new responsibilities land in the rubbish control plane:

### WorkspaceManager
- `EnsureRepo(team, user, repoURL)` — clones repo if not present; returns local path
- `CreateWorktree(team, user, repo, branch)` — runs `git worktree add`; returns path
- `RemoveWorktree(team, user, repo, branch)` — prunes worktree after retention window
- `FetchRepo(team, user, repo)` — serialised fetch with advisory lock per repo

### ExportManager
- `GrantReadOnly(team, user, vmIP)` — ensures `ro` export exists for this VM
- `GrantReadWrite(team, user, repo, worktree, vmIP)` — adds scoped `rw` export; calls
  `exportfs -r`
- `Revoke(vmIP)` — removes all exports for this VM on teardown; calls `exportfs -r`

### SessionMetadata (MMDS)
- Written to MMDS before VM start: `team`, `user`, `repo`, `worktree`, `writeEnabled`
- Read by `rcS` inside the VM to configure mounts

### RetentionReaper
- Background goroutine; runs every hour
- Queries worktrees with `last_shutdown_at` older than retention window
- Archives (tar + gzip to `/opt/rubbish/archive/<team>/<user>/`) or deletes

---

## Known Limitations and Future Work

**Disk space growth**: Deleted files inside the VM reduce NFS usage immediately (NFS
reflects the host filesystem directly), but build artifacts written during a session
(node_modules, compiled binaries) accumulate in worktrees until explicitly cleaned or the
worktree is pruned. Users should be aware that worktrees are not automatically cleaned on
VM teardown.

**Git operation serialisation**: The initial implementation serialises all `git fetch`
operations per repo with a host-side file lock. This is a blunt instrument — a proper
implementation would use a per-repo operation queue with timeout and cancellation. Deferred.

**Team-level shared repos**: The `/<team>/shared/` path segment is reserved but not
implemented. Team admins eventually need a way to add repos that are visible to all team
members without living under a specific user's home. Deferred.

**Write access granularity**: The current design grants write access at the worktree level
per VM IP. A finer-grained model (per-file or per-directory ACLs) is possible with NFS v4
ACLs but adds significant complexity. Deferred.

**NFS high availability**: A single NFS server is a single point of failure. For production
multi-host deployments, this needs to move to a distributed filesystem (CephFS, GlusterFS,
or a managed NFS service). Out of scope for self-hosted single-node deployment.

**Sparse file compaction**: Worktree directories can grow large. A compaction job that
runs `git gc` on the shared object store and removes unreferenced objects after worktrees
are pruned should be implemented alongside the RetentionReaper.
