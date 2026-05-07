#!/usr/bin/env bash
set -euo pipefail

ALPINE_VERSION="3.21"
ALPINE_ARCH="x86_64"
ROOTFS_SIZE_MB=4096
OUTPUT="/opt/rubbish/images/rootfs.ext4"
MOUNT_DIR=$(mktemp -d)
WORK_DIR=$(mktemp -d)

# SSH public key to bake in (reads from invoking user's authorized_keys)
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

# Create ext4 image
echo "Creating ${ROOTFS_SIZE_MB}MB ext4 image"
dd if=/dev/zero of="${OUTPUT}" bs=1M count="${ROOTFS_SIZE_MB}" status=progress
mkfs.ext4 -F "${OUTPUT}"

# Mount and unpack
mount -o loop "${OUTPUT}" "${MOUNT_DIR}"
tar -xzf "${TARBALL}" -C "${MOUNT_DIR}"

# Bind-mount /dev and /proc so chroot commands work
mount --bind /dev "${MOUNT_DIR}/dev"
mount --bind /proc "${MOUNT_DIR}/proc"

# Configure DNS
echo "nameserver 1.1.1.1" > "${MOUNT_DIR}/etc/resolv.conf"

# Network interface config (eth0 static IP)
cat > "${MOUNT_DIR}/etc/network/interfaces" <<'EOF'
auto lo
iface lo inet loopback

auto eth0
iface eth0 inet static
    address 172.16.0.2
    netmask 255.255.255.0
    gateway 172.16.0.1
EOF

# Install packages and set up claude user inside chroot
chroot "${MOUNT_DIR}" /bin/sh -c "
    apk update &&
    apk add --no-cache openssh bash curl git nodejs npm sudo &&

    # Create claude user (session setup does this too, but baking it in saves the apk install at boot)
    adduser -D -s /bin/bash -h /home/claude claude &&
    echo 'claude ALL=(ALL) NOPASSWD:ALL' > /etc/sudoers.d/claude &&
    chmod 440 /etc/sudoers.d/claude &&

    # Install claude-code into a claude-owned npm prefix so the user can auto-update
    mkdir -p /home/claude/.npm-global &&
    echo 'prefix=/home/claude/.npm-global' > /home/claude/.npmrc &&
    npm install -g --prefix /home/claude/.npm-global @anthropic-ai/claude-code &&
    cd /home/claude/.npm-global/lib/node_modules/@anthropic-ai/claude-code &&
    npm install --save-optional @anthropic-ai/claude-code-linux-x64-musl &&
    node install.cjs &&
    cd / &&

    # System-wide symlink so /usr/local/bin/claude resolves for root and other users
    ln -sf /home/claude/.npm-global/bin/claude /usr/local/bin/claude &&

    # Seed .profile with PATH (session setup appends credentials on top of this)
    printf 'export PATH=/home/claude/.npm-global/bin:\$PATH\n' > /home/claude/.profile &&

    chown -R claude:claude /home/claude &&

    ssh-keygen -A &&
    passwd -d root &&
    mkdir -p /root/workspace
"

# Configure sshd: allow root login, no password auth
sed -i 's/#PermitRootLogin.*/PermitRootLogin yes/' "${MOUNT_DIR}/etc/ssh/sshd_config"
sed -i 's/#PasswordAuthentication.*/PasswordAuthentication no/' "${MOUNT_DIR}/etc/ssh/sshd_config"
sed -i 's/PasswordAuthentication yes/PasswordAuthentication no/' "${MOUNT_DIR}/etc/ssh/sshd_config"

# Install SSH authorized key
mkdir -p "${MOUNT_DIR}/root/.ssh"
echo "${SSH_PUBKEY}" > "${MOUNT_DIR}/root/.ssh/authorized_keys"
chmod 700 "${MOUNT_DIR}/root/.ssh"
chmod 600 "${MOUNT_DIR}/root/.ssh/authorized_keys"
chroot "${MOUNT_DIR}" chown -R root:root /root/.ssh

# Start sshd and configure network directly via /etc/init.d/rcS (bypass openrc)
mkdir -p "${MOUNT_DIR}/etc/init.d"
# Alpine minirootfs doesn't have openrc configured for boot; use a simple rcS script
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
