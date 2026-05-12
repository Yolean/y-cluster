#!/usr/bin/env bash
# Build a y-cluster appliance interactively: stand up a local
# qemu cluster with the same fixtures we'll ship, give the
# operator a chance to poke at it, then on confirm run a
# Packer-built Hetzner snapshot and provision a server from
# it. Shows ssh + curl details for both stages.
#
# Why two clusters: the local one is for hands-on verification
# (kubectl / ssh / poke). The Hetzner one is the actual handoff.
# They're built from the same testdata fixtures, so verifying
# locally proves the fixture set; Packer rebuilds the snapshot
# fresh inside Hetzner. No round-trip artefact transfer between
# the two -- they're independent builds with shared inputs.
#
# Two confirmations:
#   1. "Local cluster looks good -- build Hetzner snapshot?"
#   2. "Snapshot ready -- create server from snapshot?"
# Either prompt aborts non-destructively. Aborting at (1)
# leaves the local cluster up; aborting at (2) leaves the
# Hetzner snapshot in your project for later use.

[ -z "$DEBUG" ] || set -x
set -eo pipefail

YHELP='appliance-build-hetzner.sh - local verify -> confirm -> Packer snapshot -> confirm -> Hetzner server

Usage: appliance-build-hetzner.sh

Environment:
  ENV_FILE          Hetzner credentials file (set in .env or shell env; required)
  HCLOUD_TOKEN      Hetzner Cloud API token (sourced from ENV_FILE)
  NAME              Local cluster name (default: appliance-hetzner-build)
  APP_HTTP_PORT     Override host port for guest 80 (y-cluster default: 80)
  APP_HTTPS_PORT    Override host port for guest 443 (y-cluster default: 443)
  APP_API_PORT      Override host port for guest 6443 (y-cluster default: 6443)
  APP_SSH_PORT      Override host port for guest 22 (y-cluster default: 2222)
  SERVER_NAME       Hetzner server name (default: y-cluster-appliance)
  SERVER_TYPE       Hetzner server type (default: cx23)
  SERVER_LOCATION   Hetzner location (default: hel1)
  SNAPSHOT_NAME     Packer snapshot description (default: y-cluster-appliance-<UTC>)
  Y_CLUSTER         Path to dev binary (default: ./dist/y-cluster)
  CACHE_DIR         Where y-cluster keeps its qcow2 (default: ~/.cache/y-cluster-qemu)
  KEEP_LOCAL        Set to keep the local cluster after Hetzner deploy (default: tear down)
  ASSUME_YES        Set to skip BOTH confirmations and proceed end-to-end

Dependencies:
  go, qemu-system-x86_64, kubectl, ssh, ssh-keygen, curl, packer, hcloud
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

NAME="${NAME:-appliance-hetzner-build}"
SERVER_NAME="${SERVER_NAME:-y-cluster-appliance}"
SERVER_TYPE="${SERVER_TYPE:-cx23}"
SERVER_LOCATION="${SERVER_LOCATION:-hel1}"
SNAPSHOT_NAME="${SNAPSHOT_NAME:-y-cluster-appliance-$(date -u +%Y%m%d-%H%M%S)}"

Y_CLUSTER="${Y_CLUSTER:-$REPO_ROOT/dist/y-cluster}"
CACHE_DIR="${CACHE_DIR:-$HOME/.cache/y-cluster-qemu}"
PACKER_TEMPLATE="$REPO_ROOT/scripts/e2e-appliance-hetzner.pkr.hcl"

# Keep CFG_DIR stable + outside CACHE_DIR (the cleanup glob in the
# qemu provisioner would otherwise match this directory and rm -f
# would bail, killing the script under set -e). Same convention as
# scripts/appliance-build-virtualbox.sh.
CFG_DIR="${CFG_DIR:-$HOME/.cache/y-cluster-appliance-build/$NAME}"

# Stable location for the per-deploy ssh key so the operator can
# ssh into the Hetzner server later. Survives across script runs
# unless they delete the file or run with a fresh SERVER_NAME.
HCLOUD_KEY_DIR="$HOME/.cache/y-cluster-appliance-build/hetzner-keys"
HCLOUD_KEY="$HCLOUD_KEY_DIR/$SERVER_NAME"

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

scripts/appliance-build-hetzner.sh is on the way out.

Hetzner Cloud has no public API for uploading custom disk
images, so this script's "build a Hetzner snapshot" stage is
a fresh build inside Hetzner via Packer -- the local-qemu
verification you do first is fixture-equivalence, NOT the same
disk that ships. That mismatches the appliance contract
(local-built disk = disk that boots elsewhere).

Replacement plan:
  - scripts/appliance-qemu-to-gcp.sh (in progress) takes the
    appliance contract path: provision local, export disk,
    upload to GCP via `gcloud compute images import`, boot a
    VM from that uploaded image. Same disk you verified
    locally is the disk GCP runs.
  - scripts/e2e-appliance-hetzner.sh is being repurposed once
    a pkg/provision/hetzner/ provisioner exists; it will then
    cover provision-on-Hetzner -> snapshot -> instantiate as
    an end-to-end test of that provisioner shape.

