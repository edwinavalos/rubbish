#!/usr/bin/env bash
set -euo pipefail

# Device mapper thin pool setup for rubbish.
# Idempotent — safe to run multiple times (on first install and on every reboot).
# Must run as root.

if [[ $EUID -ne 0 ]]; then
  echo "ERROR: must run as root" >&2
  exit 1
fi

DM_DIR="/opt/rubbish/dm"
DATA_IMG="$DM_DIR/pool-data.img"
META_IMG="$DM_DIR/pool-meta.img"
ROOTFS="/opt/rubbish/images/rootfs.ext4"
NEXT_ID_FILE="$DM_DIR/next-volume-id"

# ---------------------------------------------------------------------------
# 1. Dependencies
# ---------------------------------------------------------------------------
echo "==> Checking dependencies..."
MISSING=()
for cmd in dmsetup thin_check losetup blockdev dd; do
  command -v "$cmd" &>/dev/null || MISSING+=("$cmd")
done

if [[ ${#MISSING[@]} -gt 0 ]]; then
  echo "    Installing missing packages (thin-provisioning-tools, dmsetup)..."
  apt-get install -y thin-provisioning-tools dmsetup >/dev/null
else
  echo "    All dependencies present."
fi

# ---------------------------------------------------------------------------
# 2. Directories
# ---------------------------------------------------------------------------
echo "==> Ensuring $DM_DIR exists..."
mkdir -p "$DM_DIR"

# ---------------------------------------------------------------------------
# 3. Sparse backing files
# ---------------------------------------------------------------------------
if [[ ! -f "$DATA_IMG" ]]; then
  echo "==> Creating 100GB sparse pool-data.img..."
  truncate -s 100G "$DATA_IMG"
else
  echo "==> pool-data.img already exists, skipping."
fi

if [[ ! -f "$META_IMG" ]]; then
  echo "==> Creating 1GB sparse pool-meta.img..."
  truncate -s 1G "$META_IMG"
else
  echo "==> pool-meta.img already exists, skipping."
fi

# ---------------------------------------------------------------------------
# 4. Loop devices
# ---------------------------------------------------------------------------
echo "==> Attaching loop devices..."

attach_loop() {
  local img="$1"
  existing=$(losetup -j "$img" --output NAME --noheadings 2>/dev/null | head -1)
  if [[ -n "$existing" ]]; then
    echo "    $img already attached at $existing" >&2
    echo "$existing"
  else
    dev=$(losetup -f --show "$img")
    echo "    Attached $img at $dev" >&2
    echo "$dev"
  fi
}

POOL_DATA=$(attach_loop "$DATA_IMG")
POOL_META=$(attach_loop "$META_IMG")

# ---------------------------------------------------------------------------
# 5. Thin pool
# ---------------------------------------------------------------------------
POOL_ALREADY_EXISTS=false

if [[ -e /dev/mapper/rubbish-pool ]]; then
  echo "==> /dev/mapper/rubbish-pool already exists, skipping pool creation."
  POOL_ALREADY_EXISTS=true
else
  echo "==> Creating thin pool..."

  # Metadata linear device
  META_SECTORS=$(blockdev --getsz "$POOL_META")
  dmsetup create rubbish-pool-meta \
    --table "0 $META_SECTORS linear $POOL_META 0"
  echo "    Created rubbish-pool-meta ($META_SECTORS sectors)"

  # Zero out metadata header so dm thin-pool starts with a clean slate
  dd if=/dev/zero of=/dev/mapper/rubbish-pool-meta bs=4k count=1 status=none conv=fsync

  # Thin pool: 256-sector (128k) block size, 0 low-water-mark
  DATA_SECTORS=$(blockdev --getsz "$POOL_DATA")
  dmsetup create rubbish-pool \
    --table "0 $DATA_SECTORS thin-pool /dev/mapper/rubbish-pool-meta $POOL_DATA 256 0"
  echo "    Created rubbish-pool ($DATA_SECTORS sectors)"
fi

# If pool-meta is gone but pool somehow isn't (shouldn't happen but guard it):
if [[ ! -e /dev/mapper/rubbish-pool-meta ]] && $POOL_ALREADY_EXISTS; then
  echo "WARNING: rubbish-pool exists but rubbish-pool-meta does not — state may be inconsistent." >&2
fi

# ---------------------------------------------------------------------------
# 6. Import base rootfs
# ---------------------------------------------------------------------------
if [[ -e /dev/mapper/rubbish-base-ro ]]; then
  echo "==> /dev/mapper/rubbish-base-ro already exists, skipping base import."
else
  echo "==> Importing base rootfs from $ROOTFS..."

  if [[ ! -f "$ROOTFS" ]]; then
    echo "ERROR: base rootfs not found at $ROOTFS" >&2
    exit 1
  fi

  BASE_BYTES=$(stat -c%s "$ROOTFS")
  # Round up to 512-byte sector boundary
  BASE_SECTORS=$(( (BASE_BYTES + 511) / 512 ))

  echo "    Base image: $BASE_BYTES bytes / $BASE_SECTORS sectors"

  # Volume 0: raw writable import target
  dmsetup message /dev/mapper/rubbish-pool 0 "create_thin 0"
  dmsetup create rubbish-base \
    --table "0 $BASE_SECTORS thin /dev/mapper/rubbish-pool 0"

  echo "    Copying rootfs into thin volume 0 (this may take a minute)..."
  dd if="$ROOTFS" of=/dev/mapper/rubbish-base bs=4M status=progress conv=fsync
  dmsetup remove rubbish-base

  # Volume 1: read-only snapshot of volume 0 — the permanent base
  dmsetup message /dev/mapper/rubbish-pool 0 "create_snap 1 0"
  dmsetup create rubbish-base-ro \
    --table "0 $BASE_SECTORS thin /dev/mapper/rubbish-pool 1" \
    --readonly
  echo "    Created rubbish-base-ro (volume 1, read-only)"

  # Remove raw volume 0 — no longer needed
  dmsetup message /dev/mapper/rubbish-pool 0 "delete 0"
  echo "    Deleted raw import volume 0"
fi

# ---------------------------------------------------------------------------
# 7. Next volume ID
# ---------------------------------------------------------------------------
if [[ ! -f "$NEXT_ID_FILE" ]]; then
  echo "==> Initializing next-volume-id to 2..."
  echo "2" > "$NEXT_ID_FILE"
else
  echo "==> next-volume-id already set to $(cat $NEXT_ID_FILE), skipping."
fi

echo ""
echo "==> Storage setup complete."
echo "    Pool device:    /dev/mapper/rubbish-pool"
echo "    Base (RO):      /dev/mapper/rubbish-base-ro"
echo "    Next volume ID: $(cat $NEXT_ID_FILE)"
