#!/bin/sh
# y-cluster appliance prepare-script: in-guest cleanup that strips
# host-specific identity to make the disk portable.
#
# Run as root, either:
#   - via virt-customize against an offline qcow2 (qemu prepare-export)
#   - inline in the build VM before snapshot (Hetzner Packer)
#
# Idempotent. Does NOT power off; the caller decides when to stop
# k3s and shut down.
#
# Per-customer build model: keep the things that would otherwise
# break first boot or block ssh access, wipe the things that
# leak host identity or would mis-match a different NIC.
#
# Kept on purpose:
#   - /etc/machine-id  -- removing it breaks systemd-networkd DHCP
#                         on Ubuntu 24.04 ("No such file or
#                         directory" on init).
#   - /etc/ssh/ssh_host_*_key -- baked-in keys are part of the
#                         per-customer handoff bundle. Hetzner's
#                         cc_ssh module regenerates them on first
#                         cloned boot when instance-id changes,
#                         which is fine; keeping them here costs
#                         nothing and avoids racing ssh.service
#                         on the qemu side.
#   - ~/.ssh/authorized_keys -- the operator's per-customer keypair
#                         lives here; wiping it would break ssh
#                         access on the imported boot.

# POSIX sh because virt-customize's --run executes via /bin/sh
# (dash on Ubuntu) regardless of shebang. dash doesn't accept
# `-o pipefail`, and we don't pipe anywhere in this script, so
# `-eux` covers the cases we care about: stop on error, expand
# unset variables verbatim (we have none), and trace each line.
set -eux

# 1. cloud-init: wipe cached state so first boot of the imported
# VM re-runs first-boot logic against whatever datasource the
# target host provides. Do NOT pass --machine-id; we keep
# /etc/machine-id intact (see header).
cloud-init clean --logs --seed

# 2. cloud-init: disable network-config regeneration. Without this,
# cloud-init re-creates /etc/netplan/50-cloud-init.yaml on first
# boot pinned to the current NIC's MAC, clobbering our generic
# netplan written below.
install -d -m 0755 /etc/cloud/cloud.cfg.d
cat > /etc/cloud/cloud.cfg.d/99-y-cluster-no-network-config.cfg <<'CFG'
# y-cluster prepare-export: keep cloud-init from regenerating
# /etc/netplan/50-cloud-init.yaml on the imported host. Without
# this, cloud-init recreates the file pinned to the build host's
# NIC MAC and DHCP fails for any new MAC.
network: {config: disabled}
CFG
chmod 0644 /etc/cloud/cloud.cfg.d/99-y-cluster-no-network-config.cfg

# 3. Generic netplan. Matches any en* / eth* NIC -- the two
# common kernel NIC name schemes (predictable interface names
# for newer images, classic eth0 on hosts that disable that).
# Both stanzas use DHCP, which every cloud / hypervisor target
# supports out of the box.
install -d -m 0755 /etc/netplan
cat > /etc/netplan/50-cloud-init.yaml <<'NETPLAN'
network:
  version: 2
  renderer: networkd
  ethernets:
    any-iface:
      match:
        name: "e*"
      dhcp4: true
      dhcp6: true
NETPLAN
chmod 0600 /etc/netplan/50-cloud-init.yaml

