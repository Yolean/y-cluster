#!/usr/bin/env bash
# e2e: complete qemu-to-GCP appliance workflow, non-interactive.
#
# This is the canonical SRE example for the appliance contract:
# the disk we verify locally with qemu IS the disk that boots in
# Google Compute Engine. No re-build on the cloud side; the GCS
# tarball is exactly what `y-cluster export --format=gcp-tar`
# produced from the local provision.
#
# The workflow this script documents -- in order -- is what an
# SRE follows by hand when they want to ship a customer
# appliance to GCP:
#
#   1. Bootstrap a GCP service account in the QA project (one
#      time per project; output is a JSON key the rest of the
#      flow consumes via GOOGLE_APPLICATION_CREDENTIALS).
#         scripts/gcp-bootstrap-credentials.sh
#
#   2. Provision a y-cluster appliance locally on qemu. This
#      gives the same k3s + Envoy Gateway + bundled local-path
#      stack the customer will run.
#         y-cluster provision -c <config>
#
#   3. Install the customer's workload(s). The e2e here uses
#      the y-cluster echo workload + the appliance-stateful
#      VersityGW StatefulSet as stand-ins; in real customer
#      flows this is whatever kubectl apply / yconverge / helm
#      the customer specifies. The Hetzner Object Storage
#      tutorial uses VersityGW; the principle is the same.
#         y-cluster echo render | kubectl apply -f -
#         y-cluster yconverge -k testdata/appliance-stateful/base
#
#   4. Smoketest from the host. Anything that's reachable on
#      :80 of the local qemu's port-forward is reachable on
#      :80 of the eventual GCE VM.
#         curl http://127.0.0.1:80/q/envoy/echo
#
#   5. Stop the cluster cleanly so the qcow2 is quiesced. The
#      graceful-stop logic flushes containerd snapshot state.
#         y-cluster stop --context=$NAME
#
#   6. prepare-export: virt-customize-driven identity reset
#      (machine-id retained, ssh host keys retained, cloud-init
#      cleaned, netplan generic-NIC match installed,
#      systemd-timesyncd enabled). This is the step that makes
#      the disk portable.
#         y-cluster prepare-export --context=$NAME
#
#   7. Export to GCE custom-image format. Produces
#      <bundle>/<name>.tar.gz containing exactly disk.raw.
#         y-cluster export --context=$NAME --format=gcp-tar <bundle>
#
#   8. Upload to GCS. Bucket created on first run with
#      uniform-access mode.
#         gcloud storage cp <bundle>/<name>.tar.gz \
#             gs://<project>-appliance-images/<image-name>.tar.gz
#
#   9. Create custom image from the GCS object. Direct create
#      (no managed conversion job).
#         gcloud compute images create <image-name> \
#             --source-uri=gs://<project>-appliance-images/<image-name>.tar.gz
#
#  10. Ensure firewall opens public ports. Idempotent.
#         gcloud compute firewall-rules create y-cluster-appliance-public ...
#
#  11. Create VM from the image, tagged for the firewall rule.
#         gcloud compute instances create <vm-name> \
#             --image=<image-name> --tags=y-cluster-appliance ...
#
#  12. Wait for ssh + probe HTTP. The disk we just built is the
#      disk now booting; if smoketest passes here, it's the same
#      smoketest that passed locally.
#
#  13. Teardown: delete the VM, the image, the GCS object, the
#      local cluster. The e2e is the thing that proves the
#      contract; we don't leave artefacts behind.
#
# Re-run safety: every step is idempotent. Running this twice
# in a row produces the same result; partial-failure re-runs
# pick up where the previous left off (fresh teardown of any
# leftover server / image / cluster on entry).
#
# This script is the proof. The interactive variant is
# scripts/appliance-qemu-to-gcp.sh -- same flow but with
# operator prompts at the export and GCP-write boundaries.

[ -z "$DEBUG" ] || set -x
set -eo pipefail

YHELP='e2e-appliance-qemu-to-gcp.sh - canonical SRE workflow: provision -> install -> verify -> prepare-export -> export gcp-tar -> upload -> image -> instance -> probe -> teardown

Usage: e2e-appliance-qemu-to-gcp.sh

Environment:
  GCP_PROJECT       GCP project (set in .env or shell env; required)
  GCP_REGION        GCP region (default: europe-north2)
  GCP_ZONE          GCP zone (default: europe-north2-a)
  GCP_BUCKET        GCS bucket (default: <project>-appliance-images)
  GCP_MACHINE_TYPE  Machine type (default: e2-medium)
  GCP_KEY           Service account JSON (set in .env or shell env; required)
  NAME              Cluster + VM name (default: appliance-gcp-e2e)
  KEEP              Set to skip teardown for diagnosis (default: tear down on success)
  DEBUG             Set non-empty for bash trace

