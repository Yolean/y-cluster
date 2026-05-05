#!/usr/bin/env bash
# Round-trip an y-cluster appliance through the export/import contract:
# build with y-cluster, install a placeholder application via kubectl,
# prepare-export, stop, copy the qcow2, then boot a SECOND qemu
# instance against the copy with no y-cluster involvement (simulating
# the customer's IT importing on their hypervisor) and verify the
# application reaches a 200 from a fresh process.
#
# Why this exists:
#   The "build a per-customer appliance, ship it, customer boots it"
#   pathway has never been e2e-tested. The Hetzner Packer flow proved
#   snapshot+clone works on Hetzner; it doesn't tell us whether a
#   qcow2 produced locally boots cleanly elsewhere. This script is
#   the missing test.
#
# Conventions:
#   - The application is opaque to y-cluster. We use the echo
#     manifest as a placeholder, but install it via `y-cluster echo
#     render | kubectl apply -f -` -- the same shape the eventual
#     per-customer install will use (kubectl / kustomize / helm
#     against the live cluster). y-cluster has no `echo deploy`-like
#     special case here.
#   - The customer-side qemu invocation is bare bash. No y-cluster
#     binary, no seed image, no cloud-init reattach. Just qemu-system
#     against the exported qcow2 with new port forwards. If the
#     appliance can't survive that, prepare-export has the bug.
#
# Stages:
#   1. Build the dev binary into ./dist (gitignored).
#   2. Provision an appliance (k3s + Envoy Gateway only) under a
#      throwaway name.
#   3. Apply the placeholder app via kubectl.
#   4. Smoketest curl on the build-side host.
#   5. y-cluster stop + prepare-export.
#   6. y-cluster export to a bundle dir (flattened qcow2 +
#      keypair + README).
#   7. Boot a fresh qemu against the BUNDLED qcow2 with new
#      port forwards. The bundle has no backing-file dependency
#      on y-cluster's cloud-image cache; this proves the disk is
#      genuinely portable.
#   8. Wait for ssh + curl on the imported instance.
#   9. On failure, ssh in and dump k3s state for diagnosis.

[ -z "$DEBUG" ] || set -x
set -eo pipefail

YHELP='e2e-appliance-export-import.sh - local round-trip provision -> kubectl install -> prepare-export -> stop -> raw-qemu boot -> verify

Usage: e2e-appliance-export-import.sh

Environment:
  NAME             Appliance name (default: appliance-export-test)
  APP_HTTP_PORT    Override build-side host port for guest 80 (y-cluster default: 80)
  APP_API_PORT     Override build-side host port for guest 6443 (y-cluster default: 6443)
  APP_SSH_PORT     Override build-side host port for guest 22 (y-cluster default: 2222)
  IMP_HTTP_PORT    Import-side host port -> guest 80 (default: 39180)
  IMP_SSH_PORT     Import-side host port -> guest 22 (default: 2230)
  Y_CLUSTER        Path to dev binary (default: ./dist/y-cluster)
  CACHE_DIR        Where y-cluster keeps its qcow2 (default: ~/.cache/y-cluster-qemu)
  KEEP_BUILD       Set to keep the build-side cluster after success (default: tear it down)
  DEBUG            Set non-empty for bash trace

Dependencies:
  go, qemu-system-x86_64, kubectl, ssh, ssh-keygen, curl, virt-sysprep (libguestfs-tools)

Exit codes:
  0  Round-trip succeeded; imported instance answered the smoketest
  1  Any stage failed; build-side cluster left up for diagnosis
'

case "${1:-}" in
  help) echo "$YHELP"; exit 0 ;;
  --help) echo "$YHELP"; exit 0 ;;
  -h) echo "$YHELP"; exit 0 ;;
esac

NAME="${NAME:-appliance-export-test}"
# Import-side host ports: kept hardcoded (not env-overridable +
# defaulted) because the import-side qemu is started directly by
# this script (no y-cluster CLI involvement) and these values
# can't collide with the build-side y-cluster's defaults.
IMP_HTTP_PORT="${IMP_HTTP_PORT:-39180}"
IMP_SSH_PORT="${IMP_SSH_PORT:-2230}"

