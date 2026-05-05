#!/usr/bin/env bash
# Build a y-cluster appliance interactively, ship it to GCP.
#
# Stages (this is the appliance contract -- the disk you verify
# locally is the disk that boots in GCP):
#
#   1. Provision local qemu cluster (k3s + Envoy Gateway).
#   2. PROMPT 1: drop into a hands-on window where the
#      operator applies their custom workloads via kubectl /
#      yconverge against context $NAME, tests them, and
#      confirms when satisfied.
#   3. y-cluster stop -> y-cluster prepare-export (virt-sysprep
#      identity reset + timesyncd flip + netplan generic match).
#   4. y-cluster export --format=gcp-tar -- packs the qcow2
#      into <name>.tar.gz containing a single disk.raw, the
#      shape Compute Engine custom images expect.
#   5. PROMPT 2: confirm before any GCP-side write happens.
#   6. Upload tarball to GCS (creates bucket on first run).
#   7. gcloud compute images create from the GCS object
#      (direct, no Cloud Build).
#   8. gcloud compute firewall-rules create (idempotent) for
#      tcp:80 + tcp:443 on tagged instances.
#   9. gcloud compute instances create from the new image,
#      tagged for the firewall rule.
#  10. Wait for ssh + probe HTTP. Print connection details.
#
# Aborting at PROMPT 1 leaves the local cluster running.
# Aborting at PROMPT 2 leaves the local bundle written but
# nothing in GCP.
#
# Every gcloud invocation passes --project=$GCP_PROJECT
# explicitly. Auth is the service-account JSON pointed at by
# $GOOGLE_APPLICATION_CREDENTIALS (created by
# scripts/gcp-bootstrap-credentials.sh).

[ -z "$DEBUG" ] || set -x
set -eo pipefail

YHELP='appliance-qemu-to-gcp.sh - local provision -> hands-on -> export -> ship to GCP

Usage:
  appliance-qemu-to-gcp.sh                            build + ship to GCP
  appliance-qemu-to-gcp.sh teardown                   delete VM + image + GCS object
  appliance-qemu-to-gcp.sh teardown --delete-data-disk
                                                      also delete the persistent
                                                      /data/yolean disk (DESTRUCTIVE)

Teardown reads GCP_PROJECT / GCP_ZONE / GCP_BUCKET / VM_NAME /
GCP_DATADIR_DISK / NAME from the same env vars as the build
flow. Custom images and GCS objects are deleted by NAME prefix
(so different NAMEs in the same project do not clobber each
other). The persistent data disk, the bucket itself, and the
firewall rule are preserved unless --delete-data-disk is set.
Local cluster cleanup (if KEEP_LOCAL was set) is separate:
y-cluster teardown -c \$CFG_DIR.

Environment:
  GCP_PROJECT       GCP project (set in .env or shell env; required)
  GCP_REGION        GCP region (default: europe-north2 -- Stockholm)
  GCP_ZONE          GCP zone (default: europe-north2-a)
  GCP_BUCKET        GCS bucket for image tarballs
                    (default: <project>-appliance-images)
  GCP_MACHINE_TYPE  Compute Engine machine type (default: e2-medium)
  GCP_IMAGE_FAMILY  Image family tag (default: y-cluster-appliance)
  GCP_DATADIR_DISK  Persistent disk for /data/yolean
                    (default: appliance-gcp-datadir; preserved on teardown)
  GCP_DATADIR_SIZE  Persistent disk size (default: 10GB; only used on create)
  GCP_KEY           Service account JSON (set in .env or shell env; required)
  NAME              Local cluster name (default: appliance-gcp-build).
                    Used as the prefix for the deliverable directory.
  KUBECTX           kubectl context name (default: local). Script
                    bails if a context with this name already
                    exists in your kubeconfig -- set KUBECTX to
                    something else, or delete the existing one.
  IMAGE_NAME        Custom image name in GCE (default: <NAME>-<UTC>)
  VM_NAME           Compute Engine VM name (default: $NAME)
  APP_HTTP_PORT     Override host port for guest 80 (y-cluster default: 80)
  APP_HTTPS_PORT    Override host port for guest 443 (y-cluster default: 443)
  APP_API_PORT      Override host port for guest 6443 (y-cluster default: 6443)
  APP_SSH_PORT      Override host port for guest 22 (y-cluster default: 2222)
  Y_CLUSTER         Path to dev binary (default: ./dist/y-cluster)
  CACHE_DIR         Where y-cluster keeps its qcow2 (default: ~/.cache/y-cluster-qemu)
  KEEP_LOCAL        Set to keep the local cluster after upload (default: tear down)
  KEEP_BUNDLE       Set to keep the local export bundle (default: keep -- bundle path printed)
  ASSUME_YES        Skip BOTH confirmations and proceed end-to-end.
                    Also suppresses the optional TLS-LB prompt; set
                    TLS_DOMAINS alongside to opt in unattended.
  TLS_DOMAINS       Comma-separated FQDNs for an optional regional
                    External HTTPS LoadBalancer with a self-signed
                    cert (e.g., appliance.example.com,admin.appliance.example.com).
                    Empty: skip the LB step. The HTTPRoutes must
                    already match these hostnames.

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
GCP_IMAGE_FAMILY="${GCP_IMAGE_FAMILY:-y-cluster-appliance}"
GCP_DATADIR_DISK="${GCP_DATADIR_DISK:-appliance-gcp-datadir}"
GCP_DATADIR_SIZE="${GCP_DATADIR_SIZE:-10GB}"

NAME="${NAME:-appliance-gcp-build}"
KUBECTX="${KUBECTX:-local}"
IMAGE_NAME="${IMAGE_NAME:-${NAME}-$(date -u +%Y%m%d-%H%M%S)}"
VM_NAME="${VM_NAME:-$NAME}"

Y_CLUSTER="${Y_CLUSTER:-$REPO_ROOT/dist/y-cluster}"
CACHE_DIR="${CACHE_DIR:-$HOME/.cache/y-cluster-qemu}"
CFG_DIR="${CFG_DIR:-$HOME/.cache/y-cluster-appliance-build/$NAME}"
# Top-level deliverable dir. Holds two per-format subdirs --
# `gcp-tar/` (uploaded to Compute Engine here) and `ova/`
# (handed to a customer for VirtualBox / VMware Import
# Appliance). Both subdirs are byte-equivalent disk states;
# the only differences are the on-the-wire format and the
# README boot instructions.
BUNDLE_DIR="${BUNDLE_DIR:-$REPO_ROOT/dist/appliance/$NAME-$(date -u +%Y%m%dT%H%M%SZ)}"

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

