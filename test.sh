#!/usr/bin/env bash
# test.sh -- run every test this host can support, in one shot.
#
# Always:
#   unit tests (no build tags) + go vet
#   golangci-lint, if installed (CI installs it; dev machines opt in)
#   lint of every shell script, if shellcheck is installed
#   y-cluster binary build + serve smoke test (the same script
#   the release pipeline runs against the published archive)
#
# If Docker is reachable:
#   e2e tests against a kwok container in Docker
#   e2e tests against the k3s-in-docker provisioner
#
# If /dev/kvm + qemu-system-x86_64 are present:
#   e2e tests against the qemu provisioner (bundled into the same
#   `go test` invocation since e2e build tags compose)
#   the appliance export/import round trip, with the hook fixture in
#   testdata/appliance-hooks standing in for a downstream repo
#
# Run from the repo root or any subdir; the script cd's to its own
# directory first so it works either way.
set -euo pipefail

cd "$(dirname "$0")"

# Unit tests must never touch the operator's kubeconfig. They run
# against a sentinel file instead, and any write to it is a failure.
sentinel=$(mktemp)
printf 'apiVersion: v1\nkind: Config\nclusters:\n- name: y-cluster\n  cluster:\n    server: https://127.0.0.1:6443\ncontexts:\n- name: local\n  context:\n    cluster: y-cluster\n    user: y-cluster\nusers:\n- name: y-cluster\n  user: {}\n' > "$sentinel"
sentinel_sum=$(sha256sum "$sentinel")

echo "==> unit tests"
KUBECONFIG="$sentinel" go test -count=1 ./...
if [ "$sentinel_sum" != "$(sha256sum "$sentinel")" ]; then
  echo "FAIL: a unit test modified \$KUBECONFIG ($sentinel)" >&2
  exit 1
fi
rm "$sentinel"

echo
echo "==> go vet"
go vet ./...

echo
if command -v golangci-lint >/dev/null 2>&1; then
  echo "==> golangci-lint"
  golangci-lint run --timeout=5m
else
  echo "==> golangci-lint  (skipped: not installed; CI runs it)"
fi

echo
if command -v shellcheck >/dev/null 2>&1; then
  echo "==> shellcheck"
  shellcheck -x --severity=warning test.sh scripts/*.sh testdata/appliance-hooks/*.sh
else
  echo "==> shellcheck  (skipped: not installed; CI runs it)"
fi

echo
echo "==> serve smoke test against built binary"
bin=$(mktemp -d)/y-cluster
trap 'rm -rf "$(dirname "$bin")"' EXIT
go build -o "$bin" ./cmd/y-cluster
Y_CLUSTER_BIN="$bin" bash scripts/e2e-serve-against-binary.sh

if ! docker info >/dev/null 2>&1; then
  echo
  echo "Docker daemon not reachable; skipping e2e."
  exit 0
fi

tags="e2e,docker"
timeout=20m
if [ -e /dev/kvm ] && command -v qemu-system-x86_64 >/dev/null 2>&1; then
  tags+=",kvm"
  # Every qemu test provisions a VM: the set runs for about an hour
  # and writes several GB of disk images per test under TMPDIR. /tmp
  # is a RAM-backed tmpfs on most distros; /var/tmp is on disk.
  timeout=120m
  export TMPDIR="${TMPDIR:-/var/tmp}"
fi

echo
echo "==> e2e (-tags=$tags)"
go test -tags "$tags" -count=1 -timeout="$timeout" ./e2e/

if [[ "$tags" == *kvm* ]]; then
  echo
  echo "==> appliance export/import round trip with hook fixture"
  # Unprivileged host ports and a throwaway kubeconfig, so the run
  # neither needs CAP_NET_BIND_SERVICE nor touches the operator's
  # contexts.
  e2e_kubeconfig=$(mktemp)
  KUBECONFIG="$e2e_kubeconfig" \
  NAME=appliance-hooks-e2e \
  APP_HTTP_PORT=39080 APP_HTTPS_PORT=39443 APP_API_PORT=39643 APP_SSH_PORT=2229 \
  APPLIANCE_SEED_CMD="$PWD/testdata/appliance-hooks/seed.sh" \
  APPLIANCE_VERIFY_CMD="$PWD/testdata/appliance-hooks/verify.sh" \
    bash scripts/e2e-appliance-export-import.sh
  rm "$e2e_kubeconfig"
fi