REPO_ROOT="$(git rev-parse --show-toplevel)"
Y_CLUSTER="${Y_CLUSTER:-$REPO_ROOT/dist/y-cluster}"
CACHE_DIR="${CACHE_DIR:-$HOME/.cache/y-cluster-qemu}"
EXPORT_DIR=$(mktemp -d -p /tmp e2e-export.XXXXXX)
CFG_DIR=$(mktemp -d -p /tmp e2e-config.XXXXXX)

stage() { printf '\n=== %s ===\n' "$*"; }

cleanup() {
    set +e
    if [[ -f "$EXPORT_DIR/imported.pid" ]]; then
        local imp_pid
        imp_pid=$(cat "$EXPORT_DIR/imported.pid" 2>/dev/null)
        if [[ -n "$imp_pid" ]] && kill -0 "$imp_pid" 2>/dev/null; then
            echo "stopping imported qemu (pid $imp_pid)"
            kill -TERM "$imp_pid" 2>/dev/null # y-script-lint:disable=or-true # not relevant here
            sleep 2
            kill -KILL "$imp_pid" 2>/dev/null # y-script-lint:disable=or-true # may already be gone
        fi
    fi
}
trap cleanup EXIT

for tool in go qemu-system-x86_64 kubectl ssh ssh-keygen curl virt-sysprep; do
    command -v "$tool" >/dev/null \
        || { echo "missing required tool: $tool" >&2; exit 1; }
done

# virt-sysprep on Ubuntu fails before it touches the qcow2 if it
# can't read /boot/vmlinuz-* (libguestfs builds a tiny appliance VM
# with the host kernel via supermin). Ubuntu installs kernel images
# 0600 root, so non-root invocations bail with an opaque
# "supermin exited with error status 1". Surface the fix here.
if ! [ -r /boot/vmlinuz-"$(uname -r)" ]; then
    cat >&2 <<EOF
/boot/vmlinuz-$(uname -r) is not readable; virt-sysprep will fail.
Fix one of:
  sudo chmod +r /boot/vmlinuz-*                                      # ephemeral
  sudo dpkg-statoverride --update --add root root 0644 /boot/vmlinuz-$(uname -r)  # persistent across kernel updates
EOF
    exit 1
fi

# === 1. Build dev binary ===
stage "building dev binary -> $Y_CLUSTER"
mkdir -p "$(dirname "$Y_CLUSTER")"
( cd "$REPO_ROOT" && go build -o "$Y_CLUSTER" ./cmd/y-cluster )

# === 2. Provision the build-side appliance ===
# Idempotent re-run: tear down any leftover from a prior failed run.
stage "tearing down any leftover $NAME cluster"
# We need the config in place for teardown to find the cluster, so
# write it BEFORE the teardown attempt. teardown is idempotent
# (no-op when the cluster doesn't exist) so re-entry is safe.
# YAML emission omits any port the operator didn't override, letting
# y-cluster's Go binary apply its own defaults (sshPort=2222,
# portForwards={6443:6443, 80:80, 443:443}).
{
    echo "provider: qemu"
    echo "name: $NAME"
    echo "context: $NAME"
    [ -n "${APP_SSH_PORT:-}" ] && printf 'sshPort: "%s"\n' "$APP_SSH_PORT"
    echo 'memory: "4096"'
    echo 'cpus: "2"'
    echo 'diskSize: "40G"'
    if [ -n "${APP_HTTP_PORT:-}" ] || [ -n "${APP_API_PORT:-}" ]; then
        echo "portForwards:"
        [ -n "${APP_API_PORT:-}" ] && printf '  - host: "%s"\n    guest: "6443"\n' "$APP_API_PORT"
        [ -n "${APP_HTTP_PORT:-}" ] && printf '  - host: "%s"\n    guest: "80"\n' "$APP_HTTP_PORT"
    fi
} > "$CFG_DIR/y-cluster-provision.yaml"

"$Y_CLUSTER" teardown -c "$CFG_DIR" || true # y-script-lint:disable=or-true # idempotent re-entry: missing cluster is not an error
rm -f "$CACHE_DIR/$NAME".* "$CACHE_DIR/$NAME-"*