# do_teardown deletes GCP resources owned by this script's
# NAME prefix in the configured project + zone. Reads the
# same env vars as the build flow so a teardown after a
# customised build (e.g., NAME=customer-foo) cleans up
# exactly that customer's resources without touching other
# NAMEs that share the same project.
do_teardown() {
    local delete_data_disk=0
    while [[ $# -gt 0 ]]; do
        case "$1" in
            --delete-data-disk) delete_data_disk=1 ;;
            *) echo "unknown teardown flag: $1" >&2; exit 2 ;;
        esac
        shift
    done

    stage "inventory in $GCP_PROJECT / $GCP_ZONE"
    local vm images objects disk
    vm=$(gcloud compute instances describe "$VM_NAME" \
        --project="$GCP_PROJECT" --zone="$GCP_ZONE" \
        --format='value(name)' 2>/dev/null) \
        || true # y-script-lint:disable=or-true # missing VM is not an error
    images=$(gcloud compute images list \
        --project="$GCP_PROJECT" \
        --no-standard-images \
        --filter="name~^${NAME}-" \
        --format='value(name)' 2>/dev/null) \
        || true # y-script-lint:disable=or-true # empty list is not an error
    objects=$(gcloud storage ls "gs://$GCP_BUCKET/${NAME}-*.tar.gz" \
        --project="$GCP_PROJECT" 2>/dev/null) \
        || true # y-script-lint:disable=or-true # missing bucket / no objects is not an error
    disk=$(gcloud compute disks describe "$GCP_DATADIR_DISK" \
        --project="$GCP_PROJECT" --zone="$GCP_ZONE" \
        --format='value(name)' 2>/dev/null) \
        || true # y-script-lint:disable=or-true # missing disk is not an error

    echo
    echo "Will DELETE:"
    [[ -n "$vm" ]] && echo "  VM:                 $VM_NAME ($GCP_ZONE)"
    if [[ -n "$images" ]]; then
        echo "$images" | sed 's/^/  Image:              /'
    fi
    if [[ -n "$objects" ]]; then
        echo "$objects" | sed 's|^|  GCS object:         |'
    fi
    if [[ $delete_data_disk -eq 1 && -n "$disk" ]]; then
        echo "  Data disk:          $GCP_DATADIR_DISK (PERSISTENT DATA WILL BE LOST)"
    fi
    # If a TLS LB stack exists, do_tls_teardown will pick it up.
    # We don't enumerate every resource here -- the function logs
    # `deleting TLS LB stack ...` when it fires.
    if gcloud compute forwarding-rules describe "${NAME}-tls-fr" \
            --project="$GCP_PROJECT" --region="$GCP_REGION" \
            --format='value(name)' 2>/dev/null | grep -q .; then
        echo "  TLS LB stack:       ${NAME}-tls-* (forwarding rule + 8 dependents)"
    fi
    echo
    echo "Will PRESERVE:"
    if [[ $delete_data_disk -eq 0 && -n "$disk" ]]; then
        echo "  Data disk:          $GCP_DATADIR_DISK (--delete-data-disk to also remove)"
    fi
    echo "  GCS bucket:         gs://$GCP_BUCKET (objects matching $NAME-* deleted above)"
    echo "  Firewall rule:      y-cluster-appliance-public (tag-based, shared)"
    echo

    if [[ -z "$vm" && -z "$images" && -z "$objects" ]] \
            && { [[ $delete_data_disk -eq 0 ]] || [[ -z "$disk" ]]; }; then
        echo "Nothing to delete."
        exit 0
    fi

    confirm "Proceed with teardown?" \
        || { echo "aborted; nothing deleted."; exit 0; }

    if [[ -n "$vm" ]]; then
        stage "deleting VM $VM_NAME"
        gcloud compute instances delete "$VM_NAME" \
            --project="$GCP_PROJECT" --zone="$GCP_ZONE" --quiet >/dev/null
    fi
    if [[ -n "$images" ]]; then
        stage "deleting custom images ($(echo "$images" | wc -l))"
        # shellcheck disable=SC2086
        echo "$images" | xargs -r -I{} \
            gcloud compute images delete {} --project="$GCP_PROJECT" --quiet
    fi
    if [[ -n "$objects" ]]; then
        stage "deleting GCS objects ($(echo "$objects" | wc -l))"
        # shellcheck disable=SC2086
        echo "$objects" | xargs -r \
            gcloud storage rm --project="$GCP_PROJECT"
    fi
    if [[ $delete_data_disk -eq 1 && -n "$disk" ]]; then
        stage "deleting persistent data disk $GCP_DATADIR_DISK"
        gcloud compute disks delete "$GCP_DATADIR_DISK" \
            --project="$GCP_PROJECT" --zone="$GCP_ZONE" --quiet >/dev/null
    fi

    do_tls_teardown
    stage "teardown complete"
}

