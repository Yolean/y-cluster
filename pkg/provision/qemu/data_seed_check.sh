#!/bin/sh
# y-cluster-data-seed-check: extract the build-time /data/yolean
# snapshot onto a freshly-attached customer disk, no-op when an
# already-seeded marker is present, refuse to clobber unrecognised
# data, or fail closed when the customer forgot to attach their
# data volume.
#
# Run by y-cluster-data-seed.service (oneshot) Before=k3s.service,
# After=cloud-init.service. The cloud-init ordering matters because
# hosting automation can write /run/y-cluster-seed-bypass via a
# user_data write_files entry; this unit must run AFTER cloud-init
# has had a chance to drop that file.
#
# k3s.service has a Requires= drop-in pointing here so a failure
# blocks the cluster from coming up -- the customer SSHes in,
# reads the journal, fixes the situation, and either restarts
# this unit or starts k3s manually. sshd is unaffected by this
# unit's failure (no transitive dependency).
#
# Decision table (in order):
#   /run/y-cluster-seed-bypass exists           -> bypass: extract
#                                                  regardless of mount
#   /data/yolean is NOT a mountpoint            -> fail (production:
#                                                  customer must attach
#                                                  the data volume)
#   marker present                              -> no-op (upgrade fast
#                                                  path)
#   mountpoint empty (excl. lost+found)         -> extract, write marker
#   mountpoint non-empty, no marker             -> fail (conflict)
#
# See APPLIANCE_MAINTENANCE.md for the full lifecycle design.

set -eu

MOUNT=/data/yolean
SEED=/var/lib/y-cluster/data-seed.tar.zst
META=/var/lib/y-cluster/data-seed.meta.json
MARKER="$MOUNT/.y-cluster-seeded"
BYPASS_FLAG=/run/y-cluster-seed-bypass

# 0. Bypass: a tmpfs flag set by hosting automation (cloud-init
# write_files via the server's user_data). Only present when an
# entity with hosting-API access deliberately set it. The customer
# importing onto VMware / VirtualBox / bare metal has no cloud-init
# datasource available, so this branch is unreachable for them.
# Used for the Hetzner QA path where attaching a labeled volume per
# server is awkward; we ship the same bundle and bypass at import.
if [ -e "$BYPASS_FLAG" ]; then
    echo "y-cluster-data-seed: bypass flag $BYPASS_FLAG present; extracting regardless of mount state."
    BYPASSED=1
else
    BYPASSED=0
fi

# 1. Mount required (unless bypassed). prepare-export pre-bakes
# `LABEL=y-cluster-data /data/yolean ext4 defaults,nofail 0 2` in
# /etc/fstab, so any production appliance expects a customer-supplied
# volume. Failing closed here eliminates the customer-mounts-after-k3s
# race we hit on the GCP appliance (redpanda PVC permission-denied,
# mariadb missing grastate.dat).
if [ "$BYPASSED" = "0" ]; then
    if ! mountpoint -q "$MOUNT" 2>/dev/null; then
        cat >&2 <<EOF
y-cluster-data-seed: $MOUNT is not a mountpoint; refusing to start.

The appliance expects a separate volume mounted at $MOUNT. The build
pre-baked /etc/fstab to mount a volume labeled y-cluster-data on
first boot:

    LABEL=y-cluster-data /data/yolean ext4 defaults,nofail 0 2

Resolution:
  - Attach a volume to this VM. ext4-format it with the label:
        sudo mkfs.ext4 -L y-cluster-data /dev/<device>
    Then reboot, OR mount manually and restart this unit:
        sudo mount /data/yolean
        sudo systemctl restart y-cluster-data-seed.service
  - If you intentionally want this appliance to use the boot disk's
    /data/yolean (no separate volume), this is a hosting-automation
    concern, not a customer one -- inject $BYPASS_FLAG via cloud-init
    user_data write_files at provision time.
EOF
        exit 1
    fi
fi

# 2. Marker present -> the customer's drive has been seeded before
# (by us, on a previous boot) OR the customer placed the marker
# manually after restoring data themselves. Either way we respect
# what's there and do not touch it.
if [ -e "$MARKER" ]; then
    echo "y-cluster-data-seed: marker present at $MARKER; respecting existing data."
    cat "$MARKER" 2>/dev/null || true
    exit 0
fi

# 3. No marker. Inspect contents excluding lost+found (created by
# the kernel on every fresh ext4 mount; not a sign of customer data).
ENTRIES=$(find "$MOUNT" -mindepth 1 -maxdepth 1 ! -name 'lost+found' 2>/dev/null | head -20)
if [ -n "$ENTRIES" ]; then
    cat >&2 <<EOF
y-cluster-data-seed: $MOUNT has unmarked contents; refusing to seed.

The drive at $MOUNT contains files that we did not put there
and cannot prove are an already-seeded state. Seeding would clobber
data we don't recognise. k3s WILL NOT start until this is resolved.

Conflicting entries (first 20 shown):
$ENTRIES

Resolution:
  (a) The data is correct and already in the shape this appliance
      expects (e.g., you restored from a backup):
          # Mark as seeded WITHOUT extracting:
          echo '{"schemaVersion":1,"manuallyMarked":true}' \\
              | sudo tee $MARKER >/dev/null
          sudo systemctl restart y-cluster-data-seed.service
  (b) The data is junk (e.g., from a previous wrong boot) and you
      want a fresh seed (DESTRUCTIVE):
          sudo rm -rf $MOUNT/* $MOUNT/.[!.]*
          sudo systemctl restart y-cluster-data-seed.service
  (c) The drive is the wrong drive entirely:
          poweroff, attach the right drive, boot again.
EOF
    exit 1
fi

# 4. Empty mount (or bypassed), no marker. Extract the seed.
if [ ! -f "$SEED" ]; then
    echo "y-cluster-data-seed: no seed at $SEED; this appliance was built without one." >&2
    echo "y-cluster-data-seed: cannot proceed -- mark $MARKER manually if the empty mount is intentional." >&2
    exit 1
fi

if [ ! -f "$META" ]; then
    echo "y-cluster-data-seed: no metadata at $META; appliance build is incomplete." >&2
    exit 1
fi

# In bypass mode the path may not exist yet on disk because the
# fstab mount soft-failed. mkdir -p is a no-op if it already exists.
mkdir -p "$MOUNT"

echo "y-cluster-data-seed: extracting $SEED to $MOUNT"
zstdcat "$SEED" | tar -C "$MOUNT" -xpf -
echo "y-cluster-data-seed: extracted."

# 5. Write the marker LAST. A crashed extract leaves no marker, so
# the next boot detects "non-empty without marker" -> conflict mode
# (case 3) and surfaces the problem instead of silently retrying.
# We copy the build-time meta verbatim; in bypass mode we drop a
# sibling sentinel so a future operator can tell at a glance the
# seed went down the bypass path (the BYPASS_FLAG itself is tmpfs
# and gone after the next reboot).
cp "$META" "$MARKER"
chmod 0644 "$MARKER"
if [ "$BYPASSED" = "1" ]; then
    touch "$MOUNT/.y-cluster-seeded-via-bypass"
fi

echo "y-cluster-data-seed: seeded $MOUNT successfully."
cat "$MARKER"
