#!/usr/bin/env bash
# Bootstrap a service-account JSON key for automation to use
# against a GCP project (typically your y-cluster appliance QA
# project; see .env.example for the operator-local default).
#
# Run this on a machine where you're already gcloud-logged-in
# as a project Owner (or Editor with IAM admin). It will:
#   1. Verify your active gcloud account can act on the project.
#   2. Enable the Compute / Storage APIs the appliance-qemu-to-gcp
#      flow needs. (No Cloud Build: we convert qcow2 -> raw -> tar
#      locally and use `images create --source-uri=gs://...`, which
#      is a direct image create with no managed conversion job.)
#   3. Create (or reuse) a service account named
#      <SA_NAME>@<project>.iam.gserviceaccount.com.
#   4. Grant it roles/owner on the project. (QA project; broad
#      role keeps the bootstrap simple. Tighten later if QA gets
#      reused for non-QA assets.)
#   5. Generate a JSON key for the service account.
#   6. Print the JSON between unmistakable BEGIN/END markers so
#      you can copy-paste from your terminal scrollback to the
#      machine that needs the credentials. The key is also left
#      on disk at $KEY_FILE in case you'd rather scp it.
#
# After copying: on the other machine, save the JSON between
# the markers (NOT the markers themselves) to a file, chmod
# 600 it, and point GCP_KEY in $REPO_ROOT/.env at it. The
# appliance scripts read GCP_KEY from .env.

[ -z "$DEBUG" ] || set -x
set -eo pipefail

YHELP='gcp-bootstrap-credentials.sh - create + grant + key a service account for the y-cluster appliance flow, then print the JSON for cross-machine copy-paste

Usage: gcp-bootstrap-credentials.sh

Environment:
  GCP_PROJECT   GCP project (set in .env or shell env; required)
  SA_NAME       Service account local part (default: y-cluster-appliance)
  KEY_FILE      Where to write the JSON key on this machine
                (default: ./y-cluster-gcp-key.json)
  DEBUG         Set non-empty for bash trace

Dependencies:
  gcloud (logged in as a Project Owner or equivalent)
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
PROJECT_ID="$GCP_PROJECT"
SA_NAME="${SA_NAME:-y-cluster-appliance}"
SA_EMAIL="${SA_NAME}@${PROJECT_ID}.iam.gserviceaccount.com"
KEY_FILE="${KEY_FILE:-./y-cluster-gcp-key.json}"

command -v gcloud >/dev/null || { echo "gcloud not found on PATH" >&2; exit 1; }

stage() { printf '\n=== %s ===\n' "$*"; }

# 1. Verify caller is logged in and can see the project.
stage "verifying gcloud auth + project access ($PROJECT_ID)"
ACTIVE=$(gcloud auth list --filter=status:ACTIVE --format="value(account)" 2>/dev/null || true) # y-script-lint:disable=or-true # gcloud returns nonzero when no active account; we surface our own error below
if [[ -z "$ACTIVE" ]]; then
    echo "no active gcloud account; run: gcloud auth login" >&2
    exit 1
fi
echo "  active account: $ACTIVE"
gcloud projects describe "$PROJECT_ID" --format="value(projectId)" >/dev/null \
    || { echo "cannot read project $PROJECT_ID with $ACTIVE" >&2; exit 1; }

# 2. Enable required APIs. Idempotent: gcloud reports the
# already-enabled ones as no-ops.
stage "enabling APIs (compute, storage)"
gcloud services enable \
    compute.googleapis.com \
    storage.googleapis.com \
    --project="$PROJECT_ID"

# 3. Create the service account (idempotent: skip if it
# exists). gcloud doesn't ship a clean "create or skip", so
# we probe first.
stage "creating service account $SA_EMAIL (idempotent)"
if gcloud iam service-accounts describe "$SA_EMAIL" \
        --project="$PROJECT_ID" >/dev/null 2>&1; then
    echo "  already exists, reusing"
else
    gcloud iam service-accounts create "$SA_NAME" \
        --display-name="y-cluster appliance automation" \
        --description="Used by scripts/appliance-qemu-to-gcp.sh to upload custom images and provision VMs in $PROJECT_ID" \
        --project="$PROJECT_ID"
fi

# 4. Grant roles/owner on the project. QA project; broad role
# is intentional and matches the project's stated purpose. If
# this account ever gets reused for non-QA assets, tighten to
# the union of: compute.admin, storage.admin,
# iam.serviceAccountUser.
stage "granting roles/owner on $PROJECT_ID to $SA_EMAIL"
gcloud projects add-iam-policy-binding "$PROJECT_ID" \
    --member="serviceAccount:$SA_EMAIL" \
    --role="roles/owner" \
    --project="$PROJECT_ID" \
    --condition=None \
    >/dev/null

# 5. Mint a fresh JSON key. Each invocation creates a new key.
# GCP allows up to 10 keys per service account; if the operator
# is rotating, they can `gcloud iam service-accounts keys list`
# and delete the stale ones with `keys delete`.
stage "minting JSON key -> $KEY_FILE"
rm -f "$KEY_FILE"
gcloud iam service-accounts keys create "$KEY_FILE" \
    --iam-account="$SA_EMAIL" \
    --project="$PROJECT_ID"
chmod 600 "$KEY_FILE"

# 6. Print the JSON between markers for clipboard-friendly
# copy. Markers are exact strings the destination machine can
# grep for if they want to extract programmatically.
echo
echo "================================================================"
echo "JSON key for $SA_EMAIL"
echo "Project: $PROJECT_ID"
echo
echo "On the destination machine, save the lines BETWEEN the"
echo "----- BEGIN ... ----- and ----- END ... ----- markers"
echo "(NOT the markers themselves) to a file, then:"
echo "    chmod 600 <that-file>"
echo "    set GCP_KEY=<that-file> in \$REPO_ROOT/.env"
echo "================================================================"
echo
echo "----- BEGIN GCP SERVICE ACCOUNT KEY ($SA_EMAIL) -----"
cat "$KEY_FILE"
echo
echo "----- END GCP SERVICE ACCOUNT KEY ($SA_EMAIL) -----"
echo
echo "Local copy of the key (kept for scp / re-paste): $KEY_FILE"
echo "To revoke this key later:"
echo "  gcloud iam service-accounts keys list --iam-account=$SA_EMAIL --project=$PROJECT_ID"
echo "  gcloud iam service-accounts keys delete <KEY_ID> --iam-account=$SA_EMAIL --project=$PROJECT_ID"
