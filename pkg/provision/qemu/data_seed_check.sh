#!/bin/sh
# y-cluster-data-seed-check: extract the build-time /data/yolean
# snapshot onto a freshly-attached customer disk, or no-op when
# the appliance disk's own /data/yolean is in use, or refuse to
# clobber unrecognised data.
#
# Run by y-cluster-data-seed.service (oneshot) Before=k3s.service.
# k3s.service has a Requires= drop-in pointing here so a failure
# blocks the cluster from coming up -- the customer SSHes in,
# reads the journal, fixes the situation, and either restarts
# this unit or starts k3s manually.
#
# Decision table:
#   /data/yolean is NOT a separate mount        -> no-op
#   marker present                              -> no-op
#   no marker, dir empty (excl. lost+found)     -> extract seed
#   no marker, dir non-empty                    -> fail (conflict)
#
# See specs/y-cluster/APPLIANCE_UPGRADES.md for the full design.

set -eu

MOUNT=/data/yolean
SEED=/var/lib/y-cluster/data-seed.tar.zst
META=/var/lib/y-cluster/data-seed.meta.json
MARKER="$MOUNT/.y-cluster-seeded"

# 1. Not a separate mount -> data lives on the boot disk, already
# populated by the appliance build. No customer drive attached;
# nothing to seed. We let k3s come up against the boot-disk data.
if ! mountpoint -q "$MOUNT" 2>/dev/null; then
    echo "y-cluster-data-seed: $MOUNT is not a separate mount; using appliance boot-disk data, no seed needed."
    exit 0
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

The customer drive at $MOUNT contains files that we did not put there
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

# 4. Empty mountpoint, no marker. Extract the seed.
if [ ! -f "$SEED" ]; then
    echo "y-cluster-data-seed: no seed at $SEED; this appliance was built without one." >&2
    echo "y-cluster-data-seed: cannot proceed -- mark $MARKER manually if the empty mount is intentional." >&2
    exit 1
fi

if [ ! -f "$META" ]; then
    echo "y-cluster-data-seed: no metadata at $META; appliance build is incomplete." >&2
    exit 1
fi

echo "y-cluster-data-seed: extracting $SEED to $MOUNT"
zstdcat "$SEED" | tar -C "$MOUNT" -xpf -
echo "y-cluster-data-seed: extracted."

# 5. Write the marker LAST. A crashed extract leaves no marker, so
# the next boot detects "non-empty without marker" -> conflict mode
# (case 3) and surfaces the problem instead of silently retrying.
# We copy the build-time meta verbatim; it carries the seed sha,
# build version, and timestamp.
cp "$META" "$MARKER"
chmod 0644 "$MARKER"

echo "y-cluster-data-seed: seeded $MOUNT successfully."
cat "$MARKER"
