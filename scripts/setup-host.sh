#!/usr/bin/env bash
# setup-host.sh — idempotent host configuration for rubbish
#
# Run as root on the Linux box that will host Firecracker VMs.
# Safe to re-run; each step checks before acting.
#
# Usage:
#   sudo ./scripts/setup-host.sh [--dev-seed-dir /path/to/dev-seed]
#
# What this does:
#   1.  Install system packages (nfs-kernel-server, firecracker, etc.)
#   2.  Create rubbish + firecracker OS users
#   3.  Install helper scripts (rubbish-tap-up/down, rubbish-deploy, rubbish-setup-storage)
#   4.  Configure sudoers
#   5.  Create /opt/rubbish directory tree
#   6.  Configure the VM bridge (br0) with STP disabled  ← critical for fast SSH
#   7.  Configure NAT + IP forwarding
#   8.  Configure NFS exports for VM workspaces and Claude memory
#   9.  Install and enable systemd services
#   10. Optionally populate /opt/rubbish/dev-seed from a local directory

set -euo pipefail

# ---------------------------------------------------------------------------
# Args / defaults
# ---------------------------------------------------------------------------
DEV_SEED_SRC=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    --dev-seed-dir) DEV_SEED_SRC="$2"; shift 2 ;;
    *) echo "unknown arg: $1" >&2; exit 1 ;;
  esac
done

if [[ $EUID -ne 0 ]]; then
  echo "ERROR: must run as root" >&2
  exit 1
fi

BRIDGE="br0"
BRIDGE_IP="172.16.0.1/24"
VM_SUBNET="172.16.0.0/24"
OPT="/opt/rubbish"
SCRIPTS_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# ---------------------------------------------------------------------------
step() { echo; echo "==> $*"; }
# ---------------------------------------------------------------------------

# ---------------------------------------------------------------------------
# 1. System packages
# ---------------------------------------------------------------------------
step "Installing system packages"
apt-get update -qq
apt-get install -y \
  nfs-kernel-server \
  thin-provisioning-tools \
  dmsetup \
  curl \
  iptables \
  iproute2 \
  git \
  >/dev/null
echo "    packages ok"

# ---------------------------------------------------------------------------
# 2. OS users
# ---------------------------------------------------------------------------
step "Creating OS users"

if ! id rubbish &>/dev/null; then
  useradd --system --no-create-home --shell /usr/sbin/nologin \
    --groups disk,kvm rubbish
  echo "    created user: rubbish"
else
  # Ensure group memberships even if user already existed
  usermod -aG disk,kvm rubbish 2>/dev/null || true
  echo "    user rubbish already exists"
fi

if ! id firecracker &>/dev/null; then
  useradd --system --no-create-home --shell /usr/sbin/nologin \
    --groups kvm firecracker
  echo "    created user: firecracker"
else
  usermod -aG kvm firecracker 2>/dev/null || true
  echo "    user firecracker already exists"
fi

# ---------------------------------------------------------------------------
# 3. Helper scripts
# ---------------------------------------------------------------------------
step "Installing helper scripts"

install -m 0755 /dev/stdin /usr/local/bin/rubbish-tap-up << 'SCRIPT'
#!/bin/bash
set -euo pipefail
TAP="$1"
if [[ ! "$TAP" =~ ^tap[0-9]+$ ]]; then
  echo "invalid tap name: $TAP" >&2; exit 1
fi
# Clean up any leftover tap from a crashed session before creating a new one.
if ip link show "$TAP" &>/dev/null; then
  ip link set "$TAP" down 2>/dev/null || true
  ip tuntap del dev "$TAP" mode tap 2>/dev/null || true
fi
ip tuntap add dev "$TAP" mode tap user rubbish
ip link set "$TAP" master br0
ip link set "$TAP" up
SCRIPT

install -m 0755 /dev/stdin /usr/local/bin/rubbish-tap-down << 'SCRIPT'
#!/bin/bash
set -euo pipefail
TAP="$1"
if [[ ! "$TAP" =~ ^tap[0-9]+$ ]]; then
  echo "invalid tap name: $TAP" >&2; exit 1
