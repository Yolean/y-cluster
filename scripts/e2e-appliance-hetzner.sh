#!/usr/bin/env bash
# e2e: build a y-cluster appliance snapshot on Hetzner Cloud via
# Packer, boot a server from it, and verify the echo HTTPRoute
# answers over the public IP.
#
# Replaces the older dd-via-rescue path (qemu-img convert + zstd +
# dd /dev/sda from rescue mode) which broke at the "TCP/22 reachable,
# no SSH banner" stage we couldn't diagnose without out-of-band
# console. Packer's hcloud builder handles base-image / partition
# layout / network drivers natively, so the path "image boots on
# Hetzner" is no longer something we have to engineer ourselves --
# we get it for free by building on Hetzner from the start.
#
# Local appliance vs Hetzner appliance:
#   - Local dev still uses `y-cluster provision` against qemu and
#     prepare-export when the operator wants a portable qcow2.
#   - Production Hetzner deploys go through this script, which
#     produces a reusable snapshot a fleet can clone from.
#
# Stages:
#   1. Build a current-arch y-cluster dev binary into ./dist (the
#      Packer template uploads it onto the build host).
#   2. `packer init` + `packer build` of e2e-appliance-hetzner.pkr.hcl.
#      Packer creates a temporary cx23 in hel1, runs the workload
#      install, snapshots, and tears the temporary server down.
#   3. Resolve the snapshot ID from `hcloud image list`.
#   4. Create a fresh server from the snapshot (idempotent: deletes
#      any matching $SERVER_NAME first).
#   5. Probe http://<public-ip>/q/envoy/echo until it answers.
#
# Prerequisites:
#   - HCLOUD_TOKEN sourced from $ENV_FILE (set in .env or shell env)
#   - hcloud CLI on PATH (apt install hcloud OR snap install hcloud)
#   - packer on PATH (apt install packer after adding HashiCorp's
#     repo, OR download from releases.hashicorp.com)
#   - go (to build the dev binary), curl, ssh-keygen
#
# On success: prints the public IP and leaves the server running so
# the operator can poke at it. Teardown is manual:
#   hcloud server delete $SERVER_NAME
#   hcloud image delete <snapshot-id>     # optional: snapshot is reusable
# The script is idempotent on re-run -- it deletes any matching
# server/key first and starts fresh from a new snapshot.

[ -z "$DEBUG" ] || set -x
set -eo pipefail

YHELP='e2e-appliance-hetzner.sh - Build a y-cluster appliance snapshot on Hetzner Cloud and verify it serves traffic

Usage: e2e-appliance-hetzner.sh

Environment:
  HCLOUD_TOKEN       Hetzner Cloud API token (sourced from ENV_FILE)
  ENV_FILE           Path to env file with HCLOUD_TOKEN (set in .env or shell env; required)
  SERVER_NAME        Server name to create (default: y-cluster-appliance-test)
  SERVER_TYPE        Hetzner server type (default: cx23)
  SERVER_LOCATION    Hetzner location (default: hel1)
  SNAPSHOT_NAME      Snapshot description used as Packer output name
  Y_CLUSTER          Path to dev binary (default: ./dist/y-cluster)
  DEBUG              Set non-empty to enable bash trace

Dependencies:
  packer, hcloud, go, ssh, ssh-keygen, curl

Exit codes:
  0  Success: appliance reachable on public IP
  1  Missing prereq, packer build failure, or echo never answered
'

case "${1:-}" in
  help) echo "$YHELP"; exit 0 ;;
  --help) echo "$YHELP"; exit 0 ;;
  -h) echo "$YHELP"; exit 0 ;;
esac

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
if [[ -f "$REPO_ROOT/.env" ]]; then
    set -o allexport; . "$REPO_ROOT/.env"; set +o allexport
fi

: "${ENV_FILE:?set ENV_FILE in .env or shell env}"

if [[ ! -f "$ENV_FILE" ]]; then
    echo "missing env file: $ENV_FILE" >&2
    echo "expected at minimum: HCLOUD_TOKEN=<hetzner-cloud-api-token>" >&2
    exit 1
fi
# shellcheck disable=SC1090
source "$ENV_FILE"
[[ -n "${HCLOUD_TOKEN:-}" ]] || { echo "HCLOUD_TOKEN not set in $ENV_FILE" >&2; exit 1; }
export HCLOUD_TOKEN

# Tunables. Defaults match the Packer template's; override here when
# experimenting with alternate locations / instance types.
SERVER_NAME="${SERVER_NAME:-y-cluster-appliance-test}"
SERVER_TYPE="${SERVER_TYPE:-cx23}"
SERVER_LOCATION="${SERVER_LOCATION:-hel1}"
SNAPSHOT_NAME="${SNAPSHOT_NAME:-y-cluster-appliance-$(date -u +%Y%m%d-%H%M%S)}"

