#!/usr/bin/env bash
# Idempotently ensure a Hetzner Object Storage bucket exists,
# configured to allow public GET on individual objects but NOT
# bucket listing, then upload a single file and print its public
# URL.
#
# Use case: the operator runs scripts/appliance-build-virtualbox.sh
# to produce a VMDK bundle, then this script to publish the
# bundle (or a tarball of it) at a URL their test host can curl
# while staying anonymous.
#
# Hetzner Object Storage is S3-compatible; we shell out to the
# AWS CLI pointed at https://<region>.your-objectstorage.com.
# If `aws` is not installed locally we run the official image
# via docker, which is universally available on dev machines.
#
# Credentials live in $H_S3_ENV_FILE (set in .env or shell env;
# typically the same file that holds HCLOUD_TOKEN). The file
# should set:
#   H_S3_ACCESS_KEY=<from Hetzner Cloud Console -> Object
#                    Storage -> Credentials>
#   H_S3_SECRET_KEY=...
#   H_S3_REGION=fsn1   # or hel1 / nbg1
#   H_S3_BUCKET=...    # default bucket (script arg overrides)
#
# These are SEPARATE from HCLOUD_TOKEN: Object Storage is
# managed under the same project but the API uses dedicated
# S3 access/secret keys, not the Cloud API token. We co-locate
# them in the same env file because they share a project, not
# because they share an auth scheme.

[ -z "$DEBUG" ] || set -x
set -eo pipefail

YHELP='appliance-publish-hetzner.sh - upload a file to a Hetzner Object Storage bucket with public-read on objects (no listing)

Usage: appliance-publish-hetzner.sh <file> [object-key]

Positional:
  file         Local path to upload
  object-key   Key to write under in the bucket (default: basename of file)

Environment:
  H_S3_ENV_FILE   Path to env file with H_S3_* vars (set in .env or shell env; required)
  H_S3_BUCKET     Bucket name; overrides the env file. Required if not in env file.
  H_S3_REGION     Region; overrides the env file (fsn1, hel1, or nbg1).
  AWS_CLI         How to invoke aws. Default: local `aws` if on PATH,
                  else `docker run --rm -i public.ecr.aws/aws-cli/aws-cli`.

Examples:
  # publish a fresh appliance bundle
  ./scripts/appliance-publish-hetzner.sh \
      dist/appliance-virtualbox/appliance-virtualbox-*/appliance-virtualbox.vmdk

  # publish under a custom key
  ./scripts/appliance-publish-hetzner.sh appliance.tar.gz releases/2026-05-01/appliance.tar.gz

Dependencies:
  curl, and one of: locally-installed `aws` (preferred) OR `docker`
  (used to invoke public.ecr.aws/aws-cli/aws-cli when aws is missing)
'

case "${1:-}" in
  help) echo "$YHELP"; exit 0 ;;
  --help) echo "$YHELP"; exit 0 ;;
  -h) echo "$YHELP"; exit 0 ;;
  "") echo "$YHELP" >&2; exit 2 ;;
esac

INPUT="$1"
KEY_OVERRIDE="${2:-}"

stage() { printf '\n=== %s ===\n' "$*"; }

if [[ ! -e "$INPUT" ]]; then
    echo "path not found: $INPUT" >&2
    exit 1
fi

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
if [[ -f "$REPO_ROOT/.env" ]]; then
    set -o allexport; . "$REPO_ROOT/.env"; set +o allexport
fi

: "${H_S3_ENV_FILE:?set H_S3_ENV_FILE in .env or shell env}"
ENV_FILE="$H_S3_ENV_FILE"
if [[ -f "$ENV_FILE" ]]; then
    # shellcheck disable=SC1090
    set -a; . "$ENV_FILE"; set +a
else
    echo "credentials file not found: $ENV_FILE" >&2
    cat >&2 <<EOF
Add the following lines (alongside HCLOUD_TOKEN if you already
have y-cluster-hetzner.env in place):
  H_S3_ACCESS_KEY=...
  H_S3_SECRET_KEY=...
  H_S3_REGION=fsn1
  H_S3_BUCKET=your-bucket-name

Get the access/secret pair from the Hetzner Cloud Console:
  https://console.hetzner.com/projects/<id>/object-storage/credentials
EOF
    exit 1
fi

: "${H_S3_ACCESS_KEY:?H_S3_ACCESS_KEY not set in $ENV_FILE}"
: "${H_S3_SECRET_KEY:?H_S3_SECRET_KEY not set in $ENV_FILE}"
: "${H_S3_REGION:?H_S3_REGION not set in $ENV_FILE (fsn1, hel1, or nbg1)}"
: "${H_S3_BUCKET:?H_S3_BUCKET not set; pass via env or env file}"