This script still runs. It still produces a working appliance
on Hetzner. But the artefact you ship is built fresh on
Hetzner, not transferred from your local verification.
================================================================

WARN
confirm "Proceed with the Hetzner Packer flow anyway?" \
    || { echo "aborted; no changes made."; exit 0; }


for tool in go qemu-system-x86_64 kubectl ssh ssh-keygen curl packer hcloud; do
    command -v "$tool" >/dev/null \
        || { echo "missing required tool: $tool" >&2; exit 1; }
done

if [[ ! -f "$ENV_FILE" ]]; then
    echo "missing env file: $ENV_FILE (need HCLOUD_TOKEN)" >&2
    exit 1
fi
# shellcheck disable=SC1090
source "$ENV_FILE"
[[ -n "${HCLOUD_TOKEN:-}" ]] || { echo "HCLOUD_TOKEN not set in $ENV_FILE" >&2; exit 1; }
export HCLOUD_TOKEN

# === Build dev binary (linux/amd64 because Packer uploads it) ===
stage "building linux/amd64 dev binary -> $Y_CLUSTER"
mkdir -p "$(dirname "$Y_CLUSTER")"
( cd "$REPO_ROOT" && GOOS=linux GOARCH=amd64 go build -o "$Y_CLUSTER" ./cmd/y-cluster )

# === Local config ===
mkdir -p "$CFG_DIR"
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
    if [ -n "${APP_HTTP_PORT:-}" ] || [ -n "${APP_HTTPS_PORT:-}" ] || [ -n "${APP_API_PORT:-}" ]; then
        echo "portForwards:"
        [ -n "${APP_API_PORT:-}" ]   && printf '  - host: "%s"\n    guest: "6443"\n' "$APP_API_PORT"
        [ -n "${APP_HTTP_PORT:-}" ]  && printf '  - host: "%s"\n    guest: "80"\n'   "$APP_HTTP_PORT"
        [ -n "${APP_HTTPS_PORT:-}" ] && printf '  - host: "%s"\n    guest: "443"\n'  "$APP_HTTPS_PORT"
    fi
} > "$CFG_DIR/y-cluster-provision.yaml"

# === Stage 1: local provision + install + smoketest ===
stage "tearing down any leftover $NAME cluster"
"$Y_CLUSTER" teardown -c "$CFG_DIR" || true # y-script-lint:disable=or-true # idempotent re-entry: missing cluster is not an error