stage "provisioning appliance ($NAME) -- k3s + Envoy Gateway only"
"$Y_CLUSTER" provision -c "$CFG_DIR"

# === 3. Customer install via kubectl ===
# This deliberately uses kubectl, not `y-cluster echo deploy`. The
# pipeline below is exactly the shape the per-customer install path
# will take (render manifests, kubectl apply against the live
# cluster). y-cluster has no special case for the workload here.
stage "installing echo workload (Envoy Gateway + HTTPRoute)"
"$Y_CLUSTER" echo render \
    | kubectl --context="$NAME" apply --server-side --field-manager=customer-install -f -
kubectl --context="$NAME" -n y-cluster wait \
    --for=condition=Available deployment/echo --timeout=180s

# Stateful workload: VersityGW (S3-over-posix gateway) backed by a
# 1Gi local-path PVC. Tests the persistence path that the simpler
# echo workload skips.
stage "installing VersityGW StatefulSet via yconverge"
"$Y_CLUSTER" yconverge --context="$NAME" \
    -k "$REPO_ROOT/testdata/appliance-stateful/base"

# === 4. Build-side smoketest ===
stage "build-side smoketest: echo + s3"
probe() {
    local what=$1 url=$2 attempts=${3:-30}
    local out
    out=$(mktemp)
    for i in $(seq 1 "$attempts"); do
        if curl -fsS --max-time 8 -o "$out" -w "  $what HTTP %{http_code}\n" "$url"; then
            rm -f "$out"
            return 0
        fi
        echo "  $what attempt $i/$attempts: no answer yet"
        sleep 5
    done
    echo "$what smoketest never succeeded; aborting" >&2
    rm -f "$out"
    return 1
}
probe echo "http://127.0.0.1:${APP_HTTP_PORT:-80}/q/envoy/echo"
probe s3   "http://127.0.0.1:${APP_HTTP_PORT:-80}/s3/health"

# === 5. stop + prepare-export ===
# y-cluster stop owns the graceful guest shutdown (ssh
# poweroff -> wait for qemu exit -> SIGTERM/SIGKILL fallback).
# Without that, qemu's SIGTERM exits in ~200ms and the guest's
# k3s/containerd state isn't flushed, leaving zero-byte
# overlayfs snapshot files on the qcow2 and "exec format error"
# crash loops on the imported boot.
stage "stopping cluster ($NAME)"
"$Y_CLUSTER" stop --context="$NAME"

stage "prepare-export ($NAME)"
"$Y_CLUSTER" prepare-export --context="$NAME"

# === 6. y-cluster export -> bundle dir ===
# Produces a flattened, self-contained qcow2 (no backing file)
# plus the keypair plus a README. EXPORT_DIR was created by
# mktemp; the export subcommand refuses to write into a
# non-empty dir, so remove that dir and re-create it after the
# export.
BUNDLE_DIR="$EXPORT_DIR/bundle"
stage "exporting bundle to $BUNDLE_DIR (--format=qcow2)"
"$Y_CLUSTER" export --context="$NAME" --format=qcow2 "$BUNDLE_DIR"
ls -la "$BUNDLE_DIR/"
echo "  qemu-img info on the bundled disk:"
qemu-img info "$BUNDLE_DIR/$NAME.qcow2" | grep -E '^(file format|virtual size|disk size|backing)' | sed 's/^/    /'

# === 7. Customer-side: raw qemu against the bundled disk ===
# No y-cluster involvement here -- just qemu-system-x86_64
# pointed at the bundled qcow2 + the bundled key. This proves
# the bundle is genuinely self-contained: any host that can run
# qemu (with the cloud image NOT present at the build path)
# would boot it.
stage "booting bundled qcow2 via raw qemu (host ports $IMP_SSH_PORT -> :22, $IMP_HTTP_PORT -> :80)"
qemu-system-x86_64 \
    -name "$NAME-imported" \
    -machine accel=kvm -cpu host \
    -smp 2 -m 4096 \
    -drive "file=$BUNDLE_DIR/$NAME.qcow2,format=qcow2,if=virtio" \
    -netdev "user,id=n0,hostfwd=tcp::$IMP_SSH_PORT-:22,hostfwd=tcp::$IMP_HTTP_PORT-:80" \
    -device virtio-net-pci,netdev=n0 \
    -serial "file:$EXPORT_DIR/console.log" \
    -display none \
    -daemonize \
    -pidfile "$EXPORT_DIR/imported.pid"
