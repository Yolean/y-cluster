#!/usr/bin/env bash
# APPLIANCE_SEED_CMD fixture for scripts/e2e-appliance-export-import.sh.
#
# Stands in for a downstream repo's converge. It pins two things:
#   - the build-side Y_CLUSTER_CURRENT_* contract (scripts/_hooks.sh)
#   - `manifests add` stages on the node WITHOUT applying to the
#     build cluster; verify.sh asserts the other half, that the
#     manifest is applied on the imported instance's first boot.
set -euo pipefail

for v in BIN NAME KUBECTX LOCAL_HTTP_PORT LOCAL_HTTPS_PORT LOCAL_API_PORT \
         LOCAL_SSH_PORT LOCAL_SSH_KEY; do
    n="Y_CLUSTER_CURRENT_$v"
    [[ -n "${!n:-}" ]] || { echo "FAIL: $n is empty in the seed hook" >&2; exit 1; }
done
# Exported but not known yet on the build side.
[[ -n "${Y_CLUSTER_CURRENT_BUNDLE_DIR+set}" ]] \
    || { echo "FAIL: Y_CLUSTER_CURRENT_BUNDLE_DIR is unset (must be exported empty)" >&2; exit 1; }
[[ -x "$Y_CLUSTER_CURRENT_BIN" ]] \
    || { echo "FAIL: Y_CLUSTER_CURRENT_BIN=$Y_CLUSTER_CURRENT_BIN is not executable" >&2; exit 1; }
[[ -f "$Y_CLUSTER_CURRENT_LOCAL_SSH_KEY" ]] \
    || { echo "FAIL: no ssh key at $Y_CLUSTER_CURRENT_LOCAL_SSH_KEY" >&2; exit 1; }

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
"$Y_CLUSTER_CURRENT_BIN" manifests add appliance-hook-fixture \
    "$here/first-boot-configmap.yaml" --context="$Y_CLUSTER_CURRENT_KUBECTX"

# Re-adding byte-identical content is a documented no-op.
"$Y_CLUSTER_CURRENT_BIN" manifests add appliance-hook-fixture \
    "$here/first-boot-configmap.yaml" --context="$Y_CLUSTER_CURRENT_KUBECTX"

if kubectl --context="$Y_CLUSTER_CURRENT_KUBECTX" -n default \
        get configmap appliance-hook-fixture >/dev/null 2>&1; then
    echo "FAIL: staged manifest was applied to the build cluster" >&2
    exit 1
fi
echo "seed hook: manifest staged, not applied on the build side"
