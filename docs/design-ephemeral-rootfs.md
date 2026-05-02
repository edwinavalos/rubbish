# Design: Ephemeral Per-Session Rootfs via Device Mapper Snapshots

## Status
Design — not yet implemented.

## Problem

The current PoC boots all VMs from a single shared writable ext4 image
(`/opt/rubbish/images/rootfs.ext4`). This has two critical problems:

1. **Concurrent sessions corrupt each other's disk state.** Two VMs writing to the same
   backing file simultaneously will corrupt the filesystem.
2. **Session state bleeds across launches.** Anything an agent writes (installed packages,
   modified configs, Claude Code auth tokens, build artifacts) persists into the next
   session because the same image is reused.

Every VM session must boot from an isolated copy of the base image, and that copy must be
discarded on teardown.

## Goals

- Each VM session gets its own fully writable block device at launch
- The block device is a copy-on-write snapshot of a shared read-only base image — only
  blocks the session actually writes consume additional disk space
- The base image is never modified by a running VM
- Session disk is automatically cleaned up on VM teardown
- Base image can be updated (e.g. new Claude Code version) without affecting running sessions
- VM sees a single unified block device — no layering logic required inside the VM

## Non-Goals

- Live migration of VM disk state between hosts (out of scope)
- Snapshotting a running session for later resume (separate feature — see snapshotting
  design when written)
- Per-session disk quotas (deferred)

---

## Chosen Approach: Device Mapper Thin Provisioning

Linux device mapper provides a `thin` target that implements copy-on-write block-level
snapshots backed by a shared thin pool. This is the same mechanism used by Docker's
devicemapper storage driver and firecracker-containerd.

### How it works

1. A **thin pool** is created on the host — a large sparse backing file managed by device
   mapper that acts as the shared allocation pool for all snapshot volumes.
2. The base rootfs image is imported into the pool as a **read-only thin volume**.
3. At session launch, a **thin snapshot** of the base volume is created instantly (zero
   copy). The snapshot starts empty — it shares all blocks with the base.
4. The snapshot is presented to Firecracker as a virtio-blk device. The VM sees a normal
   writable disk.
5. As the VM writes blocks, those blocks are allocated from the pool and written only to
   the snapshot. The base volume is never touched.
6. At session teardown, the snapshot volume is deleted. The pool reclaims the allocated
   blocks.

### Why this fits rubbish better than OverlayFS

- **VM is unaware of layering.** No changes to `rcS`, no overlayfs assembly at boot. The
  VM boots exactly as it does today — one disk, normal ext4.
- **Dynamic space allocation.** Snapshots grow as blocks are written, not at creation time.
  No need to pre-size a per-session upper layer or risk running out of space mid-session.
- **Block-level CoW.** Lower overhead than filesystem-level CoW (overlayfs) for write-heavy
  workloads — each write touches one block in the pool.
- **Clean teardown.** Deleting a thin volume is instantaneous and completely reclaims all
  space allocated to that session. No sparse file compaction needed.

---

## Host Setup

### Dependencies

```bash
apt install -y thin-provisioning-tools dmsetup
```

`thin-provisioning-tools` provides `thin_check`, `thin_repair`, and `thin_dump` for pool
integrity checks and debugging.

### Thin pool creation

The pool consists of two components:
- **Data device**: stores the actual block data for all volumes (base + snapshots)
- **Metadata device**: stores the thin pool's internal allocation tables

Both are backed by sparse files, so they only consume real disk space proportional to what
is actually written.