echo "  imported pid: $(cat "$EXPORT_DIR/imported.pid")"

# === 8. Wait for SSH ===
SSH_OPTS="-i $BUNDLE_DIR/$NAME-ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o ConnectTimeout=5"
echo "  waiting for ssh"
ssh_up=0
for i in $(seq 1 60); do
    # shellcheck disable=SC2086
    if ssh $SSH_OPTS -p "$IMP_SSH_PORT" ystack@127.0.0.1 'true' 2>/dev/null; then
        ssh_up=1
        echo "  ssh up after $i tries"
        break
    fi
    sleep 5
done
if [[ $ssh_up -eq 0 ]]; then
    echo "imported instance ssh never came up; console log:" >&2
    tail -50 "$EXPORT_DIR/console.log" >&2
    exit 1
fi

# === 9. Imported smoketest ===
# Both endpoints must come back: echo (stateless) proves the
# Envoy Gateway data plane is up, /s3/health (StatefulSet against
# the local-path PV that lives on the appliance disk) proves the
# stateful workload survived the export -> bundle -> raw-qemu boot.
stage "imported-side smoketest: echo + s3"
imp_probe() {
    local what=$1 url=$2 attempts=${3:-60}
    local out
    out=$(mktemp)
    for i in $(seq 1 "$attempts"); do
        if curl -fsS --max-time 8 -o "$out" -w "  $what HTTP %{http_code}\n" "$url"; then
            echo
            echo "=== imported $what response (head) ==="
            head -25 "$out"
            echo
            rm -f "$out"
            return 0
        fi
        echo "  $what attempt $i/$attempts: no answer yet"
        sleep 5
    done
    rm -f "$out"
    return 1
}
if imp_probe echo "http://127.0.0.1:$IMP_HTTP_PORT/q/envoy/echo" \
    && imp_probe s3 "http://127.0.0.1:$IMP_HTTP_PORT/s3/health"; then
    echo "=== success: round-trip works (echo + s3) ==="
    echo "  imported echo reachable at: http://127.0.0.1:$IMP_HTTP_PORT/q/envoy/echo"
    echo "  imported s3 reachable at:   http://127.0.0.1:$IMP_HTTP_PORT/s3/health"
    echo "  imported ssh: ssh -p $IMP_SSH_PORT -i $BUNDLE_DIR/$NAME-ssh ystack@127.0.0.1"
    echo "  build-side cluster preserved (KEEP_BUILD=1) -- destroy with: $Y_CLUSTER teardown -c $CFG_DIR"
    if [[ -z "${KEEP_BUILD:-}" ]]; then
        "$Y_CLUSTER" teardown -c "$CFG_DIR" 2>/dev/null # y-script-lint:disable=or-true # success path cleanup
    fi
    exit 0
fi

# === Diagnosis on failure ===
echo >&2
echo "imported smoketest never returned 200. Diagnostics:" >&2
# shellcheck disable=SC2086
ssh $SSH_OPTS -p "$IMP_SSH_PORT" ystack@127.0.0.1 \
    'echo ===nodes===; sudo k3s kubectl get nodes -o wide;
     echo ===pods===; sudo k3s kubectl get pods -A;
     echo ===k3s status===; systemctl is-active k3s;
     echo ===listen===; sudo ss -tlnp | grep -E ":(80|443|6443)\b"
    ' >&2 # y-script-lint:disable=or-true # diagnostic best-effort
echo "  imported ssh: ssh -p $IMP_SSH_PORT -i $BUNDLE_DIR/$NAME-ssh ystack@127.0.0.1" >&2
echo "  console log: $EXPORT_DIR/console.log" >&2
exit 1