stage "provisioning local appliance ($NAME) -- k3s + Envoy Gateway"
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
probe_local() {
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
probe_local echo "http://127.0.0.1:${APP_HTTP_PORT:-80}/q/envoy/echo"
probe_local s3   "http://127.0.0.1:${APP_HTTP_PORT:-80}/s3/health"

cat <<EOF

================================================================
Local cluster $NAME is up. Verify with:

  HTTP (echo + s3):  http://127.0.0.1:${APP_HTTP_PORT:-80}/q/envoy/echo
                     http://127.0.0.1:${APP_HTTP_PORT:-80}/s3/health
  Kubernetes API:    https://127.0.0.1:${APP_API_PORT:-6443}
  kubectl context:   $NAME

  kubectl --context=$NAME get nodes -o wide
  kubectl --context=$NAME get pods -A
  kubectl --context=$NAME -n appliance-stateful get statefulset,pvc,pv

  ssh -i $CACHE_DIR/$NAME-ssh -p ${APP_SSH_PORT:-2222} \\
      -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \\
      ystack@127.0.0.1

Hetzner deploy uses the same testdata fixtures, rebuilt fresh
on Hetzner via Packer (no artefact transfer between them; this
is an independence-of-builds verification).
================================================================

EOF

confirm "Local cluster looks good -- build Hetzner snapshot?" \
    || { echo "aborted; local cluster left running. Teardown with: $Y_CLUSTER teardown -c $CFG_DIR"; exit 0; }

# === Stage 2: Packer build a Hetzner snapshot ===
# Tear down the local cluster before Packer to free local
# resources -- it's not used in the rest of the flow. Skip
# this if the operator set KEEP_LOCAL.
if [[ -z "${KEEP_LOCAL:-}" ]]; then
    stage "tearing down local cluster (set KEEP_LOCAL=1 to keep it)"
    "$Y_CLUSTER" teardown -c "$CFG_DIR" 2>/dev/null || true # y-script-lint:disable=or-true # cleanup best-effort
fi

# Pre-render the kustomize bases for Packer (the build VM doesn't
# have y-cluster, so it can't run yconverge; concat both module
# outputs into a single kubectl-applyable file). Same shape as
# scripts/e2e-appliance-hetzner.sh.
STATEFUL_MANIFEST=$(mktemp -t appliance-stateful.XXXXXX.yaml)
{
    kubectl kustomize "$REPO_ROOT/testdata/appliance-stateful/namespace"
    echo '---'
    kubectl kustomize "$REPO_ROOT/testdata/appliance-stateful/base"
} > "$STATEFUL_MANIFEST"

LOCALSTORAGE_MANIFEST=$(mktemp -t y-cluster-localstorage.XXXXXX.yaml)
"$Y_CLUSTER" localstorage render > "$LOCALSTORAGE_MANIFEST"

trap 'rm -f "$STATEFUL_MANIFEST" "$LOCALSTORAGE_MANIFEST"' EXIT

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

# Resolve snapshot id from the description we gave Packer.
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

cat <<EOF

================================================================
Snapshot ready: $SNAPSHOT_ID ($SNAPSHOT_NAME)

This snapshot is reusable -- you can clone any number of
servers from it without rebuilding.

Next: create a $SERVER_TYPE server in $SERVER_LOCATION named
"$SERVER_NAME" from this snapshot. Aborting now leaves the
snapshot in your Hetzner project; spin it up later with:

  hcloud server create --name $SERVER_NAME \\
      --type $SERVER_TYPE --location $SERVER_LOCATION \\
      --image $SNAPSHOT_ID --ssh-key <your-key>
================================================================

EOF

confirm "Create Hetzner server from snapshot $SNAPSHOT_ID?" \
    || { echo "aborted; snapshot $SNAPSHOT_ID preserved for later use."; exit 0; }

# === Stage 3: create server + verify ===
mkdir -p "$HCLOUD_KEY_DIR"
chmod 700 "$HCLOUD_KEY_DIR"
if [[ ! -f "$HCLOUD_KEY" ]]; then
    ssh-keygen -t ed25519 -N '' -C "$SERVER_NAME-$$" -f "$HCLOUD_KEY" -q
fi
KEY_NAME="$SERVER_NAME"

stage "tearing down any leftover server / key from a prior run"
hcloud server delete "$SERVER_NAME" 2>/dev/null || true # y-script-lint:disable=or-true # idempotent cleanup: missing server is not an error
hcloud ssh-key delete "$KEY_NAME" 2>/dev/null || true # y-script-lint:disable=or-true # idempotent cleanup: missing key is not an error

stage "registering ssh public key as $KEY_NAME"
hcloud ssh-key create --name "$KEY_NAME" --public-key-from-file "$HCLOUD_KEY.pub" >/dev/null

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

# Wait for sshd, then probe the workload endpoints.
SSH_OPTS="-i $HCLOUD_KEY -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o ConnectTimeout=5"
echo "  waiting for ssh on $PUBLIC_IP:22"
for _ in $(seq 1 60); do
    # shellcheck disable=SC2086
    if ssh $SSH_OPTS root@"$PUBLIC_IP" 'true' 2>/dev/null; then
        break
    fi
    sleep 5
done

# Cold boot from snapshot: cloud-init -> k3s.service first start ->
# envoy gateway controller + data plane -> VersityGW StatefulSet
# rebinds its PV -> klipper-lb binds :80. Generous loop.
probe_remote() {
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
if probe_remote echo "http://$PUBLIC_IP/q/envoy/echo" \
    && probe_remote s3 "http://$PUBLIC_IP/s3/health"; then
    cat <<EOF

================================================================
Hetzner appliance is live and serving.

  Public IP:   $PUBLIC_IP
  Server:      $SERVER_NAME ($SERVER_TYPE in $SERVER_LOCATION)
  Snapshot:    $SNAPSHOT_ID

Endpoints (unauthenticated for now):
  echo:        http://$PUBLIC_IP/q/envoy/echo
  s3 health:   http://$PUBLIC_IP/s3/health

SSH (root, key-only):
  ssh -i $HCLOUD_KEY root@$PUBLIC_IP

kubectl from your laptop:
  ssh -i $HCLOUD_KEY root@$PUBLIC_IP cat /etc/rancher/k3s/k3s.yaml \\
    | sed "s|server: .*|server: https://$PUBLIC_IP:6443|" > k3s-$SERVER_NAME.yaml
  KUBECONFIG=k3s-$SERVER_NAME.yaml kubectl get nodes
  (k3s's apiserver isn't open to the internet by default; either
   add 6443 to the Hetzner firewall, or tunnel via ssh:
   ssh -L 6443:127.0.0.1:6443 -N root@$PUBLIC_IP &)

When you're done:
  hcloud server delete $SERVER_NAME
  hcloud ssh-key delete $KEY_NAME
  hcloud image delete $SNAPSHOT_ID    # optional; snapshot is reusable
================================================================
EOF
    exit 0
fi

echo >&2
echo "echo never answered. Server $SERVER_NAME left running for diagnosis:" >&2
# shellcheck disable=SC2086
ssh $SSH_OPTS root@"$PUBLIC_IP" 'systemctl is-active k3s; kubectl get pods -A 2>&1 | head -30' >&2 \
    || true # y-script-lint:disable=or-true # diagnostic best-effort
echo "  ssh: ssh -i $HCLOUD_KEY root@$PUBLIC_IP" >&2
echo "  destroy: hcloud server delete $SERVER_NAME" >&2
exit 1