```bash
# Create sparse backing files
# Data: 100GB logical, grows as sessions write
truncate -s 100G /opt/rubbish/dm/pool-data.img
# Metadata: 1GB is sufficient for thousands of snapshots
truncate -s 1G /opt/rubbish/dm/pool-meta.img

# Set up loop devices
POOL_DATA=$(losetup -f --show /opt/rubbish/dm/pool-data.img)
POOL_META=$(losetup -f --show /opt/rubbish/dm/pool-meta.img)

# Format the metadata device
thin_metadata_size --block-size=128k --pool-size=100G --max-thins=10000 --unit=b
dmsetup create rubbish-pool-meta --table "0 $(blockdev --getsz $POOL_META) linear $POOL_META 0"
thin_metadata_init --device /dev/mapper/rubbish-pool-meta

# Create the thin pool
# Parameters: metadata dev, data dev, data block size (128k = 256 sectors), low water mark
POOL_DATA_SECTORS=$(blockdev --getsz $POOL_DATA)
dmsetup create rubbish-pool --table \
  "0 $POOL_DATA_SECTORS thin-pool /dev/mapper/rubbish-pool-meta $POOL_DATA 256 0"
```

This is a one-time setup, run by `scripts/setup-storage.sh` (to be written alongside
this implementation).

### Importing the base image

The base rootfs (`/opt/rubbish/images/rootfs.ext4`) is imported into the pool as thin
volume ID 0:

```bash
# Allocate thin volume 0 (the base)
BASE_SECTORS=$(stat -c%s /opt/rubbish/images/rootfs.ext4 | awk '{print $1/512}')
dmsetup message /dev/mapper/rubbish-pool 0 "create_thin 0"
dmsetup create rubbish-base --table "0 $BASE_SECTORS thin /dev/mapper/rubbish-pool 0"

# Write the base image into the thin volume
dd if=/opt/rubbish/images/rootfs.ext4 of=/dev/mapper/rubbish-base bs=4M status=progress

# Deactivate and recreate as read-only snapshot (volume ID 1 = read-only base)
dmsetup remove rubbish-base
dmsetup message /dev/mapper/rubbish-pool 0 "create_snap 1 0"
dmsetup create rubbish-base-ro --table "0 $BASE_SECTORS thin /dev/mapper/rubbish-pool 1" --readonly
```

Volume 1 is the permanent read-only base. Volume 0 (the raw import) can be removed once
the snapshot is created. All session snapshots are taken from volume 1.

### Per-session snapshot lifecycle

**On session launch** (called by `WorkspaceManager`):

```bash
SESSION_ID="<uuid>"
VOLUME_ID="<next available int>"  # tracked by control plane

# Create snapshot of the base (instant, zero copy)
dmsetup message /dev/mapper/rubbish-pool 0 "create_snap $VOLUME_ID 1"

# Activate the snapshot as a writable block device
BASE_SECTORS=<size of base>
dmsetup create rubbish-session-$SESSION_ID \
  --table "0 $BASE_SECTORS thin /dev/mapper/rubbish-pool $VOLUME_ID"

# /dev/mapper/rubbish-session-$SESSION_ID is now ready to give to Firecracker
```

**On session teardown:**

```bash
# Deactivate the device
dmsetup remove rubbish-session-$SESSION_ID

# Delete the thin volume — reclaims all allocated blocks immediately
dmsetup message /dev/mapper/rubbish-pool 0 "delete $VOLUME_ID"
```

---

## Control Plane Changes (Go)

### SnapshotManager

New component in `internal/storage/`:

```go
type SnapshotManager struct {
    poolDevice  string  // e.g. "/dev/mapper/rubbish-pool"
    baseVolumeID int    // always 1
    mu          sync.Mutex
    nextVolumeID int    // monotonically increasing, persisted to disk
}

// CreateSnapshot creates a thin snapshot and returns the /dev/mapper path.
func (s *SnapshotManager) CreateSnapshot(sessionID string) (string, error)

// DeleteSnapshot deactivates and removes the thin volume.
func (s *SnapshotManager) DeleteSnapshot(sessionID string) error

// BaseSize returns the sector count of the base volume (needed for dm table entries).
func (s *SnapshotManager) BaseSize() (int64, error)
```

`CreateSnapshot` and `DeleteSnapshot` shell out to `dmsetup`. Volume ID assignment is
protected by a mutex; the next available ID is persisted to
`/opt/rubbish/dm/next-volume-id` so it survives control plane restarts.