# 4. Hygienic wipes for shipping a clean appliance. None of these
# are load-bearing for portability -- they just keep the bundle
# small and free of build-time stamps the customer would see.
rm -f /root/.bash_history
# POSIX-safe glob: when no /home/*/.bash_history exists, the
# loop variable is the literal pattern, which `[ -f ]` rejects.
for h in /home/*/.bash_history; do
    [ -f "$h" ] && rm -f "$h"
done
rm -f /etc/udev/rules.d/70-persistent-net.rules
rm -f /var/lib/systemd/random-seed
rm -f /var/lib/dhcp/dhclient.leases
apt-get clean

# 5a. Clear /data/yolean on the boot disk now that BuildSeedAssets
# (host-side, BEFORE virt-customize started) has snapshotted its
# content into /var/lib/y-cluster/data-seed.tar.zst. Two reasons:
#
#   - Production: the customer's persistent volume mounts at
#     /data/yolean on first boot and shadows the boot-disk dir, so
#     the bytes here are dead weight. fstrim later reclaims the
#     freed blocks; the appliance image ships smaller.
#   - Bypass (Hetzner QA): no labeled volume attached, so the boot
#     disk's /data/yolean IS the seed target. data_seed_check.sh's
#     conflict-detection branch refuses to overwrite unmarked files;
#     clearing here means the bypass extract goes into an empty dir
#     and the marker writes cleanly.
#
# The dir itself is preserved (recreated) so the fstab mount has a
# mountpoint to attach to and seed-check has somewhere to extract
# into.
mkdir -p /data/yolean
rm -rf /data/yolean/* /data/yolean/.[!.]* 2>/dev/null || true

# 5c. Pre-bake the customer's persistent /data/yolean fstab entry.
# The customer attaches an ext4 volume labeled "y-cluster-data" to
# the imported VM; cloud-agnostic LABEL= mounting means VMware /
# VirtualBox / Hetzner / GCP all recognise the same volume without
# the customer editing fstab themselves. nofail keeps boot moving
# even if the volume isn't attached -- y-cluster-data-seed.service
# fails closed in that case and surfaces the actionable error.
# Idempotent: re-running prepare-export doesn't dupe the entry.
if ! grep -q 'LABEL=y-cluster-data' /etc/fstab; then
    echo 'LABEL=y-cluster-data /data/yolean ext4 defaults,nofail 0 2' >> /etc/fstab
fi

# 5d. Enable wall-clock sync at first boot. Without this, an
# imported VM whose RTC was set by the host clock can boot
# minutes-to-hours away from real UTC, and k3s's TLS certs
# (NotBefore = build time) read as "not yet valid", which
# manifests as a healthy-looking k3s-server with every pod
# stuck in Completed and authentication.go x509 errors in
# the journal. systemd-timesyncd is in Ubuntu's default
# install; just flip its enable bit so first boot syncs
# before k3s starts. The k3s service is `Wants=` not
# `Requires=` time-sync, so an offline appliance still boots,
# just with whatever clock the hypervisor handed over.
systemctl enable systemd-timesyncd.service

# 6. Move build-time-staged manifests into k3s's auto-apply
# directory. y-cluster's `manifests add` command writes manifests
# the appliance builder wants applied at customer boot to
# /var/lib/y-cluster/manifests-staging/. k3s does NOT scan that
# directory, so the manifests sit dormant during build (the build
# cluster doesn't react). At export time we move them into
# /var/lib/rancher/k3s/server/manifests/ -- which IS auto-applied
# by k3s on every cluster start. The customer's first boot of the
# appliance therefore runs every staged manifest against THEIR
# cluster (e.g., migration Jobs).
#
# See APPLIANCE_MAINTENANCE.md.
if [ -d /var/lib/y-cluster/manifests-staging ] \
        && [ "$(ls -A /var/lib/y-cluster/manifests-staging 2>/dev/null)" ]; then
    mkdir -p /var/lib/rancher/k3s/server/manifests
    mv /var/lib/y-cluster/manifests-staging/* \
       /var/lib/rancher/k3s/server/manifests/
fi
# Remove the (now-empty) staging dir so it's clear the customer's
# boot has nothing left to do at this layer.
rmdir /var/lib/y-cluster/manifests-staging 2>/dev/null || true

# 7. fstrim every mounted filesystem. ext4 marks deleted blocks
# free in its bitmap but doesn't zero them on disk; the next
# `qemu-img convert -O raw` carries the old content into the
# raw output and gzip can't squash non-zero bytes. Trimming
# tells the underlying device "these blocks are discardable",
# which on qemu's qcow2 + file-backed disk means they get
# zero-filled in the convert step. Effect: the gzipped tar
# we ship to GCS / VirtualBox / a customer drops by 10-30%
# on a heavily-used appliance, with no impact on what
# actually runs at first boot.
#
# We do NOT prune unused container images here -- the
# appliance contract is "ship every image the customer might
# need pre-loaded, including ones not currently referenced by
# a running pod" (e.g., upgrade images, fallback images).
# fstrim reclaims FREED blocks; it leaves every byte that's
# actually in use untouched. That is the right scope.
#
# `|| true`: a filesystem that doesn't support TRIM (tmpfs,
# read-only mounts, anything weird) returns nonzero. We don't
# care -- trimming what we can is a strict win and any
# unsupported mount fails locally without affecting the
# rest of the script.
fstrim -av || true

# 7. Truncate (don't delete) log files so service file descriptors
# stay valid if any service is still writing. Next boot starts
# with empty logs.
for f in /var/log/syslog /var/log/auth.log /var/log/cloud-init.log /var/log/cloud-init-output.log; do
    if [ -f "$f" ]; then
        truncate -s 0 "$f"
    fi
done
