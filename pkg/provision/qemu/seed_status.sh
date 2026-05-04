#!/bin/sh
# y-cluster-seed-status: customer-facing troubleshooting helper.
# Prints the seed metadata, marker contents, k3s state, and any
# conflict listing in a single readable block.
#
# Suggested when something looks wrong with the appliance's
# first-boot setup. Pointed at by the bundle README.

set -u

MOUNT=/data/yolean
SEED=/var/lib/y-cluster/data-seed.tar.zst
META=/var/lib/y-cluster/data-seed.meta.json
MARKER="$MOUNT/.y-cluster-seeded"

echo "=== y-cluster appliance seed status ==="
echo

echo "--- /data/yolean mount"
if mountpoint -q "$MOUNT" 2>/dev/null; then
    findmnt -no SOURCE,TARGET,FSTYPE,SIZE "$MOUNT" 2>/dev/null \
        || mount | grep " $MOUNT " || true
    echo
    echo "Used / free:"
    df -h "$MOUNT" 2>/dev/null | tail -1
else
    echo "NOT a separate mount -- using boot-disk /data/yolean."
fi
echo

echo "--- seed assets on appliance disk"
ls -lh "$SEED" "$META" 2>&1 | head -5
echo

echo "--- seed metadata (build-time)"
if [ -f "$META" ]; then
    cat "$META"
else
    echo "(no $META)"
fi
echo

echo "--- marker on /data/yolean"
if [ -e "$MARKER" ]; then
    echo "PRESENT:"
    cat "$MARKER"
else
    echo "ABSENT."
fi
echo

echo "--- /data/yolean entries (first 20, excluding lost+found)"
if [ -d "$MOUNT" ]; then
    find "$MOUNT" -mindepth 1 -maxdepth 1 ! -name 'lost+found' 2>/dev/null | head -20
fi
echo

echo "--- y-cluster-data-seed.service"
systemctl status y-cluster-data-seed.service --no-pager --lines=20 2>&1 || true
echo

echo "--- k3s.service"
systemctl is-active k3s.service 2>/dev/null || echo "k3s.service is not active"
echo

echo "Recovery recipes (if conflict mode):"
echo "  - mark existing data as seeded: echo '{\"schemaVersion\":1,\"manuallyMarked\":true}' | sudo tee $MARKER"
echo "  - wipe and re-seed (DESTRUCTIVE): sudo rm -rf $MOUNT/* $MOUNT/.[!.]* && sudo systemctl restart y-cluster-data-seed.service"
echo "Then: sudo systemctl start k3s.service"
