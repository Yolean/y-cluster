#!/usr/bin/env bash
# Build a y-cluster appliance and pause for hands-on testing
# before exporting a VirtualBox-friendly bundle.
#
# Same provision shape as scripts/e2e-appliance-export-import.sh:
# qemu provider, k3s + Envoy Gateway, echo workload, VersityGW
# StatefulSet (covers stateful PV path). Then it stops, prints
# kubectl + ssh access info, and waits for the operator to
# confirm before running prepare-export + export.
#
# Why interactive: the VirtualBox handoff is precious. We want
# the operator to sanity-check the live cluster before we lock
# the disk for export and (optionally) tear it down. Yes lets
# y-cluster prepare-export + export run; "no" leaves the cluster
# up for further poking (and prints the teardown command).
#
# The bundled VMDK uses subformat=monolithicSparse, which
# imports more cleanly under VirtualBox's "Use Existing Virtual
# Hard Disk File" than the streamOptimized default that ships
# for ESXi. The README inside the bundle documents both.

[ -z "$DEBUG" ] || set -x
set -eo pipefail

YHELP='appliance-build-virtualbox.sh - provision -> install -> pause -> export VirtualBox-friendly VMDK

Usage: appliance-build-virtualbox.sh [bundle-dir]

Positional:
  bundle-dir   Where to write the export bundle. Default:
               ./dist/appliance-virtualbox/<NAME>-<timestamp>

Environment:
  NAME             Appliance name (default: appliance-virtualbox)
  APP_HTTP_PORT    Host port -> guest 80 (default: 80)
  APP_API_PORT     Host port -> guest 6443 (default: 39443)
  APP_SSH_PORT     Host port -> guest 22 (default: 2229)
  Y_CLUSTER        Path to dev binary (default: ./dist/y-cluster)
  CACHE_DIR        Where y-cluster keeps its qcow2 (default: ~/.cache/y-cluster-qemu)
  KEEP_CLUSTER     Set to keep the cluster alive after export (default: tear it down)
  SKIP_PROVISION   Set to skip provision + install (resume into the prompt against
                   an already-running cluster of the same NAME)
  ASSUME_YES       Set to skip the interactive prompt and proceed to export

Dependencies:
  go, qemu-system-x86_64, qemu-img, kubectl, ssh, ssh-keygen, curl, virt-sysprep
'

case "${1:-}" in
  help) echo "$YHELP"; exit 0 ;;
  --help) echo "$YHELP"; exit 0 ;;
  -h) echo "$YHELP"; exit 0 ;;
esac

NAME="${NAME:-appliance-virtualbox}"
APP_HTTP_PORT="${APP_HTTP_PORT:-80}"
APP_API_PORT="${APP_API_PORT:-39443}"
APP_SSH_PORT="${APP_SSH_PORT:-2229}"

REPO_ROOT="$(git rev-parse --show-toplevel)"
Y_CLUSTER="${Y_CLUSTER:-$REPO_ROOT/dist/y-cluster}"
CACHE_DIR="${CACHE_DIR:-$HOME/.cache/y-cluster-qemu}"

DEFAULT_BUNDLE="$REPO_ROOT/dist/appliance-virtualbox/$NAME-$(date -u +%Y%m%dT%H%M%SZ)"
BUNDLE_DIR="${1:-$DEFAULT_BUNDLE}"

# CFG_DIR lives OUTSIDE $CACHE_DIR on purpose: the cleanup glob
# below ("$CACHE_DIR/$NAME-"*) would otherwise match a config
# directory whose name starts with $NAME, and rm -f bails on
# directories under set -e. Keep it stable (not mktemp -d) so
# SKIP_PROVISION can resume against an existing cluster.
CFG_DIR="${CFG_DIR:-$HOME/.cache/y-cluster-appliance-build/$NAME}"

stage() { printf '\n=== %s ===\n' "$*"; }

for tool in go qemu-system-x86_64 qemu-img kubectl ssh ssh-keygen curl virt-sysprep; do
    command -v "$tool" >/dev/null \
        || { echo "missing required tool: $tool" >&2; exit 1; }
done

# virt-sysprep needs to read /boot/vmlinuz-* (libguestfs supermin
# builds an appliance VM with the host kernel). Ubuntu installs
# kernel images 0600 root, so non-root invocations bail with an
# opaque "supermin exited with error status 1". Surface the fix.
if ! [ -r /boot/vmlinuz-"$(uname -r)" ]; then
    cat >&2 <<EOF
/boot/vmlinuz-$(uname -r) is not readable; virt-sysprep will fail.
Fix one of:
  sudo chmod +r /boot/vmlinuz-*
  sudo dpkg-statoverride --update --add root root 0644 /boot/vmlinuz-$(uname -r)
EOF
    exit 1
fi

# === Build dev binary ===
stage "building dev binary -> $Y_CLUSTER"
mkdir -p "$(dirname "$Y_CLUSTER")"
( cd "$REPO_ROOT" && go build -o "$Y_CLUSTER" ./cmd/y-cluster )

# === Config (always written; teardown + prepare-export need it) ===
mkdir -p "$CFG_DIR"
cat > "$CFG_DIR/y-cluster-provision.yaml" <<EOF
provider: qemu
name: $NAME
context: $NAME
sshPort: "$APP_SSH_PORT"
memory: "4096"
cpus: "2"
diskSize: "40G"
portForwards:
  - host: "$APP_API_PORT"
    guest: "6443"
  - host: "$APP_HTTP_PORT"
    guest: "80"
EOF