BUCKET="$H_S3_BUCKET"
REGION="$H_S3_REGION"
ENDPOINT="https://${REGION}.your-objectstorage.com"

# === Decide what to upload ===
# Two modes:
#   bundle - INPUT is a directory that looks like a y-cluster
#            bundle (or a file inside one, identified by a
#            sibling README.md). We tar `-C parent dirname` so
#            the tarball extracts to a sibling directory in the
#            customer's CWD: `tar xzf <name>.tgz` produces
#            `./<name>/{README.md, *.vmdk, *-ssh, *-ssh.pub}`.
#   single - INPUT is a regular file with no bundle context.
#            Upload as-is. Key defaults to its basename.
# Bundle mode is preferred whenever a README.md sits next to
# the disk file, so the operator can pass either the directory
# or the .vmdk and get the same bundle-tarball result.
SOURCE_FILE=""
KEY=""
BUNDLE_DIR=""

if [[ -d "$INPUT" ]]; then
    BUNDLE_DIR=$(realpath "$INPUT")
elif [[ -f "$INPUT" && -f "$(dirname "$INPUT")/README.md" ]]; then
    BUNDLE_DIR=$(realpath "$(dirname "$INPUT")")
fi

if [[ -n "$BUNDLE_DIR" ]]; then
    bundle_name=$(basename "$BUNDLE_DIR")
    bundle_parent=$(dirname "$BUNDLE_DIR")
    # Write the tarball next to the bundle dir, NOT under /tmp.
    # /tmp is tmpfs on most distros (~16 GB) and a 1.5 GiB
    # appliance tarball easily exhausts it; bundle_parent is on
    # the operator's chosen output volume where space matches
    # the bundle size.
    TGZ="$bundle_parent/.${bundle_name}.$$.tgz"
    trap 'rm -f "$TGZ"' EXIT
    stage "packing bundle $BUNDLE_DIR -> $TGZ"
    tar -czf "$TGZ" -C "$bundle_parent" "$bundle_name"
    SOURCE_FILE="$TGZ"
    KEY="${KEY_OVERRIDE:-${bundle_name}.tgz}"
else
    SOURCE_FILE="$INPUT"
    KEY="${KEY_OVERRIDE:-$(basename "$INPUT")}"
fi

PUBLIC_URL="https://${BUCKET}.${REGION}.your-objectstorage.com/${KEY}"

# === Pick an AWS CLI invocation ===
# Prefer a local `aws` to avoid pulling a 200MB image on every
# run; fall back to docker so a fresh dev box doesn't have to
# install awscli first.
if [[ -n "${AWS_CLI:-}" ]]; then
    : # operator override; trust it verbatim
elif command -v aws >/dev/null; then
    AWS_CLI="aws"
elif command -v docker >/dev/null; then
    # Mount /tmp because mktemp puts the policy + tarball there;
    # mount $HOME so absolute paths under $HOME (typical y-cluster
    # cache locations) resolve inside the container; -w $PWD +
    # -v $PWD:$PWD lets relative paths the operator typed work.
    AWS_CLI="docker run --rm -i \
        -e AWS_ACCESS_KEY_ID -e AWS_SECRET_ACCESS_KEY -e AWS_DEFAULT_REGION \
        -v $HOME:$HOME -v $PWD:$PWD -v /tmp:/tmp -w $PWD \
        public.ecr.aws/aws-cli/aws-cli"
else
    echo "neither 'aws' nor 'docker' found; install one or set AWS_CLI" >&2
    exit 1
fi

export AWS_ACCESS_KEY_ID="$H_S3_ACCESS_KEY"
export AWS_SECRET_ACCESS_KEY="$H_S3_SECRET_KEY"
export AWS_DEFAULT_REGION="$REGION"

aws_s3api() {
    # shellcheck disable=SC2086
    $AWS_CLI s3api --endpoint-url "$ENDPOINT" "$@"
}
aws_s3() {
    # shellcheck disable=SC2086
    $AWS_CLI s3 --endpoint-url "$ENDPOINT" "$@"
}

# === Ensure bucket exists ===
# head-bucket exits 0 if the bucket exists and we have access,
# nonzero with stderr "Not Found" / "Forbidden" otherwise. We
# only auto-create on Not Found; Forbidden means a name clash
# in another tenant and the operator should pick a different
# bucket name.
stage "checking bucket s3://$BUCKET (endpoint: $ENDPOINT)"
head_err=$(mktemp)
trap 'rm -f "$head_err"' EXIT
if aws_s3api head-bucket --bucket "$BUCKET" 2>"$head_err"; then
    echo "  bucket exists"