Y_CLUSTER="${Y_CLUSTER:-$REPO_ROOT/dist/y-cluster}"
PACKER_TEMPLATE="$REPO_ROOT/scripts/e2e-appliance-hetzner.pkr.hcl"

for tool in packer hcloud go ssh ssh-keygen curl; do
    command -v "$tool" >/dev/null \
        || { echo "missing required tool: $tool" >&2; exit 1; }
done

stage() { printf '\n=== %s ===\n' "$*"; }

confirm() {
    local prompt=$1
    if [[ -n "${ASSUME_YES:-}" ]]; then
        echo "ASSUME_YES set; proceeding ($prompt)"
        return 0
    fi
    read -r -p "$prompt [y/N] " answer
    case "${answer,,}" in
        y|yes) return 0 ;;
        *) return 1 ;;
    esac
}

cat <<'WARN'

================================================================
DEPRECATION WARNING

scripts/e2e-appliance-hetzner.sh's role is changing.

Today this script tests the legacy "Hetzner-as-export-mode"
shape: build an appliance inside a Hetzner VM via Packer,
snapshot, boot a server from the snapshot. This shape doesn't
match the appliance contract (Hetzner Cloud has no public API
for uploading a locally-built disk).

Replacement plan:
  - This script will be REPURPOSED once a Hetzner PROVISIONER
    exists in pkg/provision/hetzner/ (alongside qemu / docker /
    multipass). Repurposed scope: end-to-end test of
    `y-cluster provision -c hetzner.yaml` -> snapshot ->
    instantiate-from-snapshot. The Packer-build half goes away;
    the snapshot becomes a regular y-cluster lifecycle artefact.
  - The local-build appliance contract is moving to
    scripts/appliance-qemu-to-gcp.sh (Hetzner's API can't
    accept a local disk; GCP's `gcloud compute images import`
    can).

This script still runs. It still passes. But its purpose is
about to flip; treat results from a green run today as
"Packer build still works" rather than "appliance contract
verified".
================================================================

WARN
confirm "Proceed with the legacy Packer e2e anyway?" \
    || { echo "aborted; no changes made."; exit 0; }

# === 1. Build the dev binary the Packer template uploads ===
stage "building linux/amd64 dev binary -> $Y_CLUSTER"
mkdir -p "$(dirname "$Y_CLUSTER")"
( cd "$REPO_ROOT" && GOOS=linux GOARCH=amd64 go build -o "$Y_CLUSTER" ./cmd/y-cluster )

# === 2. render stateful manifest + packer init + build ===
# Packer's file provisioner doesn't recursively upload
# directories cleanly across all builder/communicator
# combinations (hcloud's SSH communicator scp's a directory
# argument as a single path and gets back "Is a directory").
# Pre-render the kustomize base on the host into one yaml file
# and ship that single file to the build VM instead. Same end
# result, no scp recursion concerns.
# The fixture is split into two yconverge modules (namespace
# first, then the StatefulSet+Service+HTTPRoute) so the local
# convergence path can express the dep with a cue import. The
# Hetzner Packer flow doesn't run yconverge inside the build
# VM (would need the y-cluster binary on the VM) -- it stays
# kubectl-apply, but we render BOTH bases and concat. kubectl
# applies a Namespace ahead of namespaced resources in the
# same -f input, so a single concat'd file converges in the
# right order.
STATEFUL_MANIFEST=$(mktemp -t appliance-stateful.XXXXXX.yaml)
{
    kubectl kustomize "$REPO_ROOT/testdata/appliance-stateful/namespace"
    echo '---'
    kubectl kustomize "$REPO_ROOT/testdata/appliance-stateful/base"
} > "$STATEFUL_MANIFEST"

# y-cluster's bundled local-path-provisioner manifest (replaces
# k3s's disabled local-storage). Rendered with the same defaults
# the Go-side provisioners install so an appliance built via
# Hetzner Packer ends up indistinguishable from one built locally.
LOCALSTORAGE_MANIFEST=$(mktemp -t y-cluster-localstorage.XXXXXX.yaml)
"$Y_CLUSTER" localstorage render > "$LOCALSTORAGE_MANIFEST"

stage "packer init"
packer init "$PACKER_TEMPLATE"

stage "packer build (creates a temporary $SERVER_TYPE in $SERVER_LOCATION, snapshots, deletes)"
packer build \
    -var "snapshot_name=$SNAPSHOT_NAME" \
    -var "server_type=$SERVER_TYPE" \
    -var "location=$SERVER_LOCATION" \
    -var "y_cluster_binary=$Y_CLUSTER" \
    -var "prepare_script=$REPO_ROOT/pkg/provision/qemu/prepare_inguest.sh" \
    -var "stateful_manifest=$STATEFUL_MANIFEST" \
    -var "localstorage_manifest=$LOCALSTORAGE_MANIFEST" \
    "$PACKER_TEMPLATE"