# do_tls_frontend stands up a regional External Application
# Load Balancer in front of $VM_NAME with a self-signed cert
# covering $1 (comma-separated FQDNs). Idempotent: each create
# is describe-then-create, so re-runs converge.
#
# Resources are named ${NAME}-tls-* so do_tls_teardown can clean
# them up alongside the rest of the appliance.
#
# Cost: regional EXTERNAL_MANAGED LB forwarding rule (~hourly)
# + reserved IP (only while reserved). Both billed by the
# forwarding-rule-hour and the IP-hour respectively, so teardown
# stops the meter immediately.
do_tls_frontend() {
    local domains_csv=$1
    local first_domain
    first_domain=$(echo "$domains_csv" | cut -d, -f1)
    local sans
    sans="DNS:$(echo "$domains_csv" | sed 's/,/,DNS:/g')"
    local cert_dir="$BUNDLE_DIR/tls"
    mkdir -p "$cert_dir"

    stage "generating self-signed cert for $domains_csv (90 days)"
    openssl req -x509 -newkey rsa:2048 -nodes \
        -keyout "$cert_dir/privkey.pem" -out "$cert_dir/fullchain.pem" \
        -days 90 -subj "/CN=$first_domain" \
        -addext "subjectAltName=$sans" 2>/dev/null
    chmod 600 "$cert_dir/privkey.pem"

    # Proxy-only subnet: required by regional EXTERNAL_MANAGED LBs,
    # one ACTIVE per region+VPC. Reuse if any exists; otherwise
    # create a per-build one we can clean up on teardown.
    stage "ensuring proxy-only subnet in $GCP_REGION"
    if gcloud compute networks subnets list \
            --project="$GCP_PROJECT" \
            --filter "region:$GCP_REGION AND purpose=REGIONAL_MANAGED_PROXY AND role=ACTIVE" \
            --format='value(name)' 2>/dev/null | grep -q .; then
        echo "  reusing existing proxy-only subnet"
    else
        gcloud compute networks subnets create "${NAME}-tls-proxy-subnet" \
            --project="$GCP_PROJECT" --region="$GCP_REGION" \
            --network=default --range=192.168.42.0/24 \
            --purpose=REGIONAL_MANAGED_PROXY --role=ACTIVE >/dev/null
    fi

    stage "reserving regional external IP ${NAME}-tls-ip"
    if ! gcloud compute addresses describe "${NAME}-tls-ip" \
            --project="$GCP_PROJECT" --region="$GCP_REGION" >/dev/null 2>&1; then
        gcloud compute addresses create "${NAME}-tls-ip" \
            --project="$GCP_PROJECT" --region="$GCP_REGION" \
            --network-tier=STANDARD >/dev/null
    fi
    local lb_ip
    lb_ip=$(gcloud compute addresses describe "${NAME}-tls-ip" \
        --project="$GCP_PROJECT" --region="$GCP_REGION" \
        --format='value(address)')

    stage "uploading SSL cert ${NAME}-tls-cert"
    if ! gcloud compute ssl-certificates describe "${NAME}-tls-cert" \
            --project="$GCP_PROJECT" --region="$GCP_REGION" >/dev/null 2>&1; then
        gcloud compute ssl-certificates create "${NAME}-tls-cert" \
            --project="$GCP_PROJECT" --region="$GCP_REGION" \
            --certificate="$cert_dir/fullchain.pem" \
            --private-key="$cert_dir/privkey.pem" >/dev/null
    fi

    stage "creating health check ${NAME}-tls-hc"
    if ! gcloud compute health-checks describe "${NAME}-tls-hc" \
            --project="$GCP_PROJECT" --region="$GCP_REGION" >/dev/null 2>&1; then
        gcloud compute health-checks create http "${NAME}-tls-hc" \
            --project="$GCP_PROJECT" --region="$GCP_REGION" \
            --port=80 --request-path=/q/envoy/echo \
            --check-interval=10s --timeout=5s >/dev/null
    fi

    stage "creating network endpoint group ${NAME}-tls-neg"
    if ! gcloud compute network-endpoint-groups describe "${NAME}-tls-neg" \
            --project="$GCP_PROJECT" --zone="$GCP_ZONE" >/dev/null 2>&1; then
        gcloud compute network-endpoint-groups create "${NAME}-tls-neg" \
            --project="$GCP_PROJECT" --zone="$GCP_ZONE" \
            --network-endpoint-type=GCE_VM_IP_PORT --default-port=80 >/dev/null
        gcloud compute network-endpoint-groups update "${NAME}-tls-neg" \
            --project="$GCP_PROJECT" --zone="$GCP_ZONE" \
            --add-endpoint="instance=$VM_NAME,port=80" >/dev/null
    fi

    stage "creating backend service ${NAME}-tls-backend"
    if ! gcloud compute backend-services describe "${NAME}-tls-backend" \
            --project="$GCP_PROJECT" --region="$GCP_REGION" >/dev/null 2>&1; then
        gcloud compute backend-services create "${NAME}-tls-backend" \
            --project="$GCP_PROJECT" --region="$GCP_REGION" \
            --load-balancing-scheme=EXTERNAL_MANAGED --protocol=HTTP \
            --health-checks="${NAME}-tls-hc" \
            --health-checks-region="$GCP_REGION" >/dev/null
        gcloud compute backend-services add-backend "${NAME}-tls-backend" \
            --project="$GCP_PROJECT" --region="$GCP_REGION" \
            --network-endpoint-group="${NAME}-tls-neg" \
            --network-endpoint-group-zone="$GCP_ZONE" \
            --balancing-mode=RATE --max-rate-per-endpoint=100 >/dev/null
    fi

    stage "creating URL map ${NAME}-tls-urlmap"
    if ! gcloud compute url-maps describe "${NAME}-tls-urlmap" \
            --project="$GCP_PROJECT" --region="$GCP_REGION" >/dev/null 2>&1; then
        gcloud compute url-maps create "${NAME}-tls-urlmap" \
            --project="$GCP_PROJECT" --region="$GCP_REGION" \
            --default-service="projects/$GCP_PROJECT/regions/$GCP_REGION/backendServices/${NAME}-tls-backend" >/dev/null
    fi

    stage "creating target HTTPS proxy ${NAME}-tls-proxy"
    if ! gcloud compute target-https-proxies describe "${NAME}-tls-proxy" \
            --project="$GCP_PROJECT" --region="$GCP_REGION" >/dev/null 2>&1; then
        gcloud compute target-https-proxies create "${NAME}-tls-proxy" \
            --project="$GCP_PROJECT" --region="$GCP_REGION" \
            --url-map="${NAME}-tls-urlmap" \
            --ssl-certificates="${NAME}-tls-cert" >/dev/null
    fi

    stage "creating forwarding rule ${NAME}-tls-fr (:443)"
    if ! gcloud compute forwarding-rules describe "${NAME}-tls-fr" \
            --project="$GCP_PROJECT" --region="$GCP_REGION" >/dev/null 2>&1; then
        gcloud compute forwarding-rules create "${NAME}-tls-fr" \
            --project="$GCP_PROJECT" --region="$GCP_REGION" \
            --load-balancing-scheme=EXTERNAL_MANAGED --network-tier=STANDARD \
            --network=default --address="${NAME}-tls-ip" \
            --target-https-proxy="${NAME}-tls-proxy" \
            --target-https-proxy-region="$GCP_REGION" --ports=443 >/dev/null
    fi

    # === HTTP -> HTTPS redirect chain ===
    # GCP regional EXTERNAL_MANAGED URL maps can do a default redirect
    # but `gcloud compute url-maps create` has no flag for it -- we
    # have to import a YAML body. A URL map can have either
    # `defaultService` (forward) or `defaultUrlRedirect` (redirect),
    # not both, hence the second URL map + second target proxy + second
    # forwarding rule sharing the same reserved IP.
    stage "creating redirect URL map ${NAME}-tls-redirect (HTTP -> HTTPS)"
    if ! gcloud compute url-maps describe "${NAME}-tls-redirect" \
            --project="$GCP_PROJECT" --region="$GCP_REGION" >/dev/null 2>&1; then
        gcloud compute url-maps import "${NAME}-tls-redirect" \
                --project="$GCP_PROJECT" --region="$GCP_REGION" \
                --source=- --quiet >/dev/null <<YAML
name: ${NAME}-tls-redirect
defaultUrlRedirect:
  httpsRedirect: true
  redirectResponseCode: MOVED_PERMANENTLY_DEFAULT
  stripQuery: false
YAML
    fi

    stage "creating target HTTP proxy ${NAME}-tls-http-proxy"
    if ! gcloud compute target-http-proxies describe "${NAME}-tls-http-proxy" \
            --project="$GCP_PROJECT" --region="$GCP_REGION" >/dev/null 2>&1; then
        gcloud compute target-http-proxies create "${NAME}-tls-http-proxy" \
            --project="$GCP_PROJECT" --region="$GCP_REGION" \
            --url-map="${NAME}-tls-redirect" \
            --url-map-region="$GCP_REGION" >/dev/null
    fi

    stage "creating forwarding rule ${NAME}-tls-fr-http (:80 -> redirect)"
    if ! gcloud compute forwarding-rules describe "${NAME}-tls-fr-http" \
            --project="$GCP_PROJECT" --region="$GCP_REGION" >/dev/null 2>&1; then
        gcloud compute forwarding-rules create "${NAME}-tls-fr-http" \
            --project="$GCP_PROJECT" --region="$GCP_REGION" \
            --load-balancing-scheme=EXTERNAL_MANAGED --network-tier=STANDARD \
            --network=default --address="${NAME}-tls-ip" \
            --target-http-proxy="${NAME}-tls-http-proxy" \
            --target-http-proxy-region="$GCP_REGION" --ports=80 >/dev/null
    fi

    cat <<EOF

================================================================
External HTTPS LoadBalancer ready.

  IP:        $lb_ip
  Hostnames: ${domains_csv//,/ }
  Cert:      SELF-SIGNED (browser will warn; curl needs -k)
  HTTP:      :80 -> 301 redirect to :443 (so plain http:// works
             as long as the client follows redirects, e.g. curl -L)

To test from another machine, append this single line to /etc/hosts:

  $lb_ip  ${domains_csv//,/ }

For a real cert (cert-manager / Let's Encrypt), upload a fresh PEM
+ key as ${NAME}-tls-cert-vN, then point the proxy at it via
\`gcloud compute target-https-proxies update ${NAME}-tls-proxy
--ssl-certificates=${NAME}-tls-cert-vN --region=$GCP_REGION\`.
================================================================

EOF
}

# do_tls_teardown deletes everything do_tls_frontend created.
# Idempotent: missing resources are not errors. Order matters --
# the forwarding rule has to go before the proxy/url-map/backend
# chain, and the IP after.
do_tls_teardown() {
    local fr fr_http proxy http_proxy urlmap urlmap_redirect backend neg hc cert ip subnet
    fr=$(gcloud compute forwarding-rules describe "${NAME}-tls-fr" \
        --project="$GCP_PROJECT" --region="$GCP_REGION" \
        --format='value(name)' 2>/dev/null) || true # y-script-lint:disable=or-true # missing fr is not an error
    fr_http=$(gcloud compute forwarding-rules describe "${NAME}-tls-fr-http" \
        --project="$GCP_PROJECT" --region="$GCP_REGION" \
        --format='value(name)' 2>/dev/null) || true # y-script-lint:disable=or-true # missing :80 redirect fr is not an error
    proxy=$(gcloud compute target-https-proxies describe "${NAME}-tls-proxy" \
        --project="$GCP_PROJECT" --region="$GCP_REGION" \
        --format='value(name)' 2>/dev/null) || true # y-script-lint:disable=or-true # missing proxy is not an error
    http_proxy=$(gcloud compute target-http-proxies describe "${NAME}-tls-http-proxy" \
        --project="$GCP_PROJECT" --region="$GCP_REGION" \
        --format='value(name)' 2>/dev/null) || true # y-script-lint:disable=or-true # missing :80 redirect proxy is not an error
    urlmap=$(gcloud compute url-maps describe "${NAME}-tls-urlmap" \
        --project="$GCP_PROJECT" --region="$GCP_REGION" \
        --format='value(name)' 2>/dev/null) || true # y-script-lint:disable=or-true # missing url-map is not an error
    urlmap_redirect=$(gcloud compute url-maps describe "${NAME}-tls-redirect" \
        --project="$GCP_PROJECT" --region="$GCP_REGION" \
        --format='value(name)' 2>/dev/null) || true # y-script-lint:disable=or-true # missing redirect url-map is not an error
    backend=$(gcloud compute backend-services describe "${NAME}-tls-backend" \
        --project="$GCP_PROJECT" --region="$GCP_REGION" \
        --format='value(name)' 2>/dev/null) || true # y-script-lint:disable=or-true # missing backend is not an error
    neg=$(gcloud compute network-endpoint-groups describe "${NAME}-tls-neg" \
        --project="$GCP_PROJECT" --zone="$GCP_ZONE" \
        --format='value(name)' 2>/dev/null) || true # y-script-lint:disable=or-true # missing neg is not an error
    hc=$(gcloud compute health-checks describe "${NAME}-tls-hc" \
        --project="$GCP_PROJECT" --region="$GCP_REGION" \
        --format='value(name)' 2>/dev/null) || true # y-script-lint:disable=or-true # missing hc is not an error
    cert=$(gcloud compute ssl-certificates describe "${NAME}-tls-cert" \
        --project="$GCP_PROJECT" --region="$GCP_REGION" \
        --format='value(name)' 2>/dev/null) || true # y-script-lint:disable=or-true # missing cert is not an error
    ip=$(gcloud compute addresses describe "${NAME}-tls-ip" \
        --project="$GCP_PROJECT" --region="$GCP_REGION" \
        --format='value(name)' 2>/dev/null) || true # y-script-lint:disable=or-true # missing ip is not an error
    subnet=$(gcloud compute networks subnets describe "${NAME}-tls-proxy-subnet" \
        --project="$GCP_PROJECT" --region="$GCP_REGION" \
        --format='value(name)' 2>/dev/null) || true # y-script-lint:disable=or-true # missing subnet is not an error

    if [[ -z "$fr$fr_http$proxy$http_proxy$urlmap$urlmap_redirect$backend$neg$hc$cert$ip$subnet" ]]; then
        return
    fi

    stage "deleting TLS LB stack (${NAME}-tls-*)"
    # Forwarding rules first (they reference proxies) -- both :443
    # and the :80 redirect.
    [[ -n "$fr" ]] && gcloud compute forwarding-rules delete "${NAME}-tls-fr" \
        --project="$GCP_PROJECT" --region="$GCP_REGION" --quiet >/dev/null
    [[ -n "$fr_http" ]] && gcloud compute forwarding-rules delete "${NAME}-tls-fr-http" \
        --project="$GCP_PROJECT" --region="$GCP_REGION" --quiet >/dev/null
    # Then proxies (they reference URL maps).
    [[ -n "$proxy" ]] && gcloud compute target-https-proxies delete "${NAME}-tls-proxy" \
        --project="$GCP_PROJECT" --region="$GCP_REGION" --quiet >/dev/null
    [[ -n "$http_proxy" ]] && gcloud compute target-http-proxies delete "${NAME}-tls-http-proxy" \
        --project="$GCP_PROJECT" --region="$GCP_REGION" --quiet >/dev/null
    # Then URL maps (the :443 backend-pointing one + the :80 redirect one).
    [[ -n "$urlmap" ]] && gcloud compute url-maps delete "${NAME}-tls-urlmap" \
        --project="$GCP_PROJECT" --region="$GCP_REGION" --quiet >/dev/null
    [[ -n "$urlmap_redirect" ]] && gcloud compute url-maps delete "${NAME}-tls-redirect" \
        --project="$GCP_PROJECT" --region="$GCP_REGION" --quiet >/dev/null
    [[ -n "$backend" ]] && gcloud compute backend-services delete "${NAME}-tls-backend" \
        --project="$GCP_PROJECT" --region="$GCP_REGION" --quiet >/dev/null
    [[ -n "$neg" ]] && gcloud compute network-endpoint-groups delete "${NAME}-tls-neg" \
        --project="$GCP_PROJECT" --zone="$GCP_ZONE" --quiet >/dev/null
    [[ -n "$hc" ]] && gcloud compute health-checks delete "${NAME}-tls-hc" \
        --project="$GCP_PROJECT" --region="$GCP_REGION" --quiet >/dev/null
    [[ -n "$cert" ]] && gcloud compute ssl-certificates delete "${NAME}-tls-cert" \
        --project="$GCP_PROJECT" --region="$GCP_REGION" --quiet >/dev/null
    [[ -n "$ip" ]] && gcloud compute addresses delete "${NAME}-tls-ip" \
        --project="$GCP_PROJECT" --region="$GCP_REGION" --quiet >/dev/null
    # Subnet last: only delete the per-build one (do_tls_frontend
    # never creates a subnet that already exists, so anything named
    # ${NAME}-tls-proxy-subnet was definitely ours).
    [[ -n "$subnet" ]] && gcloud compute networks subnets delete "${NAME}-tls-proxy-subnet" \
        --project="$GCP_PROJECT" --region="$GCP_REGION" --quiet >/dev/null
}

# Minimal pre-checks shared by build and teardown: gcloud
# binary + GCP key + activation. The build flow does
# additional tool checks below the dispatch.
command -v gcloud >/dev/null \
    || { echo "missing required tool: gcloud" >&2; exit 1; }

if [[ ! -f "$GCP_KEY" ]]; then
    echo "missing GCP key: $GCP_KEY" >&2
    echo "create it with: scripts/gcp-bootstrap-credentials.sh on a machine with gcloud Owner access" >&2
    exit 1
fi
export GOOGLE_APPLICATION_CREDENTIALS="$GCP_KEY"

# Acknowledge parallel composite uploads up front. The setting
# both turns on multi-stream uploads (which is what we want for
# 1.5+ GiB tarballs) AND silences the WARNING stanza gcloud
# would otherwise emit on every `storage cp`. Env-var form so
# we don't mutate the operator's gcloud config.
export CLOUDSDK_STORAGE_PARALLEL_COMPOSITE_UPLOAD_ENABLED=True

stage "activating GCP service account ($GCP_KEY)"
gcloud auth activate-service-account --key-file="$GCP_KEY" --project="$GCP_PROJECT" >/dev/null

# Subcommand dispatch. Teardown only needs gcloud + GCP_KEY,
# both verified above; doesn't need go / qemu-img / etc. so
# the build-flow tool check below stays out of its path.
if [[ "${1:-}" = "teardown" ]]; then
    shift
    do_teardown "$@"
    exit 0
fi

# Build-flow tool check (additional to gcloud above).
for tool in go qemu-system-x86_64 qemu-img kubectl ssh ssh-keygen curl virt-sysprep; do
    command -v "$tool" >/dev/null \
        || { echo "missing required tool: $tool" >&2; exit 1; }
done

# virt-sysprep needs to read /boot/vmlinuz-* (libguestfs supermin).
if ! [ -r /boot/vmlinuz-"$(uname -r)" ]; then
    cat >&2 <<EOF
/boot/vmlinuz-$(uname -r) is not readable; virt-sysprep will fail.
Fix one of:
  sudo chmod +r /boot/vmlinuz-*
  sudo dpkg-statoverride --update --add root root 0644 /boot/vmlinuz-$(uname -r)
EOF
    exit 1
fi

# === Stage 1: build dev binary + provision local qemu ===
stage "building dev binary -> $Y_CLUSTER"
mkdir -p "$(dirname "$Y_CLUSTER")"
( cd "$REPO_ROOT" && go build -o "$Y_CLUSTER" ./cmd/y-cluster )

mkdir -p "$CFG_DIR"
# YAML emission omits any port the operator didn't override, letting
# y-cluster's Go binary apply its own defaults (sshPort=2222,
# portForwards={6443:6443, 80:80, 443:443}). Set APP_*_PORT to take
# different values; otherwise the script doesn't restate y-cluster's
# defaults in two places.
{
    echo "provider: qemu"
    echo "name: $NAME"
    echo "context: $KUBECTX"
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

stage "tearing down any leftover $NAME cluster"
"$Y_CLUSTER" teardown -c "$CFG_DIR" || true # y-script-lint:disable=or-true # idempotent re-entry: missing cluster is not an error

# Bail-out guard: our own teardown above would have removed
# the kubectl context THIS script registered on a previous
# run. A surviving "$KUBECTX" entry means something else owns
# it (e.g., a parallel y-cluster cluster, or the operator's
# personal "local" dev cluster). We refuse to clobber.
if kubectl config get-contexts -o name 2>/dev/null | grep -Fxq "$KUBECTX"; then
    echo "kubectl context '$KUBECTX' already exists and is not owned by this script." >&2
    echo "  Either remove it:    kubectl config delete-context $KUBECTX" >&2
    echo "  Or pick a new name:  KUBECTX=appliance-qa $0" >&2
    exit 1
fi

stage "provisioning $NAME (k3s + Envoy Gateway)"
"$Y_CLUSTER" provision -c "$CFG_DIR"

# Echo is what creates the Gateway listener (not just the
# Envoy Gateway controller -- the actual Gateway resource that
# binds :80). Without it, any HTTPRoute the operator applies
# in the hands-on window has nothing to attach to and curl
# returns "connection refused" both locally and on the eventual
# GCP VM. Auto-install so the Gateway listener is up by default;
# operators can still delete + replace echo with their own
# workload (the Gateway listener stays, the routing changes).
stage "installing echo workload (Gateway listener + baseline route)"
"$Y_CLUSTER" echo render \
    | kubectl --context="$KUBECTX" apply --server-side --field-manager=appliance-build -f -
kubectl --context="$KUBECTX" -n y-cluster wait \
    --for=condition=Available deployment/echo --timeout=180s

# === Stage 2: hands-on prompt ===
SSH_KEY="$CACHE_DIR/$NAME-ssh"
cat <<EOF

================================================================
Local cluster $NAME is up. Echo is already serving on :80.

  Echo route (baseline, already up):
    curl -sf http://127.0.0.1:${APP_HTTP_PORT:-80}/q/envoy/echo

  Kubernetes API:   https://127.0.0.1:${APP_API_PORT:-6443}
  kubectl context:  $KUBECTX

Optional: apply more workloads before the disk gets sealed.
The Gateway listener echo brought up is shared, so HTTPRoutes
in any namespace can attach to it.

  # S3 backend example (VersityGW StatefulSet on local-path PV):
  $Y_CLUSTER yconverge --context=$KUBECTX -k $REPO_ROOT/testdata/appliance-stateful/base
  curl -sf http://127.0.0.1:${APP_HTTP_PORT:-80}/s3/health

  # Re-apply echo (e.g., after editing the manifest):
  $Y_CLUSTER echo render | kubectl --context=$KUBECTX apply -f -

  # Your own workloads:
  kubectl --context=$KUBECTX apply -f my-workload.yaml
  $Y_CLUSTER yconverge --context=$KUBECTX -k path/to/kustomize-base

SSH into the local VM (passwordless sudo as ystack):
  ssh -i $SSH_KEY -p ${APP_SSH_PORT:-2222} \\
      -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \\
      ystack@127.0.0.1

Once you confirm, the local cluster will be stopped, the disk
will be sealed (prepare-export), packed as a GCE-custom-image
tarball, uploaded to GCS, and a VM will be created from it in
$GCP_PROJECT/$GCP_ZONE.

(KEEP_LOCAL=1 to keep the local cluster running after upload.)
================================================================

EOF

confirm "Proceed to export + GCP deploy?" \
    || { echo "aborted; local cluster left running. Teardown with: $Y_CLUSTER teardown -c $CFG_DIR"; exit 0; }

# === Stage 3: stop + prepare-export + export gcp-tar ===
stage "stopping cluster ($NAME)"
"$Y_CLUSTER" stop --context="$KUBECTX"

stage "prepare-export ($NAME)"
"$Y_CLUSTER" prepare-export --context="$KUBECTX"

# Dual export to per-format subdirs of the deliverable.
# Both reads come from the same prepare-export'd qcow2 so
# the disk state is byte-identical; the only differences are
# the on-the-wire packaging (tar.gz with disk.raw vs OVF +
# streamOptimized VMDK in tar) and the per-format README.
# The SSH keypair `<name>-ssh{,.pub}` lands in both subdirs;
# the pair is identical (one keypair was generated at
# provision time, both export passes copy from the same
# source under $CACHE_DIR).
mkdir -p "$BUNDLE_DIR"

stage "exporting Compute Engine image format -> $BUNDLE_DIR/gcp-tar"
"$Y_CLUSTER" export --context="$KUBECTX" --format=gcp-tar "$BUNDLE_DIR/gcp-tar"

stage "exporting OVA (VirtualBox / VMware Import Appliance) -> $BUNDLE_DIR/ova"
"$Y_CLUSTER" export --context="$KUBECTX" --format=ova "$BUNDLE_DIR/ova"

ls -lh "$BUNDLE_DIR"/*/
TARBALL="$BUNDLE_DIR/gcp-tar/$NAME.tar.gz"

# === Stage 4: confirm before any GCP write ===
cat <<EOF

================================================================
Local export ready: $TARBALL
  size: $(stat -c '%s' "$TARBALL" | numfmt --to=iec-i --suffix=B 2>/dev/null || stat -c '%s' "$TARBALL")

Next: upload to gs://$GCP_BUCKET/$IMAGE_NAME.tar.gz, create a
GCE custom image, ensure firewall opens tcp:80 + tcp:443 on
tagged VMs, create $VM_NAME ($GCP_MACHINE_TYPE in $GCP_ZONE)
from the image. Aborting now leaves the bundle on local disk
unchanged.
================================================================

EOF

confirm "Upload $TARBALL to GCS and create VM in $GCP_PROJECT?" \
    || { echo "aborted; bundle preserved at $BUNDLE_DIR."; exit 0; }

# === Stage 5: GCS bucket (idempotent) ===
stage "ensuring GCS bucket gs://$GCP_BUCKET (location $GCP_REGION)"
if ! gcloud storage buckets describe "gs://$GCP_BUCKET" --project="$GCP_PROJECT" >/dev/null 2>&1; then
    gcloud storage buckets create "gs://$GCP_BUCKET" \
        --project="$GCP_PROJECT" \
        --location="$GCP_REGION" \
        --uniform-bucket-level-access
else
    echo "  bucket exists"
fi

# === Stage 6: upload tarball ===
stage "uploading $TARBALL -> gs://$GCP_BUCKET/$IMAGE_NAME.tar.gz"
gcloud storage cp "$TARBALL" "gs://$GCP_BUCKET/$IMAGE_NAME.tar.gz" --project="$GCP_PROJECT"

# === Stage 7: create custom image ===
stage "creating GCE custom image $IMAGE_NAME (family $GCP_IMAGE_FAMILY)"
gcloud compute images create "$IMAGE_NAME" \
    --project="$GCP_PROJECT" \
    --source-uri="gs://$GCP_BUCKET/$IMAGE_NAME.tar.gz" \
    --family="$GCP_IMAGE_FAMILY" \
    --architecture=X86_64 \
    >/dev/null

# === Stage 8: firewall rule (idempotent) ===
FIREWALL_RULE="y-cluster-appliance-public"
stage "ensuring firewall rule $FIREWALL_RULE (tcp:80,443 -> y-cluster-appliance tag)"
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
else
    echo "  rule exists"
fi

# === Stage 8.5: ensure persistent data disk ===
# Persistent disk attached to the VM and mounted at /data/yolean
# (the bundled local-path-provisioner's default storage root).
# Survives instance redeploys: tear down the VM, redeploy with a
# fresh image, the same /data/yolean comes back. Disk auto-delete
# is OFF when attaching an existing disk via --disk=name=, so
# `instances delete` won't wipe it.
stage "ensuring persistent data disk $GCP_DATADIR_DISK (size only used on create: $GCP_DATADIR_SIZE)"
if gcloud compute disks describe "$GCP_DATADIR_DISK" \
        --project="$GCP_PROJECT" --zone="$GCP_ZONE" >/dev/null 2>&1; then
    echo "  disk exists -- reusing (data preserved from previous deploy)"
else
    gcloud compute disks create "$GCP_DATADIR_DISK" \
        --project="$GCP_PROJECT" \
        --zone="$GCP_ZONE" \
        --size="$GCP_DATADIR_SIZE" \
        --type=pd-balanced \
        >/dev/null
    echo "  disk created (fresh; will be ext4-formatted on first mount)"
fi

# === Stage 9: create VM (delete first if exists for idempotency) ===
stage "creating $VM_NAME ($GCP_MACHINE_TYPE in $GCP_ZONE) from image $IMAGE_NAME"
if gcloud compute instances describe "$VM_NAME" --project="$GCP_PROJECT" --zone="$GCP_ZONE" >/dev/null 2>&1; then
    echo "  $VM_NAME exists, deleting first"
    gcloud compute instances delete "$VM_NAME" \
        --project="$GCP_PROJECT" --zone="$GCP_ZONE" --quiet >/dev/null
fi
# device-name=datadir is what GCE writes after the
# `scsi-0Google_PersistentDisk_` prefix in /dev/disk/by-id/
# inside the VM; the SSH-side mount block uses that stable path
# regardless of /dev/sd* enumeration order.
gcloud compute instances create "$VM_NAME" \
    --project="$GCP_PROJECT" \
    --zone="$GCP_ZONE" \
    --machine-type="$GCP_MACHINE_TYPE" \
    --image="$IMAGE_NAME" \
    --image-project="$GCP_PROJECT" \
    --boot-disk-size=40GB \
    --disk="name=$GCP_DATADIR_DISK,device-name=datadir,mode=rw,boot=no" \
    --tags=y-cluster-appliance \
    >/dev/null

PUBLIC_IP=$(gcloud compute instances describe "$VM_NAME" \
    --project="$GCP_PROJECT" \
    --zone="$GCP_ZONE" \
    --format='get(networkInterfaces[0].accessConfigs[0].natIP)')
echo "  public ip: $PUBLIC_IP"

# === Stage 10: wait for ssh + probe ===
# SSH_KEY (from CACHE_DIR) was used by the local cluster but is
# wiped by `y-cluster teardown` at the end of this flow. The
# bundle-dir copy is what the operator can reach the GCP VM
# with afterwards. Switch to the bundle path BEFORE teardown
# runs so subsequent prints reference the path that'll exist.
SSH_KEY="$BUNDLE_DIR/gcp-tar/$NAME-ssh"
SSH_OPTS="-i $SSH_KEY -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o ConnectTimeout=5"
echo "  waiting for ssh on $PUBLIC_IP:22 (cloud-init can take 30-90s on first boot)"
ssh_up=0
for i in $(seq 1 60); do
    # shellcheck disable=SC2086
    if ssh $SSH_OPTS -p 22 ystack@"$PUBLIC_IP" 'true' 2>/dev/null; then
        echo "  ssh up after $i attempt(s)"
        ssh_up=1
        break
    fi
    echo "  ssh attempt $i/60: not yet"
    sleep 5
done
if [[ $ssh_up -eq 0 ]]; then
    echo "ssh on $PUBLIC_IP never came up; VM left running for diagnosis" >&2
    echo "  delete with: gcloud compute instances delete $VM_NAME --project=$GCP_PROJECT --zone=$GCP_ZONE" >&2
    exit 1
fi

# === Stage 10.5: mount the persistent disk at /data/yolean ===
# The appliance disk doesn't carry GCE guest-tools and our
# prepare_inguest pinned cloud-init to NoCloud only, so we can't
# mount via cloud-init mounts/ or via google-startup-scripts.
# We SSH in and do it directly:
#   - format the disk if it has no filesystem (fresh disk)
#   - persist the mount via fstab UUID for subsequent reboots
#   - mount now
#   - restart k3s so it re-discovers /data/yolean (k3s started
#     before the mount existed; existing PVs would have mapped
#     to empty paths on the root FS until restart)
stage "mounting $GCP_DATADIR_DISK at /data/yolean and restarting k3s"
# shellcheck disable=SC2087
ssh $SSH_OPTS ystack@"$PUBLIC_IP" 'sudo bash -s' <<'REMOTE'
set -eu
# /dev/disk/by-id/google-<device-name> requires google-guest-agent,
# which only ships in Google's own GCE images. Our appliance is
# built from the upstream Ubuntu cloud image, so we get the
# kernel-provided SCSI udev path instead:
#   /dev/disk/by-id/scsi-0Google_PersistentDisk_<device-name>
# `<device-name>` is what we passed to `gcloud --disk=device-name=datadir`,
# so the path is fully deterministic. We try both shapes -- SCSI
# first (matches the current appliance) and the guest-agent shape
# as a fallback for a future build that does install the agent.
MOUNT=/data/yolean
DEVICE=""
for cand in /dev/disk/by-id/scsi-0Google_PersistentDisk_datadir /dev/disk/by-id/google-datadir; do
    for _ in $(seq 1 30); do
        if [ -b "$cand" ]; then
            DEVICE="$cand"
            break 2
        fi
        sleep 1
    done
done
[ -n "$DEVICE" ] || { echo "datadir disk never appeared at any expected /dev/disk/by-id/ path" >&2; exit 1; }
echo "datadir: $DEVICE"

# Format with the label that matches the appliance's pre-baked
# fstab entry (LABEL=y-cluster-data /data/yolean ext4 ...).
# Using a different label, or adding a UUID-based fstab line,
# would either skip the pre-bake mount or duplicate it -- we
# want the LABEL line to be the one that fires at boot.
if ! blkid "$DEVICE" >/dev/null 2>&1; then
    mkfs.ext4 -F -L y-cluster-data "$DEVICE"
fi
# Idempotent label enforcement: re-running this script against a
# data disk that was formatted by a PREVIOUS version of the script
# (with a different label, e.g. `data-yolean`) would skip mkfs
# above (blkid finds an existing FS) and leave the wrong label in
# place. The appliance's pre-baked /etc/fstab matches by LABEL, so
# a wrong label means the boot-time mount silently no-ops and the
# seed gate fails. e2label is a no-op when the label is already
# correct, so applying it unconditionally is cheap insurance.
e2label "$DEVICE" y-cluster-data

install -d -m 0755 "$MOUNT"
if ! mountpoint -q "$MOUNT"; then
    mount "$MOUNT"
fi

# At first boot the seed unit ran before this disk was formatted
# and mounted, so it failed the mount-required gate and k3s.service
# stayed down on its Requires=. Now that /data/yolean is a real
# mountpoint, restart the seed unit so it extracts the seed onto
# the customer's volume, then k3s.
systemctl reset-failed y-cluster-data-seed.service k3s.service
systemctl restart y-cluster-data-seed.service
systemctl restart k3s.service
REMOTE

probe() {
    local what=$1 url=$2 attempts=${3:-60}
    for i in $(seq 1 "$attempts"); do
        if curl -fsS --max-time 8 -o /dev/null -w "  $what HTTP %{http_code}\n" "$url"; then
            return 0
        fi
        echo "  $what attempt $i/$attempts: no answer yet"
        sleep 10
    done
    return 1
}

# probe_route checks one Gateway-bound FQDN through PUBLIC_IP via
# `--resolve <fqdn>:80:<ip>`. Reachability == any HTTP response,
# not 200: an HTTPRoute that legitimately answers 302 / 401 / 404
# is still proof the firewall + klipper-lb + envoy-gateway chain
# is working end-to-end. Only `000` (timeout, refused) counts as
# unreachable.
probe_route() {
    local fqdn=$1 attempts=${2:-30}
    local code
    for i in $(seq 1 "$attempts"); do
        code=$(curl -sS -o /dev/null -m 5 \
            --resolve "$fqdn:80:$PUBLIC_IP" \
            -w '%{http_code}' "http://$fqdn/" 2>/dev/null \
            || echo 000)
        if [[ "$code" != "000" ]]; then
            printf '  %-40s HTTP %s (attempt %d)\n' "$fqdn" "$code" "$i"
            return 0
        fi
        echo "  $fqdn attempt $i/$attempts: no answer yet"
        sleep 10
    done
    return 1
}

stage "enumerating Gateway routes on the appliance"
# Walk HTTPRoutes + GRPCRoutes for spec.hostnames. Each unique FQDN
# becomes a probe target. We use SSH + `sudo k3s kubectl` rather
# than extracting the kubeconfig because the apiserver isn't yet
# externally exposed at this point in the script (the kubeconfig
# extract recipe at the bottom of the success summary is for the
# operator after teardown of the local cluster).
ROUTE_HOSTS=$(ssh $SSH_OPTS ystack@"$PUBLIC_IP" \
    'sudo k3s kubectl get httproute,grpcroute -A -o jsonpath="{range .items[*]}{range .spec.hostnames[*]}{@}{\"\n\"}{end}{end}"' \
    2>/dev/null | sort -u)

if [[ -z "$ROUTE_HOSTS" ]]; then
    echo "  no Gateway-bound HTTPRoutes/GRPCRoutes; falling back to echo probe"
    probe echo "http://$PUBLIC_IP/q/envoy/echo" 30 || \
        echo "  (echo also unreachable; cluster still booting?)"
else
    stage "probing each Gateway route via $PUBLIC_IP"
    fail_count=0
    fail_list=""
    while IFS= read -r fqdn; do
        if ! probe_route "$fqdn" 30; then
            fail_count=$((fail_count + 1))
            fail_list="$fail_list $fqdn"
        fi
    done <<<"$ROUTE_HOSTS"
    if [[ $fail_count -gt 0 ]]; then
        echo "  WARNING: $fail_count route(s) unreachable after 5min:$fail_list"
        echo "  Possible causes:"
        echo "    - firewall y-cluster-appliance-public source-ranges narrowed (check"
        echo "      \`gcloud compute firewall-rules describe y-cluster-appliance-public\`)"
        echo "    - HTTPRoute attached but backend Service not Ready"
        echo "    - workload still rolling out (re-run \`probe_route <fqdn>\` later)"
    fi
fi

# === Stage 11: optional external HTTPS LoadBalancer ===
# Operator-driven add-on: if TLS_DOMAINS isn't set in the env,
# prompt for it (skip on empty input). With ASSUME_YES + TLS_DOMAINS
# set, runs without prompting. With ASSUME_YES alone, skip silently
# -- ASSUME_YES is for unattended e2e and we don't want to surprise
# the operator with a billing meter they didn't ask for.
if [[ -z "${TLS_DOMAINS:-}" && -z "${ASSUME_YES:-}" ]]; then
    echo
    echo "================================================================"
    echo "Optional: external HTTPS LoadBalancer (regional, EXTERNAL_MANAGED)"
    echo
    echo "Sets up a regional GCP External Application Load Balancer in"
    echo "front of $VM_NAME with a SELF-SIGNED cert covering the FQDNs"
    echo "you specify. Useful for testing the LB+routing chain without"
    echo "DNS or a real CA. Browsers will warn on the cert; tools need"
    echo "--insecure / -k. Cost: ~hourly forwarding-rule + reserved IP."
    echo
    echo "HTTPRoutes on the cluster need spec.hostnames covering the"
    echo "same FQDNs (the LB forwards Host: unchanged). Patch them"
    echo "yourself before answering yes."
    echo "================================================================"
    read -r -p "FQDNs (comma-separated, empty to skip): " TLS_DOMAINS
fi
if [[ -n "${TLS_DOMAINS:-}" ]]; then
    do_tls_frontend "$TLS_DOMAINS"
fi

if [[ -z "${KEEP_LOCAL:-}" ]]; then
    stage "tearing down local cluster (set KEEP_LOCAL=1 to keep it)"
    "$Y_CLUSTER" teardown -c "$CFG_DIR" 2>/dev/null || true # y-script-lint:disable=or-true # cleanup best-effort
fi

cat <<EOF

================================================================
Appliance live in GCP.

  Project:       $GCP_PROJECT
  Zone:          $GCP_ZONE
  VM:            $VM_NAME ($GCP_MACHINE_TYPE)
  Public IP:     $PUBLIC_IP
  Image:         $IMAGE_NAME (family $GCP_IMAGE_FAMILY)
  Data disk:     $GCP_DATADIR_DISK -> /data/yolean (persistent)
  Deliverable:   $BUNDLE_DIR
                 ├── gcp-tar/  (uploaded to GCE, used for the
                 │              live $VM_NAME above)
                 └── ova/      (hand to a customer for VirtualBox /
                                VMware -- same disk state)

Connect:
  ssh -i $SSH_KEY ystack@$PUBLIC_IP
  curl http://$PUBLIC_IP/<your-route>

kubectl from your laptop (apiserver not externally exposed):
  ssh -L 6443:127.0.0.1:6443 -N -i $SSH_KEY ystack@$PUBLIC_IP &
  ssh -i $SSH_KEY ystack@$PUBLIC_IP sudo cat /etc/rancher/k3s/k3s.yaml \\
    > k3s-$VM_NAME.yaml
  KUBECONFIG=k3s-$VM_NAME.yaml kubectl get nodes

Teardown when done:
  gcloud compute instances delete $VM_NAME --project=$GCP_PROJECT --zone=$GCP_ZONE
  gcloud compute images delete $IMAGE_NAME --project=$GCP_PROJECT
  gcloud storage rm gs://$GCP_BUCKET/$IMAGE_NAME.tar.gz --project=$GCP_PROJECT

Persistent data disk is PRESERVED on teardown so PVC data
survives across redeploys. Re-running this script reuses the
same /data/yolean. Delete it manually when you're truly done:
  gcloud compute disks delete $GCP_DATADIR_DISK --project=$GCP_PROJECT --zone=$GCP_ZONE
================================================================
EOF