Dependencies:
  go, qemu-system-x86_64, qemu-img, kubectl, ssh, ssh-keygen, curl,
  virt-sysprep, gcloud
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

: "${GCP_PROJECT:?set GCP_PROJECT in .env or shell env}"
: "${GCP_KEY:?set GCP_KEY in .env or shell env}"

GCP_REGION="${GCP_REGION:-europe-north2}"
GCP_ZONE="${GCP_ZONE:-europe-north2-a}"
GCP_BUCKET="${GCP_BUCKET:-${GCP_PROJECT}-appliance-images}"
GCP_MACHINE_TYPE="${GCP_MACHINE_TYPE:-e2-medium}"

NAME="${NAME:-appliance-gcp-e2e}"

Y_CLUSTER="${Y_CLUSTER:-$REPO_ROOT/dist/y-cluster}"
CACHE_DIR="${CACHE_DIR:-$HOME/.cache/y-cluster-qemu}"
CFG_DIR="$HOME/.cache/y-cluster-appliance-build/$NAME"
BUNDLE_DIR=$(mktemp -d -p "$REPO_ROOT/dist" "appliance-gcp-e2e.XXXXXX" 2>/dev/null \
    || mktemp -d -p /tmp "appliance-gcp-e2e.XXXXXX")

IMAGE_NAME="$NAME-$(date -u +%Y%m%d-%H%M%S)"
VM_NAME="$NAME"

stage() { printf '\n=== %s ===\n' "$*"; }

teardown() {
    set +e
    if [[ -n "${KEEP:-}" ]]; then
        echo
        echo "KEEP set; preserving artefacts for diagnosis:"
        echo "  local cluster: $Y_CLUSTER teardown -c $CFG_DIR"
        echo "  GCE VM:        gcloud compute instances delete $VM_NAME --project=$GCP_PROJECT --zone=$GCP_ZONE"
        echo "  GCE image:     gcloud compute images delete $IMAGE_NAME --project=$GCP_PROJECT"
        echo "  GCS object:    gcloud storage rm gs://$GCP_BUCKET/$IMAGE_NAME.tar.gz --project=$GCP_PROJECT"
        echo "  bundle:        $BUNDLE_DIR"
        return
    fi
    stage "teardown"
    gcloud compute instances delete "$VM_NAME" \
        --project="$GCP_PROJECT" --zone="$GCP_ZONE" --quiet 2>/dev/null # y-script-lint:disable=or-true # idempotent cleanup: missing VM is not an error
    gcloud compute images delete "$IMAGE_NAME" \
        --project="$GCP_PROJECT" --quiet 2>/dev/null # y-script-lint:disable=or-true # idempotent cleanup: missing image is not an error
    gcloud storage rm "gs://$GCP_BUCKET/$IMAGE_NAME.tar.gz" \
        --project="$GCP_PROJECT" 2>/dev/null # y-script-lint:disable=or-true # idempotent cleanup: missing object is not an error
    "$Y_CLUSTER" teardown -c "$CFG_DIR" 2>/dev/null # y-script-lint:disable=or-true # idempotent cleanup: missing cluster is not an error
    rm -rf "$BUNDLE_DIR"
}
trap teardown EXIT

for tool in go qemu-system-x86_64 qemu-img kubectl ssh ssh-keygen curl virt-sysprep gcloud; do
    command -v "$tool" >/dev/null \
        || { echo "missing required tool: $tool" >&2; exit 1; }
done

if [[ ! -f "$GCP_KEY" ]]; then
    echo "missing GCP key: $GCP_KEY" >&2
    echo "create it with: scripts/gcp-bootstrap-credentials.sh" >&2
    exit 1
fi
export GOOGLE_APPLICATION_CREDENTIALS="$GCP_KEY"

# Acknowledge parallel composite uploads up front (silences
# the WARNING stanza gcloud would otherwise emit on every
# `storage cp` for files >150 MiB).
export CLOUDSDK_STORAGE_PARALLEL_COMPOSITE_UPLOAD_ENABLED=True

if ! [ -r /boot/vmlinuz-"$(uname -r)" ]; then
    cat >&2 <<EOF
/boot/vmlinuz-$(uname -r) is not readable; virt-sysprep will fail.
  sudo chmod +r /boot/vmlinuz-*
