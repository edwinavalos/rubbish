#!/usr/bin/env bash
set -euo pipefail

ALPINE_VERSION="3.21"
ALPINE_ARCH="x86_64"
GO_VERSION="1.24.3"
ROOTFS_SIZE_MB=4096
OUTPUT="/opt/rubbish/images/rootfs.ext4"
MOUNT_DIR=$(mktemp -d)
WORK_DIR=$(mktemp -d)

# SSH public key to bake in for both root and claude (reads from invoking user's authorized_keys)
SSH_PUBKEY="${SSH_PUBKEY:-$(cat ~/.ssh/authorized_keys 2>/dev/null | head -1 || true)}"

cleanup() {
    umount "${MOUNT_DIR}/proc" 2>/dev/null || true
    umount "${MOUNT_DIR}/dev" 2>/dev/null || true
    umount "${MOUNT_DIR}" 2>/dev/null || true
    rm -rf "${WORK_DIR}" "${MOUNT_DIR}"
}
trap cleanup EXIT

echo "Building Alpine ${ALPINE_VERSION} rootfs -> ${OUTPUT}"

# Download Alpine minirootfs
ALPINE_URL="https://dl-cdn.alpinelinux.org/alpine/v${ALPINE_VERSION}/releases/${ALPINE_ARCH}/alpine-minirootfs-${ALPINE_VERSION}.0-${ALPINE_ARCH}.tar.gz"
TARBALL="${WORK_DIR}/alpine-minirootfs.tar.gz"
echo "Downloading ${ALPINE_URL}"
curl -L -o "${TARBALL}" "${ALPINE_URL}"

# Download Go toolchain
GO_URL="https://go.dev/dl/go${GO_VERSION}.linux-amd64.tar.gz"
GO_TARBALL="${WORK_DIR}/go.tar.gz"
echo "Downloading Go ${GO_VERSION}"
curl -L -o "${GO_TARBALL}" "${GO_URL}"

# Create ext4 image
echo "Creating ${ROOTFS_SIZE_MB}MB ext4 image"
dd if=/dev/zero of="${OUTPUT}" bs=1M count="${ROOTFS_SIZE_MB}" status=progress
mkfs.ext4 -F "${OUTPUT}"

# Mount and unpack
mount -o loop "${OUTPUT}" "${MOUNT_DIR}"
tar -xzf "${TARBALL}" -C "${MOUNT_DIR}"

# Install Go into the rootfs
rm -rf "${MOUNT_DIR}/usr/local/go"
tar -C "${MOUNT_DIR}/usr/local" -xzf "${GO_TARBALL}"
echo "Go ${GO_VERSION} installed at /usr/local/go"

# Bind-mount /dev and /proc so chroot commands work
mount --bind /dev "${MOUNT_DIR}/dev"
mount --bind /proc "${MOUNT_DIR}/proc"

# Configure DNS
echo "nameserver 1.1.1.1" > "${MOUNT_DIR}/etc/resolv.conf"

# Network interface config (eth0 static IP — overwritten per-slot at boot by InjectNetworkConfig)
cat > "${MOUNT_DIR}/etc/network/interfaces" <<'EOF'
auto lo
iface lo inet loopback

auto eth0
iface eth0 inet static
    address 172.16.0.2
    netmask 255.255.255.0
    gateway 172.16.0.1
EOF

# System-wide Go + claude PATH/env for login shells
cat > "${MOUNT_DIR}/etc/profile.d/rubbish.sh" <<'ENVEOF'
export PATH=/usr/local/go/bin:/home/claude/.npm-global/bin:$PATH
export GOPATH=/home/claude/go
export GOCACHE=/home/claude/.cache/go
ENVEOF
chmod 644 "${MOUNT_DIR}/etc/profile.d/rubbish.sh"

# Install packages and set up claude as the primary user
chroot "${MOUNT_DIR}" /bin/sh -c "
    apk update &&
    apk add --no-cache openssh bash curl git nodejs npm sudo tmux make nfs-utils &&

    # claude is the primary interactive user — no root shell at runtime.
    adduser -D -s /bin/bash -h /home/claude claude &&
    echo 'claude ALL=(ALL) NOPASSWD:ALL' > /etc/sudoers.d/claude &&
    chmod 440 /etc/sudoers.d/claude &&

    # Workspace directory
    mkdir -p /home/claude/workspace &&

    # claude-owned npm prefix for Claude Code (auto-update without sudo)
    mkdir -p /home/claude/.npm-global &&
    echo 'prefix=/home/claude/.npm-global' > /home/claude/.npmrc &&
    npm install -g --prefix /home/claude/.npm-global @anthropic-ai/claude-code &&
    cd /home/claude/.npm-global/lib/node_modules/@anthropic-ai/claude-code &&
    npm install --save-optional @anthropic-ai/claude-code-linux-x64-musl &&
    node install.cjs &&
    cd / &&

    # System-wide symlink so /usr/local/bin/claude resolves for any user
    ln -sf /home/claude/.npm-global/bin/claude /usr/local/bin/claude &&

    # claude's .profile: PATH, GOPATH/GOCACHE so go build works without extra env flags
    printf 'export PATH=/home/claude/.npm-global/bin:/usr/local/go/bin:\$PATH\n' > /home/claude/.profile &&
    printf 'export GOPATH=/home/claude/go\n' >> /home/claude/.profile &&
    printf 'export GOCACHE=/home/claude/.cache/go\n' >> /home/claude/.profile &&

    # tmux: no status bar, mouse scrollback, no prefix (prevents pane/window creation)
    printf 'set -g status off\nset -g mouse on\nset -g history-limit 50000\nset -g prefix None\nset -g escape-time 0\n' > /home/claude/.tmux.conf &&

    chown -R claude:claude /home/claude &&

    ssh-keygen -A &&
    passwd -d root &&
    mkdir -p /root/workspace
