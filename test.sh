#!/usr/bin/env bash
# test.sh -- run every test this host can support, in one shot.
#
# Always:
#   unit tests (no build tags) + go vet
#
# If Docker is reachable:
#   e2e tests against a kwok container in Docker
#   e2e tests against the k3s-in-docker provisioner
#
# If /dev/kvm + qemu-system-x86_64 are present:
#   e2e tests against the qemu provisioner (bundled into the same
#   `go test` invocation since e2e build tags compose)
#
# Run from the repo root or any subdir; the script cd's to its own
# directory first so it works either way.
set -euo pipefail

cd "$(dirname "$0")"

echo "==> unit tests"
go test -count=1 ./...

echo
echo "==> go vet"
go vet ./...

if ! docker info >/dev/null 2>&1; then
  echo
  echo "Docker daemon not reachable; skipping e2e."
  exit 0
fi

tags="e2e,docker"
if [ -e /dev/kvm ] && command -v qemu-system-x86_64 >/dev/null 2>&1; then
  tags+=",kvm"
fi

echo
echo "==> e2e (-tags=$tags)"
exec go test -tags "$tags" -count=1 -timeout=20m ./e2e/