EOF
    exit 1
fi

# === 0. Auth ===
stage "activating GCP service account"
gcloud auth activate-service-account --key-file="$GCP_KEY" --project="$GCP_PROJECT" >/dev/null

# === 1. Build dev binary ===
stage "building dev binary -> $Y_CLUSTER"
mkdir -p "$(dirname "$Y_CLUSTER")"
( cd "$REPO_ROOT" && go build -o "$Y_CLUSTER" ./cmd/y-cluster )

# === 2. Provision local qemu ===
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
    if [ -n "${APP_HTTP_PORT:-}" ] || [ -n "${APP_API_PORT:-}" ]; then
        echo "portForwards:"
        [ -n "${APP_API_PORT:-}" ] && printf '  - host: "%s"\n    guest: "6443"\n' "$APP_API_PORT"
        [ -n "${APP_HTTP_PORT:-}" ] && printf '  - host: "%s"\n    guest: "80"\n' "$APP_HTTP_PORT"
    fi
} > "$CFG_DIR/y-cluster-provision.yaml"

stage "tearing down any leftover $NAME cluster"
"$Y_CLUSTER" teardown -c "$CFG_DIR" || true # y-script-lint:disable=or-true # idempotent re-entry: missing cluster is not an error

stage "provisioning $NAME (k3s + Envoy Gateway)"
"$Y_CLUSTER" provision -c "$CFG_DIR"

# === 3. Install canonical workloads ===
stage "installing echo workload"
"$Y_CLUSTER" echo render \
    | kubectl --context="$NAME" apply --server-side --field-manager=customer-install -f -
kubectl --context="$NAME" -n y-cluster wait \
    --for=condition=Available deployment/echo --timeout=180s

stage "installing VersityGW StatefulSet via yconverge"
"$Y_CLUSTER" yconverge --context="$NAME" \
    -k "$REPO_ROOT/testdata/appliance-stateful/base"

# === 4. Local smoketest ===
stage "local smoketest: echo + s3"
probe_local() {
    local what=$1 url=$2 attempts=${3:-30}
    for i in $(seq 1 "$attempts"); do
        if curl -fsS --max-time 8 -o /dev/null -w "  $what HTTP %{http_code}\n" "$url"; then
            return 0
        fi
        echo "  $what attempt $i/$attempts"
        sleep 5
    done
    return 1
}
probe_local echo "http://127.0.0.1:${APP_HTTP_PORT:-80}/q/envoy/echo"
probe_local s3   "http://127.0.0.1:${APP_HTTP_PORT:-80}/s3/health"

# === 5. Stop ===
stage "stopping cluster"
"$Y_CLUSTER" stop --context="$NAME"

# === 6. prepare-export ===
stage "prepare-export"
"$Y_CLUSTER" prepare-export --context="$NAME"

# === 7. Export to GCE-tar ===
stage "exporting GCE-custom-image tarball -> $BUNDLE_DIR"
# y-cluster export refuses non-empty bundle dirs; the mktemp -d
# above created an empty dir we own, so a fresh re-run is fine.
# On retry-after-failure paths the dir might have content from
# the previous attempt, so we wipe + let export recreate.
rm -rf "$BUNDLE_DIR"
"$Y_CLUSTER" export --context="$NAME" --format=gcp-tar "$BUNDLE_DIR"
TARBALL="$BUNDLE_DIR/$NAME.tar.gz"
echo "  size: $(stat -c '%s' "$TARBALL" | numfmt --to=iec-i --suffix=B 2>/dev/null || stat -c '%s' "$TARBALL")"

# === 8. Upload to GCS ===
stage "ensuring bucket gs://$GCP_BUCKET ($GCP_REGION)"
if ! gcloud storage buckets describe "gs://$GCP_BUCKET" --project="$GCP_PROJECT" >/dev/null 2>&1; then
    gcloud storage buckets create "gs://$GCP_BUCKET" \
        --project="$GCP_PROJECT" \
        --location="$GCP_REGION" \
        --uniform-bucket-level-access
fi

stage "uploading -> gs://$GCP_BUCKET/$IMAGE_NAME.tar.gz"
gcloud storage cp "$TARBALL" "gs://$GCP_BUCKET/$IMAGE_NAME.tar.gz" --project="$GCP_PROJECT"