### Launcher changes

`internal/vm/launcher.go` currently hardcodes `RootfsPath` as the drive path. This
changes to accept a block device path from `SnapshotManager`:

```go
func Launch(ctx context.Context, snapshotDevice string) (*VM, error)
```

The drive config changes from:
```go
PathOnHost: firecracker.String(RootfsPath),
IsReadOnly: firecracker.Bool(false),
```
to:
```go
PathOnHost: firecracker.String(snapshotDevice),
IsReadOnly: firecracker.Bool(false),
```

The `main.go` flow becomes:

```go
snapMgr := storage.NewSnapshotManager(...)
device, err := snapMgr.CreateSnapshot(sessionID)
// ...
v, err := vm.Launch(ctx, device)
defer func() {
    v.Stop(ctx)
    snapMgr.DeleteSnapshot(sessionID)
}()
```

---

## Updating the Base Image

When a new version of Claude Code is released or the base Alpine image needs updating:

1. Build a new `rootfs.ext4` via `scripts/build-rootfs.sh` (unchanged)
2. Run `scripts/update-base-image.sh` (to be written):
   - Imports the new ext4 into the pool as a new thin volume (e.g. volume ID N)
   - Creates a new read-only snapshot from it (volume ID N+1)
   - Updates the `baseVolumeID` in `SnapshotManager` config
   - Removes the old base volume from the pool
3. Running sessions are unaffected — they snapshot from the old base and continue normally
4. New sessions from this point use the new base

---

## Persistence Across Host Reboots

Device mapper mappings are not persistent across reboots by default. On host restart:

1. The loop devices (`pool-data.img`, `pool-meta.img`) must be re-attached
2. The thin pool must be re-created with the same parameters
3. The base read-only volume must be re-activated

This is handled by a systemd unit `rubbish-storage.service` (to be written) that runs
`scripts/setup-storage.sh` at host startup before the rubbish control plane starts.

Any session snapshot volumes that existed before the reboot are orphaned (their VMs are
gone). The control plane detects orphaned volume IDs on startup and cleans them up.

---

## Storage Sizing Guidelines

| Component | Recommended size | Notes |
|---|---|---|
| Pool data backing file | 100GB+ | Sparse — only consumed blocks use real disk |
| Pool metadata backing file | 1GB | Sufficient for ~50,000 thin volumes |
| Base rootfs image | ~2GB | Alpine + Node.js + Claude Code |
| Per-session delta (typical) | 500MB–2GB | npm installs, build artifacts, git checkouts |
| Max concurrent sessions | ~20–30 on 100GB | Assuming 2–3GB delta per session |

The pool data file is sparse, so creating a 100GB pool on a host with 50GB free disk is
fine as long as the sum of actual session writes stays within available space. The control
plane should monitor pool utilisation and refuse new session launches when the pool is above
a configurable threshold (default: 80%).

---

## Known Limitations and Future Work

**Volume ID exhaustion**: Volume IDs are monotonically increasing integers. At 10,000
sessions per day this would exhaust a 32-bit ID space in ~1,000 years. In practice, IDs
can be reused after a volume is deleted; implement ID recycling in `SnapshotManager` when
needed.

**Pool corruption recovery**: If the metadata device is corrupted, all volumes in the pool
are inaccessible. `thin_repair` can recover most cases but this is a serious operational
risk. For production multi-host deployments, pool metadata should be on redundant storage.
For single-host self-hosted deployments, regular metadata backups via `thin_dump` are
sufficient.

**No disk quota per session**: A runaway agent can fill the pool and affect all concurrent
sessions. Device mapper thin provisioning supports per-volume quotas via the `thin` target's
`error_if_no_space` mode. Implement session quotas when this becomes an operational problem.

**Host reboot during active session**: Any running VM is killed on host reboot. Session
snapshots are orphaned and cleaned up on restart. There is no live migration or
checkpoint/restore in the initial implementation. This is acceptable for a self-hosted
single-node deployment where the user controls the host.