fi
ip link set "$TAP" down 2>/dev/null || true
ip tuntap del dev "$TAP" mode tap 2>/dev/null || true
SCRIPT

install -m 0755 /dev/stdin /usr/local/bin/rubbish-deploy << 'SCRIPT'
#!/bin/bash
set -euo pipefail

BINARY=${1:-}
if [[ -z "$BINARY" ]]; then
  echo "usage: rubbish-deploy <path-to-new-binary>" >&2
  exit 1
fi
if [[ ! -f "$BINARY" ]]; then
  echo "error: $BINARY not found" >&2
  exit 1
fi

INSTALL=/usr/local/bin/rubbish-poc
PREV=/usr/local/bin/rubbish-poc.prev
HEALTH_URL=http://localhost:8080/api/sessions
TIMEOUT=15

wait_healthy() {
  local label=$1
  for i in $(seq 1 $TIMEOUT); do
    if curl -sf "$HEALTH_URL" > /dev/null 2>&1; then
      echo "[$label] healthy after ${i}s"
      return 0
    fi
    sleep 1
  done
  echo "[$label] did not become healthy within ${TIMEOUT}s" >&2
  return 1
}

echo "==> backing up current binary to $PREV"
cp "$INSTALL" "$PREV"

echo "==> installing new binary"
cp "$BINARY" "${INSTALL}.new"
chmod +x "${INSTALL}.new"
mv "${INSTALL}.new" "$INSTALL"

echo "==> restarting rubbish-poc"
systemctl restart rubbish-poc

if wait_healthy "new binary"; then
  echo "==> deploy successful"
  exit 0
fi

echo "==> health check failed — rolling back"
cp "$PREV" "${INSTALL}.new"
chmod +x "${INSTALL}.new"
mv "${INSTALL}.new" "$INSTALL"
systemctl restart rubbish-poc

if wait_healthy "rollback"; then
  echo "==> rollback successful — previous binary is running"
else
  echo "==> CRITICAL: rollback also failed — check systemctl status rubbish-poc" >&2
fi
exit 1
SCRIPT

# rubbish-setup-storage delegates to setup-storage.sh (idempotent, run by systemd)
if [[ -f "$SCRIPTS_DIR/setup-storage.sh" ]]; then
  install -m 0755 "$SCRIPTS_DIR/setup-storage.sh" /usr/local/bin/rubbish-setup-storage
  echo "    installed rubbish-setup-storage from scripts/setup-storage.sh"
else
  echo "    WARNING: scripts/setup-storage.sh not found — rubbish-setup-storage not installed" >&2
fi

echo "    helper scripts installed"

# ---------------------------------------------------------------------------
# 4. Sudoers
# ---------------------------------------------------------------------------
step "Configuring sudoers"

cat > /etc/sudoers.d/rubbish-firecracker << 'EOF'
rubbish ALL=(root) NOPASSWD: /usr/local/bin/rubbish-tap-up
rubbish ALL=(root) NOPASSWD: /usr/local/bin/rubbish-tap-down
rubbish ALL=(root) NOPASSWD: /usr/sbin/dmsetup
rubbish ALL=(root) NOPASSWD: /usr/sbin/blockdev
rubbish ALL=(root) NOPASSWD: /usr/bin/mount
rubbish ALL=(root) NOPASSWD: /usr/bin/umount
rubbish ALL=(root) NOPASSWD: /usr/bin/cp
rubbish ALL=(firecracker) NOPASSWD: /usr/local/bin/firecracker
EOF
chmod 0440 /etc/sudoers.d/rubbish-firecracker

# Allow the claude deploy user to call rubbish-deploy without a password
if id claude &>/dev/null; then
  cat > /etc/sudoers.d/rubbish-deploy << 'EOF'
claude ALL=(ALL) NOPASSWD: /usr/local/bin/rubbish-deploy
EOF
  chmod 0440 /etc/sudoers.d/rubbish-deploy
fi

echo "    sudoers configured"