# === 9. Create custom image ===
stage "creating GCE custom image $IMAGE_NAME"
gcloud compute images create "$IMAGE_NAME" \
    --project="$GCP_PROJECT" \
    --source-uri="gs://$GCP_BUCKET/$IMAGE_NAME.tar.gz" \
    --family=y-cluster-appliance \
    --architecture=X86_64 \
    >/dev/null

# === 10. Firewall (idempotent) ===
FIREWALL_RULE="y-cluster-appliance-public"
stage "ensuring firewall rule $FIREWALL_RULE"
if ! gcloud compute firewall-rules describe "$FIREWALL_RULE" --project="$GCP_PROJECT" >/dev/null 2>&1; then
    gcloud compute firewall-rules create "$FIREWALL_RULE" \
        --project="$GCP_PROJECT" \
        --direction=INGRESS \
        --network=default \
        --action=ALLOW \
        --rules=tcp:80,tcp:443 \
        --target-tags=y-cluster-appliance \
        --source-ranges=0.0.0.0/0 \
        >/dev/null
fi

# === 11. Create VM ===
stage "creating $VM_NAME ($GCP_MACHINE_TYPE in $GCP_ZONE)"
if gcloud compute instances describe "$VM_NAME" --project="$GCP_PROJECT" --zone="$GCP_ZONE" >/dev/null 2>&1; then
    gcloud compute instances delete "$VM_NAME" \
        --project="$GCP_PROJECT" --zone="$GCP_ZONE" --quiet >/dev/null
fi
gcloud compute instances create "$VM_NAME" \
    --project="$GCP_PROJECT" \
    --zone="$GCP_ZONE" \
    --machine-type="$GCP_MACHINE_TYPE" \
    --image="$IMAGE_NAME" \
    --image-project="$GCP_PROJECT" \
    --boot-disk-size=20GB \
    --tags=y-cluster-appliance \
    >/dev/null
PUBLIC_IP=$(gcloud compute instances describe "$VM_NAME" \
    --project="$GCP_PROJECT" \
    --zone="$GCP_ZONE" \
    --format='get(networkInterfaces[0].accessConfigs[0].natIP)')
echo "  public ip: $PUBLIC_IP"

# === 12. Wait for ssh + probe HTTP ===
SSH_KEY="$CACHE_DIR/$NAME-ssh"
SSH_OPTS="-i $SSH_KEY -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o ConnectTimeout=5"

echo "  waiting for ssh on $PUBLIC_IP:22 (cloud-init can take 30-90s on first boot)"
ssh_up=0
for i in $(seq 1 60); do
    # shellcheck disable=SC2086
    if ssh $SSH_OPTS ystack@"$PUBLIC_IP" 'true' 2>/dev/null; then
        echo "  ssh up after $i attempt(s)"
        ssh_up=1
        break
    fi
    echo "  ssh attempt $i/60: not yet"
    sleep 5
done
[[ $ssh_up -eq 1 ]] || { echo "ssh never came up on $PUBLIC_IP" >&2; exit 1; }

probe_remote() {
    local what=$1 url=$2 attempts=${3:-60}
    for i in $(seq 1 "$attempts"); do
        if curl -fsS --max-time 8 -o /dev/null -w "  $what HTTP %{http_code}\n" "$url"; then
            return 0
        fi
        echo "  $what attempt $i/$attempts"
        sleep 10
    done
    return 1
}

stage "probing http://$PUBLIC_IP -- echo + s3 (same routes the local cluster served)"
if probe_remote echo "http://$PUBLIC_IP/q/envoy/echo" \
    && probe_remote s3 "http://$PUBLIC_IP/s3/health"; then
    echo
    echo "================================================================"
    echo "PASS: appliance-qemu-to-gcp e2e."
    echo
    echo "Local-built disk booted in GCP and served the same routes that"
    echo "the local qemu served. The appliance contract holds."
    echo
    echo "  Public IP:  $PUBLIC_IP"
    echo "  SSH:        ssh -i $SSH_KEY ystack@$PUBLIC_IP"
    echo "  echo:       http://$PUBLIC_IP/q/envoy/echo"
    echo "  s3 health:  http://$PUBLIC_IP/s3/health"
    echo "================================================================"
    exit 0
fi

echo >&2
echo "remote probes never returned; instance left for diagnosis (KEEP=1 to skip cleanup):" >&2
# shellcheck disable=SC2086
ssh $SSH_OPTS ystack@"$PUBLIC_IP" \
    'sudo systemctl is-active k3s; sudo k3s kubectl get pods -A 2>&1 | head -30' >&2 \
    || true # y-script-lint:disable=or-true # diagnostic best-effort
exit 1
