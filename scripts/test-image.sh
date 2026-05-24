#!/usr/bin/env bash
# Mount the base rootfs image and run sanity checks inside a chroot.
# Must be run as root on the test box (192.168.1.35).
# Usage:
#   ./scripts/test-image.sh              # sanity check only
#   ./scripts/test-image.sh fix          # also run claude postinstall
set -euo pipefail

IMAGE="${IMAGE:-/opt/rubbish/images/rootfs.ext4}"
MOUNT_DIR=$(mktemp -d)

cleanup() {
    umount "${MOUNT_DIR}/proc" 2>/dev/null || true
    umount "${MOUNT_DIR}/dev"  2>/dev/null || true
    umount "${MOUNT_DIR}"      2>/dev/null || true
    rmdir  "${MOUNT_DIR}"      2>/dev/null || true
}
trap cleanup EXIT

echo "Mounting ${IMAGE} -> ${MOUNT_DIR}"
mount -o loop "${IMAGE}" "${MOUNT_DIR}"
mount --bind /dev  "${MOUNT_DIR}/dev"
mount --bind /proc "${MOUNT_DIR}/proc"

if [[ "${1:-}" == "fix" ]]; then
    echo "--- Running claude postinstall ---"
    chroot "${MOUNT_DIR}" /bin/sh -c \
        'node $(npm root -g)/@anthropic-ai/claude-code/install.cjs'
fi

echo "--- Sanity checks ---"
chroot "${MOUNT_DIR}" /bin/sh -c '
    set -e
    echo "node:    $(node --version)"
    echo "npm:     $(npm --version)"
    echo "claude:  $(claude --version)"
    echo "sshd:    $(sshd -V 2>&1 | head -1)"
    echo "git:     $(git --version)"
'
echo "--- All checks passed ---"