# === 3. Resolve snapshot ID ===
# Packer's hcloud builder prints the snapshot ID at the end of build
# but doesn't expose it in a stable machine-readable way without a
# manifest post-processor. hcloud image list is the simpler path.
stage "resolving snapshot id for $SNAPSHOT_NAME"
SNAPSHOT_ID=$(hcloud image list \
    --type=snapshot \
    --selector="purpose=y-cluster-appliance" \
    --output=json \
    | python3 -c "
import json, sys
images = json.load(sys.stdin)
matches = [i for i in images if i.get('description') == '$SNAPSHOT_NAME']
if not matches:
    sys.exit('no snapshot named $SNAPSHOT_NAME found')
print(matches[0]['id'])
")
echo "  snapshot id: $SNAPSHOT_ID"

# === 4. Create a fresh ssh keypair + server from the snapshot ===
KEY_DIR=$(mktemp -d)
trap 'rm -rf "$KEY_DIR" "$STATEFUL_MANIFEST" "$LOCALSTORAGE_MANIFEST"' EXIT
ssh-keygen -t ed25519 -N '' -C "$SERVER_NAME-$$" -f "$KEY_DIR/id" -q
KEY_NAME="$SERVER_NAME"

stage "tearing down any leftover server / key from a prior run"
hcloud server delete "$SERVER_NAME" 2>/dev/null || true # y-script-lint:disable=or-true # idempotent cleanup: missing server is not an error
hcloud ssh-key delete "$KEY_NAME" 2>/dev/null || true # y-script-lint:disable=or-true # idempotent cleanup: missing key is not an error

stage "registering ssh public key as $KEY_NAME"
hcloud ssh-key create --name "$KEY_NAME" --public-key-from-file "$KEY_DIR/id.pub" >/dev/null

stage "creating $SERVER_NAME from snapshot $SNAPSHOT_ID"
hcloud server create \
    --name "$SERVER_NAME" \
    --type "$SERVER_TYPE" \
    --image "$SNAPSHOT_ID" \
    --location "$SERVER_LOCATION" \
    --ssh-key "$KEY_NAME" \
    >/dev/null
PUBLIC_IP=$(hcloud server ip "$SERVER_NAME")
echo "  public ip: $PUBLIC_IP"

# === 5. Wait for sshd, then probe the echo HTTPRoute ===
SSH_OPTS="-i $KEY_DIR/id -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o ConnectTimeout=5"
echo "  waiting for ssh on $PUBLIC_IP:22"
for _ in $(seq 1 60); do
    # shellcheck disable=SC2086
    if ssh $SSH_OPTS root@"$PUBLIC_IP" 'true' 2>/dev/null; then
        break
    fi
    sleep 5
done

# Cold boot from snapshot: cloud-init runs (~30s), k3s.service starts
# for the first time, the envoy gateway controller comes up, the
# envoy proxy data plane comes up, the VersityGW StatefulSet
# rebinds its PV, klipper-lb binds :80. The probe loop is long
# enough to cover the whole chain on a fresh cx23.
probe() {
    local what=$1 url=$2 attempts=${3:-60}
    local out
    out=$(mktemp)
    for i in $(seq 1 "$attempts"); do
        if curl -fsS --max-time 8 -o "$out" -w "  $what HTTP %{http_code}\n" "$url"; then
            echo
            echo "=== $what response (head) ==="
            head -25 "$out"
            echo
            rm -f "$out"
            return 0
        fi
        echo "  $what attempt $i/$attempts: no answer yet"
        sleep 10
    done
    rm -f "$out"
    return 1
}

stage "probing http://$PUBLIC_IP -- echo + s3"
if probe echo "http://$PUBLIC_IP/q/envoy/echo" \
    && probe s3 "http://$PUBLIC_IP/s3/health"; then
    echo "=== success: cloned server serves echo + s3 ==="
    echo "  echo: http://$PUBLIC_IP/q/envoy/echo"
    echo "  s3:   http://$PUBLIC_IP/s3/health"
    echo "  ssh: ssh -i $KEY_DIR/id root@$PUBLIC_IP"
    echo "  destroy: hcloud server delete $SERVER_NAME"
    echo "  snapshot ($SNAPSHOT_ID) preserved -- reuse with: hcloud server create --image=$SNAPSHOT_ID ..."
    exit 0
fi

echo >&2
echo "echo never answered within $((ATTEMPTS * 10))s. server still up for diagnosis:" >&2
# shellcheck disable=SC2086
ssh $SSH_OPTS root@"$PUBLIC_IP" 'systemctl is-active k3s; kubectl get pods -A 2>&1 | head -30' >&2 \
    || true # y-script-lint:disable=or-true # diagnostic best-effort -- main failure already exits 1
echo "  ssh: ssh -i $KEY_DIR/id root@$PUBLIC_IP" >&2
echo "  destroy: hcloud server delete $SERVER_NAME" >&2
exit 1