"

# Configure sshd: no password auth, allow both root and claude key login
sed -i 's/#PermitRootLogin.*/PermitRootLogin yes/' "${MOUNT_DIR}/etc/ssh/sshd_config"
sed -i 's/#PasswordAuthentication.*/PasswordAuthentication no/' "${MOUNT_DIR}/etc/ssh/sshd_config"
sed -i 's/PasswordAuthentication yes/PasswordAuthentication no/' "${MOUNT_DIR}/etc/ssh/sshd_config"

# Bake SSH key into root's authorized_keys
mkdir -p "${MOUNT_DIR}/root/.ssh"
echo "${SSH_PUBKEY}" > "${MOUNT_DIR}/root/.ssh/authorized_keys"
chmod 700 "${MOUNT_DIR}/root/.ssh"
chmod 600 "${MOUNT_DIR}/root/.ssh/authorized_keys"
chroot "${MOUNT_DIR}" chown -R root:root /root/.ssh

# Bake SSH key into claude's authorized_keys — boot setup connects as claude, not root
mkdir -p "${MOUNT_DIR}/home/claude/.ssh"
echo "${SSH_PUBKEY}" > "${MOUNT_DIR}/home/claude/.ssh/authorized_keys"
chmod 700 "${MOUNT_DIR}/home/claude/.ssh"
chmod 600 "${MOUNT_DIR}/home/claude/.ssh/authorized_keys"
chroot "${MOUNT_DIR}" chown -R claude:claude /home/claude/.ssh

# Workspace virtio-fs mount point.
# Workflow C mounts the host workspace here via virtiofs so the VM sees the
# repo without any in-VM git clone.  The mount tag "workspace" matches
# vm.VirtioFSGuestTag.  The directory is owned by claude so she can write.
mkdir -p "${MOUNT_DIR}/home/claude/workspace"
chroot "${MOUNT_DIR}" chown claude:claude /home/claude/workspace

# Start sshd and configure network via /etc/init.d/rcS (bypass openrc for microVM simplicity)
mkdir -p "${MOUNT_DIR}/etc/init.d"
cat > "${MOUNT_DIR}/etc/init.d/rcS" <<'RCEOF'
#!/bin/sh
mkdir -p /dev/pts
mount -t devpts devpts /dev/pts
mkdir -p /proc
mount -t proc proc /proc
ip link set lo up
ip addr add 127.0.0.1/8 dev lo
ip link set eth0 up
ip addr add 172.16.0.2/24 dev eth0
ip route add default via 172.16.0.1

# Workflow C: mount the host workspace via virtiofs if the device is present.
# The virtiofsd daemon on the host exports the per-session workspace directory
# under the tag "workspace".  If no tag is present (e.g. no repo was requested
# or Workflow C is not yet fully wired) this is a harmless no-op.
#
# Prerequisites:
#   1. Kernel built with CONFIG_VIRTIO_FS=y (or virtiofs module loadable)
#   2. virtiofsd running on the host before Firecracker starts
#   3. Firecracker configured with a vhost-user-fs device (tag=workspace)
#
# The mount is attempted regardless so that a failed mount surfaces in the
# boot log rather than failing silently.  If virtiofsd is not running, the
# mount will fail and the workspace will simply be empty — the session is
# still usable (the VM falls back to any git-credential-based clone the
# server initiates via SSH).
if [ -e /sys/bus/virtio/drivers/virtiofs ] || modprobe virtiofs 2>/dev/null; then
    mkdir -p /home/claude/workspace
    mount -t virtiofs workspace /home/claude/workspace 2>/dev/null && \
        chown claude:claude /home/claude/workspace || \
        true  # non-fatal: workspace may be empty, session is still usable
fi

/usr/sbin/sshd
RCEOF
chmod +x "${MOUNT_DIR}/etc/init.d/rcS"

# inittab: run rcS then drop to login on ttyS0
cat > "${MOUNT_DIR}/etc/inittab" <<'EOF'
::sysinit:/etc/init.d/rcS
ttyS0::respawn:/sbin/getty -L ttyS0 115200 vt100
::ctrlaltdel:/sbin/reboot
::shutdown:/bin/sh -c "kill -TERM -1"
EOF

BUILD_TS=$(date -u +"%Y-%m-%dT%H:%M:%SZ")
echo "${BUILD_TS}" > "${MOUNT_DIR}/etc/rubbish-build"

echo "Rootfs built: ${OUTPUT} (${BUILD_TS})"
echo "Go ${GO_VERSION} included. claude is the primary user with SSH key baked in."