else
    if grep -qiE '404|Not Found|NoSuchBucket' "$head_err"; then
        stage "creating bucket s3://$BUCKET"
        # Hetzner rejects LocationConstraint=us-east-1 (the
        # AWS-CLI default for create-bucket without
        # --create-bucket-configuration). Hetzner-region values
        # work as the LocationConstraint.
        aws_s3api create-bucket \
            --bucket "$BUCKET" \
            --create-bucket-configuration "LocationConstraint=$REGION"
    else
        echo "head-bucket failed and not a 404:" >&2
        cat "$head_err" >&2
        exit 1
    fi
fi

# === Apply public-read-on-objects, no-listing policy ===
# This is the "anonymous can curl any individual object whose
# key they already know, but cannot enumerate the bucket"
# pattern. We allow only s3:GetObject on the
# arn:aws:s3:::BUCKET/* resource; ListBucket on the bucket
# itself is omitted, so anonymous LIST is denied.
stage "applying public-read-objects policy"
policy_file=$(mktemp)
trap 'rm -f "$head_err" "$policy_file"' EXIT
cat > "$policy_file" <<EOF
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Sid": "PublicReadObjects",
      "Effect": "Allow",
      "Principal": "*",
      "Action": "s3:GetObject",
      "Resource": "arn:aws:s3:::${BUCKET}/*"
    }
  ]
}
EOF

# put-bucket-policy is idempotent: re-applying the same JSON is
# a no-op as far as the visible behaviour is concerned. We
# always re-apply so a manually-tweaked policy snaps back to
# the documented shape.
aws_s3api put-bucket-policy \
    --bucket "$BUCKET" \
    --policy "file://$policy_file"

# Belt-and-braces: explicitly disable the per-object public-
# access-block guards so the policy above takes effect even on
# accounts where Hetzner default-blocks public ACL/policy.
# Hetzner mirrors the AWS PublicAccessBlockConfiguration
# subset; setting all four to false is the documented "I
# really mean public" stance.
aws_s3api put-public-access-block \
    --bucket "$BUCKET" \
    --public-access-block-configuration \
        BlockPublicAcls=false,IgnorePublicAcls=false,BlockPublicPolicy=false,RestrictPublicBuckets=false \
    2>/dev/null || true # y-script-lint:disable=or-true # not all S3-compat backends implement put-public-access-block; policy alone is sufficient on Hetzner

# === Upload ===
stage "uploading $SOURCE_FILE -> s3://$BUCKET/$KEY"
size=$(stat -c '%s' "$SOURCE_FILE")
echo "  size: $size bytes ($(numfmt --to=iec-i --suffix=B "$size" 2>/dev/null || echo "$size B"))"

# `aws s3 cp` handles multipart for >8MB by default and prints
# a progress bar to stderr; preferred over `s3api put-object`
# for arbitrary-sized files (qcow2 / vmdk are easily >5GB).
aws_s3 cp "$SOURCE_FILE" "s3://$BUCKET/$KEY"

# === Verify the object is anonymously reachable ===
# Use a fresh curl with no creds to confirm the policy actually
# took effect; surfaces config drift (e.g. another script
# overwriting the bucket policy) at publish time, not at
# customer-download time.
stage "verifying anonymous GET"
http_code=$(curl -sI -o /dev/null -w '%{http_code}' "$PUBLIC_URL")
if [[ "$http_code" != "200" ]]; then
    echo "anonymous GET returned HTTP $http_code (expected 200)" >&2
    echo "URL: $PUBLIC_URL" >&2
    exit 1
fi
echo "  anonymous GET HTTP 200"

# === Verify the bucket is NOT anonymously listable ===
list_code=$(curl -sI -o /dev/null -w '%{http_code}' "https://${BUCKET}.${REGION}.your-objectstorage.com/")
case "$list_code" in
    403) echo "  anonymous LIST denied (HTTP 403): correct" ;;
    200) echo "WARNING: anonymous LIST returned HTTP 200; the bucket is enumerable. Check the policy." >&2 ;;
    *)   echo "  anonymous LIST returned HTTP $list_code" ;;
esac

cat <<EOF

================================================================
Published.

  Public URL:  $PUBLIC_URL

To download from a fresh host:
  curl -fLO "$PUBLIC_URL"

To delete later:
  AWS_ACCESS_KEY_ID=... AWS_SECRET_ACCESS_KEY=... \\
    aws s3 --endpoint-url $ENDPOINT rm s3://$BUCKET/$KEY
================================================================
EOF
