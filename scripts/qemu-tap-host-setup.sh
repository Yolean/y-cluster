#!/usr/bin/env bash
# Prepare a host for a y-cluster qemu cluster with `network.mode: tap`.
#
# y-cluster is unprivileged and never creates or configures host
# network devices; this is the one-time, root-owned half. It prints
# nothing y-cluster does not also print when its tap preflight fails.
#
# What it sets up: one tap device owned by the user who will run
# y-cluster, carrying the gateway address of a host-only subnet, and
# IPv4 forwarding. One tap device and one subnet per VM; there is no
# bridge and no DHCP server, the guest's address is static.
#
# Nothing here survives a reboot. For persistence use
# systemd-networkd (.netdev + .network) and /etc/sysctl.d.

[ -z "$DEBUG" ] || set -x
set -eo pipefail

YHELP='qemu-tap-host-setup.sh - create the tap device for a y-cluster qemu cluster in network.mode tap

Usage: sudo qemu-tap-host-setup.sh IFNAME GATEWAY_CIDR [USER]

  IFNAME        tap device name, = network.ifname        (e.g. ycl0)
  GATEWAY_CIDR  host address on the device with prefix,
                = network.gateway + guestAddress prefix  (e.g. 10.88.0.1/24)
  USER          owner of the device; default: $SUDO_USER

Publishing the cluster (optional, nftables; own table, no flush ruleset):
  qemu-tap-host-setup.sh nft PUBLIC_IF GUEST_IP SUBNET_CIDR
    prints a ruleset that DNATs 80/443 arriving on PUBLIC_IF to the guest
    and masquerades the guest subnet going out. Review it, then apply with
    `sudo nft -f -`. DNAT leaves the client address alone, which is the
    point of tap mode; never masquerade traffic going INTO the tap.

Dependencies: ip (iproute2), sysctl
'

case "${1:-}" in
  ""|help|--help|-h) echo "$YHELP"; exit 0 ;;
esac

if [ "$1" = "nft" ]; then
  [ $# -eq 4 ] || { echo "usage: $0 nft PUBLIC_IF GUEST_IP SUBNET_CIDR" >&2; exit 2; }
  public_if=$2 guest_ip=$3 subnet=$4
  # `add table` + `delete table` first makes re-applying idempotent
  # without touching any table this ruleset does not own.
  cat <<NFT
add table ip ycluster
delete table ip ycluster
table ip ycluster {
  chain prerouting {
    type nat hook prerouting priority dstnat;
    iifname "$public_if" tcp dport { 80, 443 } dnat to $guest_ip
  }
  chain postrouting {
    type nat hook postrouting priority srcnat;
    ip saddr $subnet oifname "$public_if" masquerade
  }
}
NFT
  exit 0
fi

[ $# -ge 2 ] || { echo "$YHELP" >&2; exit 2; }
ifname=$1 gateway_cidr=$2 owner=${3:-${SUDO_USER:-}}
[ -n "$owner" ] || { echo "no USER given and \$SUDO_USER is empty" >&2; exit 2; }
[ "$(id -u)" -eq 0 ] || { echo "must run as root (sudo)" >&2; exit 1; }

if ip link show "$ifname" >/dev/null 2>&1; then
  echo "$ifname already exists; leaving the device as it is"
else
  ip tuntap add dev "$ifname" mode tap user "$owner"
fi
if ip -4 addr show dev "$ifname" | grep -q " ${gateway_cidr} "; then
  echo "$ifname already has $gateway_cidr"
else
  ip addr add "$gateway_cidr" dev "$ifname"
fi
ip link set "$ifname" up
sysctl -w net.ipv4.ip_forward=1

echo "ready: $ifname owned by $owner with $gateway_cidr"