# ---------------------------------------------------------------------------
# 5. Directory tree
# ---------------------------------------------------------------------------
step "Creating /opt/rubbish directory tree"

# Core dirs owned by the rubbish service account
install -d -m 0755 -o rubbish -g rubbish \
  "$OPT" \
  "$OPT/firecracker" \
  "$OPT/images" \
  "$OPT/dm" \
  "$OPT/ssh" \
  "$OPT/creds" \
  "$OPT/certs"

# Workspace + repo cache: world-readable so NFS clients (uid=1000) can write
install -d -m 0755 "$OPT/repos"
install -d -m 0755 "$OPT/workspaces"

# Claude shared memory (NFS-exported to VMs)
install -d -m 0777 "$OPT/claude-shared"
install -d -m 0777 "$OPT/claude-shared/memory"
install -d -m 0777 "$OPT/claude-shared/projects"

# dev-seed: files injected into dev-mode VMs at boot
install -d -m 0755 "$OPT/dev-seed"
install -d -m 0755 "$OPT/dev-seed/claude"

echo "    directory tree ok"

# ---------------------------------------------------------------------------
# 6. VM bridge (br0) — STP disabled for instant TAP port forwarding
# ---------------------------------------------------------------------------
step "Configuring VM bridge (br0) with STP disabled"

# Write a persistent netplan config.
# CRITICAL: stp: false removes the 30-second Spanning Tree Protocol delay
# that would otherwise block each new TAP device from forwarding packets.
# Without this, SSH takes ~30s to become reachable after every VM boot.
NETPLAN_FILE="/etc/netplan/10-rubbish-br0.yaml"
cat > "$NETPLAN_FILE" << EOF
network:
  version: 2
  renderer: NetworkManager
  bridges:
    ${BRIDGE}:
      addresses:
        - ${BRIDGE_IP}
      dhcp4: false
      parameters:
        stp: false
        forward-delay: 0
EOF
chmod 600 "$NETPLAN_FILE"

netplan apply 2>/dev/null || true

# Ensure STP is off on the live interface (netplan apply may not update a
# running bridge immediately in all NM versions)
if ip link show "$BRIDGE" &>/dev/null; then
  ip link set dev "$BRIDGE" type bridge stp_state 0 2>/dev/null || true
fi

echo "    bridge configured (stp_state=$(cat /sys/class/net/${BRIDGE}/bridge/stp_state 2>/dev/null || echo 'n/a'))"

# ---------------------------------------------------------------------------
# 7. NAT + IP forwarding
# ---------------------------------------------------------------------------
step "Configuring NAT and IP forwarding"

# Persist ip_forward across reboots
if ! grep -q "^net.ipv4.ip_forward" /etc/sysctl.d/99-rubbish.conf 2>/dev/null; then
  echo "net.ipv4.ip_forward = 1" >> /etc/sysctl.d/99-rubbish.conf
fi
sysctl -w net.ipv4.ip_forward=1 >/dev/null

HOST_IFACE=$(ip route get 8.8.8.8 | awk '{for(i=1;i<=NF;i++) if($i=="dev") print $(i+1)}' | head -1)

# NAT masquerade for the VM subnet
if ! iptables -t nat -C POSTROUTING -s "$VM_SUBNET" -o "$HOST_IFACE" -j MASQUERADE 2>/dev/null; then
  iptables -t nat -A POSTROUTING -s "$VM_SUBNET" -o "$HOST_IFACE" -j MASQUERADE
fi
if ! iptables -C FORWARD -i "$BRIDGE" -o "$HOST_IFACE" -j ACCEPT 2>/dev/null; then
  iptables -A FORWARD -i "$BRIDGE" -o "$HOST_IFACE" -j ACCEPT
fi
if ! iptables -C FORWARD -i "$HOST_IFACE" -o "$BRIDGE" -m state --state RELATED,ESTABLISHED -j ACCEPT 2>/dev/null; then
  iptables -A FORWARD -i "$HOST_IFACE" -o "$BRIDGE" -m state --state RELATED,ESTABLISHED -j ACCEPT
fi

