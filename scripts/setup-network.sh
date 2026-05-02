#!/usr/bin/env bash
set -euo pipefail

BRIDGE="br0"
BRIDGE_IP="172.16.0.1/24"
VM_SUBNET="172.16.0.0/24"

# Detect host's default outbound interface
HOST_IFACE=$(ip route get 8.8.8.8 | awk '{for(i=1;i<=NF;i++) if($i=="dev") print $(i+1)}' | head -1)

echo "Setting up bridge ${BRIDGE} (${BRIDGE_IP}) -> NAT via ${HOST_IFACE}"

# Create bridge if it doesn't exist
if ! ip link show "${BRIDGE}" &>/dev/null; then
    ip link add "${BRIDGE}" type bridge
fi

ip addr replace "${BRIDGE_IP}" dev "${BRIDGE}"
ip link set "${BRIDGE}" up

# IP forwarding
echo 1 > /proc/sys/net/ipv4/ip_forward

# NAT: VM subnet -> host outbound interface
if ! iptables -t nat -C POSTROUTING -s "${VM_SUBNET}" -o "${HOST_IFACE}" -j MASQUERADE 2>/dev/null; then
    iptables -t nat -A POSTROUTING -s "${VM_SUBNET}" -o "${HOST_IFACE}" -j MASQUERADE
fi

# Allow forwarding between bridge and host interface
if ! iptables -C FORWARD -i "${BRIDGE}" -o "${HOST_IFACE}" -j ACCEPT 2>/dev/null; then
    iptables -A FORWARD -i "${BRIDGE}" -o "${HOST_IFACE}" -j ACCEPT
fi
if ! iptables -C FORWARD -i "${HOST_IFACE}" -o "${BRIDGE}" -m state --state RELATED,ESTABLISHED -j ACCEPT 2>/dev/null; then
    iptables -A FORWARD -i "${HOST_IFACE}" -o "${BRIDGE}" -m state --state RELATED,ESTABLISHED -j ACCEPT
fi

echo "Network ready: bridge=${BRIDGE} ip=${BRIDGE_IP} nat=${HOST_IFACE}"