if [[ -z "${SKIP_PROVISION:-}" ]]; then
    stage "tearing down any leftover $NAME cluster"
    "$Y_CLUSTER" teardown -c "$CFG_DIR" || true # y-script-lint:disable=or-true # idempotent re-entry: missing cluster is not an error
    rm -f "$CACHE_DIR/$NAME".* "$CACHE_DIR/$NAME-"*

    stage "provisioning $NAME (k3s + Envoy Gateway)"
    "$Y_CLUSTER" provision -c "$CFG_DIR"

    stage "installing echo workload"
    "$Y_CLUSTER" echo render \
        | kubectl --context="$NAME" apply --server-side --field-manager=customer-install -f -
    kubectl --context="$NAME" -n y-cluster wait \
        --for=condition=Available deployment/echo --timeout=180s

    stage "installing VersityGW StatefulSet via yconverge"
    "$Y_CLUSTER" yconverge --context="$NAME" \
        -k "$REPO_ROOT/testdata/appliance-stateful/base"

    stage "smoketest: echo + s3"
    probe() {
        local what=$1 url=$2 attempts=${3:-30}
        for i in $(seq 1 "$attempts"); do
            if curl -fsS --max-time 8 -o /dev/null -w "  $what HTTP %{http_code}\n" "$url"; then
                return 0
            fi
            echo "  $what attempt $i/$attempts: no answer yet"
            sleep 5
        done
        echo "$what smoketest never succeeded; aborting" >&2
        return 1
    }
    probe echo "http://127.0.0.1:$APP_HTTP_PORT/q/envoy/echo"
    probe s3   "http://127.0.0.1:$APP_HTTP_PORT/s3/health"
else
    stage "SKIP_PROVISION set; resuming against existing $NAME cluster"
fi

# === Interactive pause for hands-on testing ===
SSH_KEY="$CACHE_DIR/$NAME-ssh"

cat <<EOF

================================================================
Cluster $NAME is up and ready for testing.

  HTTP (echo + s3):  http://127.0.0.1:$APP_HTTP_PORT/q/envoy/echo
                     http://127.0.0.1:$APP_HTTP_PORT/s3/health
  Kubernetes API:    https://127.0.0.1:$APP_API_PORT
  kubectl context:   $NAME

Quick checks:
  kubectl --context=$NAME get nodes -o wide
  kubectl --context=$NAME get pods -A
  kubectl --context=$NAME -n appliance-stateful get statefulset,pvc,pv
  curl -sf http://127.0.0.1:$APP_HTTP_PORT/q/envoy/echo

SSH into the VM (passwordless sudo as ystack):
  ssh -i $SSH_KEY -p $APP_SSH_PORT \\
      -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \\
      ystack@127.0.0.1

Once you have finished poking around:
  - Continue (y) to stop the VM, prepare-export, and write a
    VirtualBox-friendly VMDK bundle to:
      $BUNDLE_DIR
  - Abort (n) to leave the cluster running. Tear down later with:
      $Y_CLUSTER teardown -c $CFG_DIR
================================================================

EOF

if [[ -n "${ASSUME_YES:-}" ]]; then
    answer=y
    echo "ASSUME_YES set; proceeding to export"
else
    read -r -p "Proceed to export? [y/N] " answer
fi
case "${answer,,}" in
    y|yes) ;;
    *) echo "aborting; cluster left running. Teardown with: $Y_CLUSTER teardown -c $CFG_DIR"; exit 0 ;;
esac

# === Stop + prepare-export ===
stage "stopping cluster ($NAME)"
"$Y_CLUSTER" stop --context="$NAME"

stage "prepare-export ($NAME)"
"$Y_CLUSTER" prepare-export --context="$NAME"

# === Export OVA (VirtualBox-friendly) ===
# OVA = uncompressed tar of (OVF descriptor + streamOptimized
# VMDK). VirtualBox's File -> Import Appliance wizard accepts
# only OVF / OVA, NOT raw VMDK -- so we ship OVA. The OVF
# carries the CPU/RAM/NIC hints; VirtualBox just needs port
# forwards added post-import.
stage "exporting OVA (VirtualBox-importable) -> $BUNDLE_DIR"
mkdir -p "$(dirname "$BUNDLE_DIR")"
"$Y_CLUSTER" export \
    --context="$NAME" \
    --format=ova \
    "$BUNDLE_DIR"

ls -la "$BUNDLE_DIR/"
echo
echo "  bundled .ova members:"
tar tvf "$BUNDLE_DIR/$NAME.ova" | sed 's/^/    /'

cat <<EOF

================================================================
Bundle ready at: $BUNDLE_DIR

Files:
$(ls -1 "$BUNDLE_DIR" | sed 's/^/  /')

VirtualBox import:
  1. File -> Import Appliance -> select $BUNDLE_DIR/$NAME.ova
  2. Confirm CPU / RAM / disk on the wizard (defaults come
     from the OVF: $(awk '/cpus/{print $2}' "$CFG_DIR/y-cluster-provision.yaml") vCPU, $(awk '/memory/{print $2}' "$CFG_DIR/y-cluster-provision.yaml") MiB RAM)
  3. After import: Network -> Adapter 1 -> Advanced -> Port
     Forwarding, add:
       ssh    TCP  host 2222 -> guest 22
       http   TCP  host 8080 -> guest 80
       https  TCP  host 8443 -> guest 443
  4. Start. SSH key + access details in $BUNDLE_DIR/README.md
================================================================
EOF

if [[ -z "${KEEP_CLUSTER:-}" ]]; then
    stage "tearing down build-side cluster (set KEEP_CLUSTER=1 to keep it)"
    "$Y_CLUSTER" teardown -c "$CFG_DIR" 2>/dev/null || true # y-script-lint:disable=or-true # cleanup best-effort
fi