# Persist iptables rules
if command -v netfilter-persistent &>/dev/null; then
  netfilter-persistent save >/dev/null 2>&1 || true
elif command -v iptables-save &>/dev/null; then
  iptables-save > /etc/iptables/rules.v4 2>/dev/null || true
fi

echo "    NAT configured (${VM_SUBNET} → ${HOST_IFACE})"

# ---------------------------------------------------------------------------
# 8. NFS exports
# ---------------------------------------------------------------------------
step "Configuring NFS exports"

systemctl enable --now nfs-kernel-server >/dev/null 2>&1

EXPORTS_FILE="/etc/exports"
add_export() {
  local path="$1" opts="$2"
  if ! grep -qF "$path" "$EXPORTS_FILE" 2>/dev/null; then
    echo "$path ${VM_SUBNET}(${opts})" >> "$EXPORTS_FILE"
    echo "    added export: $path"
  else
    echo "    export already present: $path"
  fi
}

# Per-session repo workspaces (writable; uid=1000 inside VM maps to claude user)
add_export "$OPT/workspaces" "rw,sync,no_subtree_check,no_root_squash"

# Shared Claude memory directories (NFS-mounted in dev-mode VMs)
add_export "$OPT/claude-shared/memory"  "rw,sync,no_subtree_check,no_root_squash"
add_export "$OPT/claude-shared/projects" "rw,sync,no_subtree_check,no_root_squash"

exportfs -ra
echo "    NFS exports active"

# ---------------------------------------------------------------------------
# 9. Systemd services
# ---------------------------------------------------------------------------
step "Installing systemd services"

SYSTEMD_DIR=/etc/systemd/system

install_service() {
  local name="$1" src="$SCRIPTS_DIR/${name}.service"
  if [[ -f "$src" ]]; then
    cp "$src" "$SYSTEMD_DIR/${name}.service"
    echo "    installed ${name}.service"
  else
    echo "    WARNING: $src not found — skipping" >&2
  fi
}

install_service rubbish-poc
install_service rubbish-terminal
install_service rubbish-storage

systemctl daemon-reload

# Storage setup runs once at boot, before the main service
systemctl enable rubbish-storage >/dev/null 2>&1 || true

# Main service and terminal proxy are manually started after binaries are deployed
# (they need /usr/local/bin/rubbish-poc and /usr/local/bin/rubbish-terminal)
systemctl enable rubbish-poc     >/dev/null 2>&1 || true
systemctl enable rubbish-terminal >/dev/null 2>&1 || true

echo "    services enabled (start manually after deploying binaries)"

# ---------------------------------------------------------------------------
# 10. dev-seed population (optional)
# ---------------------------------------------------------------------------
if [[ -n "$DEV_SEED_SRC" ]]; then
  step "Populating dev-seed from $DEV_SEED_SRC"
  if [[ ! -d "$DEV_SEED_SRC" ]]; then
    echo "    WARNING: $DEV_SEED_SRC is not a directory — skipping" >&2
  else
    rsync -a --delete "$DEV_SEED_SRC/" "$OPT/dev-seed/"
    echo "    dev-seed populated"
  fi
fi

# ---------------------------------------------------------------------------
# Summary
# ---------------------------------------------------------------------------
echo
echo "==> Host setup complete."
echo
echo "    Next steps:"
echo "    1. Place Firecracker binary at /opt/rubbish/firecracker/"
echo "       (or symlink /usr/local/bin/firecracker)"
echo "    2. Place kernel image at /opt/rubbish/firecracker/vmlinux"
echo "    3. Place base rootfs at /opt/rubbish/images/rootfs.ext4"
echo "    4. Generate a VM SSH key: ssh-keygen -t ed25519 -f /opt/rubbish/ssh/id_ed25519 -N ''"
echo "    5. Run: systemctl start rubbish-storage"
echo "    6. Deploy binaries: sudo rubbish-deploy <path-to-rubbish-poc>"
echo "       (rubbish-terminal is deployed the same way separately)"
echo "    7. Start services: systemctl start rubbish-poc rubbish-terminal"
echo "    8. Verify: curl http://localhost:8080/api/sessions"
