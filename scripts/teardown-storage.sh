#!/usr/bin/env bash
set -euo pipefail

# Tears down all rubbish device mapper devices and loop devices.
# For debugging and cleanup only — NOT for normal shutdown.
# Must run as root.

if [[ $EUID -ne 0 ]]; then
  echo "ERROR: must run as root" >&2
  exit 1
fi

DATA_IMG="/opt/rubbish/dm/pool-data.img"
META_IMG="/opt/rubbish/dm/pool-meta.img"

# ---------------------------------------------------------------------------
# Session snapshot devices
# ---------------------------------------------------------------------------
mapfile -t SESSIONS < <(dmsetup ls --target thin 2>/dev/null | awk '{print $1}' | grep '^rubbish-session-' || true)
if [[ ${#SESSIONS[@]} -gt 0 ]]; then
  for dev in "${SESSIONS[@]}"; do
    echo "==> Removing session device: $dev"
    dmsetup remove "$dev" || echo "    WARNING: failed to remove $dev"
  done
else
  echo "==> No rubbish-session-* devices found."
fi

# ---------------------------------------------------------------------------
# Base read-only volume
# ---------------------------------------------------------------------------
if [[ -e /dev/mapper/rubbish-base-ro ]]; then
  echo "==> Removing rubbish-base-ro..."
  dmsetup remove rubbish-base-ro
else
  echo "==> rubbish-base-ro not present, skipping."
fi

# ---------------------------------------------------------------------------
# Thin pool
# ---------------------------------------------------------------------------
if [[ -e /dev/mapper/rubbish-pool ]]; then
  echo "==> Removing rubbish-pool..."
  dmsetup remove rubbish-pool
else
  echo "==> rubbish-pool not present, skipping."
fi

# ---------------------------------------------------------------------------
# Metadata linear device
# ---------------------------------------------------------------------------
if [[ -e /dev/mapper/rubbish-pool-meta ]]; then
  echo "==> Removing rubbish-pool-meta..."
  dmsetup remove rubbish-pool-meta
else
  echo "==> rubbish-pool-meta not present, skipping."
fi

# ---------------------------------------------------------------------------
# Loop devices
# ---------------------------------------------------------------------------
for img in "$DATA_IMG" "$META_IMG"; do
  mapfile -t LOOPS < <(losetup -j "$img" --output NAME --noheadings 2>/dev/null || true)
  if [[ ${#LOOPS[@]} -gt 0 ]]; then
    for loop in "${LOOPS[@]}"; do
      echo "==> Detaching loop device $loop ($img)..."
      losetup -d "$loop"
    done
  else
    echo "==> No loop device attached for $img, skipping."
  fi
done

echo ""
echo "==> Teardown complete."
