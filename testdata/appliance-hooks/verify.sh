#!/usr/bin/env bash
# APPLIANCE_VERIFY_CMD fixture for scripts/e2e-appliance-export-import.sh.
#
# Asserts the import-side Y_CLUSTER_CURRENT_* contract and that the
# manifest seed.sh staged was applied by k3s on the imported
# instance's first boot. The imported instance has no kubeconfig
# context on the host, so the apiserver is reached over ssh.
set -euo pipefail

for v in NAME BUNDLE_DIR IMPORTED_HTTP_PORT IMPORTED_SSH_PORT IMPORTED_SSH_KEY; do
    n="Y_CLUSTER_CURRENT_$v"
    [[ -n "${!n:-}" ]] || { echo "FAIL: $n is empty in the verify hook" >&2; exit 1; }
done

ssh_guest() {
    ssh -i "$Y_CLUSTER_CURRENT_IMPORTED_SSH_KEY" \
        -p "$Y_CLUSTER_CURRENT_IMPORTED_SSH_PORT" \
        -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
        -o ConnectTimeout=5 -o LogLevel=ERROR \
        ystack@127.0.0.1 "$@"
}

for i in $(seq 1 30); do
    if staged_by=$(ssh_guest sudo k3s kubectl -n default get configmap \
            appliance-hook-fixture -o 'jsonpath={.data.staged-by}'); then
        [[ "$staged_by" == "testdata/appliance-hooks/seed.sh" ]] \
            || { echo "FAIL: unexpected configmap content: $staged_by" >&2; exit 1; }
        echo "verify hook: staged manifest was applied on first boot (try $i)"
        exit 0
    fi
    sleep 5
done
echo "FAIL: staged manifest never appeared on the imported instance" >&2
exit 1
