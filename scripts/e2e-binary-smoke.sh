#!/usr/bin/env bash
# Smoke test for a built or released y-cluster binary.
#
# Expects $Y_CLUSTER_BIN to point at a y-cluster executable. Runs the
# checks that need neither a cluster nor docker, so they work on every
# OS/arch a release ships for. Behaviour is what the e2e suite covers,
# on linux/amd64 only; this answers "does the asset we published start
# and do real work on this platform".
#
# Runs in .github/workflows/ci.yaml against the build artifact and in
# .github/workflows/e2e-release.yaml on ubuntu-latest and macos-latest
# against the downloaded release asset.
set -euo pipefail

Y_CLUSTER_BIN="${Y_CLUSTER_BIN:-./y-cluster}"
if [ ! -x "$Y_CLUSTER_BIN" ]; then
  echo "Y_CLUSTER_BIN is not executable: $Y_CLUSTER_BIN" >&2
  exit 2
fi

work=$(mktemp -d 2>/dev/null || mktemp -d -t 'y-cluster-smoke')
trap 'rm -rf "$work"' EXIT

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

echo "==> --version"
"$Y_CLUSTER_BIN" --version

echo "==> images list (YAML mode: parses a manifest stream, no cluster)"
cat >"$work/manifests.yaml" <<'YAML'
apiVersion: apps/v1
kind: Deployment
metadata:
  name: web
spec:
  selector:
    matchLabels: {app: web}
  template:
    metadata:
      labels: {app: web}
    spec:
      initContainers:
      - name: init
        image: ghcr.io/example/init:1
      containers:
      - name: web
        image: ghcr.io/example/web:2
---
apiVersion: batch/v1
kind: Job
metadata:
  name: migrate
spec:
  template:
    spec:
      restartPolicy: Never
      containers:
      - name: migrate
        image: ghcr.io/example/web:2
YAML
got=$("$Y_CLUSTER_BIN" images list "$work/manifests.yaml")
want='ghcr.io/example/init:1
ghcr.io/example/web:2'
[ "$got" = "$want" ] || fail "images list printed:
$got
want (sorted, deduplicated):
$want"

echo "==> cache info --path honours Y_CLUSTER_CACHE_DIR"
got=$(Y_CLUSTER_CACHE_DIR="$work/cache" "$Y_CLUSTER_BIN" cache info --path)
[ "$got" = "$work/cache" ] || fail "cache root is $got, want $work/cache"

echo "==> a config that names no known provider is refused with the list"
mkdir "$work/cfg"
printf 'provider: nosuchprovider\n' >"$work/cfg/y-cluster-provision.yaml"
if out=$("$Y_CLUSTER_BIN" provision -c "$work/cfg" 2>&1); then
  fail "provision accepted provider nosuchprovider: $out"
fi
case "$out" in
  *qemu*docker*|*docker*qemu*) ;;
  *) fail "the refusal does not list the providers: $out" ;;
esac

echo "=== all checks passed ==="
