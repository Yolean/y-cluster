#!/usr/bin/env bash
# End-to-end test for a released y-cluster binary.
#
# Expects $Y_CLUSTER_BIN to point at a y-cluster executable. Creates a
# temp workspace with a two-base y-kustomize-local fixture, runs
#   serve ensure → GET → serve stop
# and exits non-zero on any assertion failure.
#
# Intended to run in .github/workflows/e2e-release.yaml on ubuntu-latest
# and macos-latest against the extracted release archive.
set -euo pipefail

Y_CLUSTER_BIN="${Y_CLUSTER_BIN:-./y-cluster}"
if [ ! -x "$Y_CLUSTER_BIN" ]; then
  echo "Y_CLUSTER_BIN is not executable: $Y_CLUSTER_BIN" >&2
  exit 2
fi

work=$(mktemp -d 2>/dev/null || mktemp -d -t 'y-cluster-e2e')
trap '"$Y_CLUSTER_BIN" serve stop --state-dir "$work/state" >/dev/null 2>&1 || true; rm -rf "$work"' EXIT

cfg="$work/config"
src_a="$work/sources/a"
src_b="$work/sources/b"
state="$work/state"
mkdir -p "$cfg" "$src_a/blobs-setup-bucket-job" \
  "$src_b/kafka-setup-topic-job" "$state"

# y-kustomize-local now requires a kustomization.yaml at each
# source root: serve runs `kustomize build` against the dir,
# which surfaces a Secret per group/name that the HTTP layer
# unpacks into /v1/<group>/<name>/<file> routes.
cat >"$src_a/kustomization.yaml" <<'EOF'
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
secretGenerator:
- name: y-kustomize.blobs.setup-bucket-job
  options:
    disableNameSuffixHash: true
  files:
  - blobs-setup-bucket-job/base-for-annotations.yaml
  - blobs-setup-bucket-job/values.yaml
generatorOptions:
  disableNameSuffixHash: true
EOF

cat >"$src_a/blobs-setup-bucket-job/base-for-annotations.yaml" <<'EOF'
apiVersion: batch/v1
kind: Job
metadata:
  name: setup-bucket-job
EOF

cat >"$src_a/blobs-setup-bucket-job/values.yaml" <<'EOF'
bucket: builds
EOF

cat >"$src_b/kustomization.yaml" <<'EOF'
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
secretGenerator:
- name: y-kustomize.kafka.setup-topic-job
  options:
    disableNameSuffixHash: true
  files:
  - kafka-setup-topic-job/base-for-annotations.yaml
generatorOptions:
  disableNameSuffixHash: true
EOF

cat >"$src_b/kafka-setup-topic-job/base-for-annotations.yaml" <<'EOF'
apiVersion: batch/v1
kind: Job
metadata:
  name: setup-topic-job
EOF

# Pick an ephemeral port: ask Python (present on both runners).
port=$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1]); s.close()')
cat >"$cfg/y-cluster-serve.yaml" <<EOF
port: $port
type: y-kustomize-local
sources:
- dir: $src_a
- dir: $src_b
EOF

echo "--> starting y-cluster serve on :$port"
"$Y_CLUSTER_BIN" serve ensure -c "$cfg" --state-dir "$state"

echo "--> GET /health"
curl -fsS "http://127.0.0.1:$port/health" | grep -q '"ok":true'

echo "--> GET a file from source A"
body=$(curl -fsS "http://127.0.0.1:$port/v1/blobs/setup-bucket-job/values.yaml")
echo "$body" | grep -q "bucket: builds"

echo "--> GET a file from source B"
curl -fsS "http://127.0.0.1:$port/v1/kafka/setup-topic-job/base-for-annotations.yaml" \
  | grep -q "setup-topic-job"

echo "--> GET /openapi.yaml"
spec=$(curl -fsS "http://127.0.0.1:$port/openapi.yaml")
echo "$spec" | grep -q "/v1/blobs/setup-bucket-job/values.yaml"
echo "$spec" | grep -q "/v1/kafka/setup-topic-job/base-for-annotations.yaml"

echo "--> ETag + 304"
etag=$(curl -fsS -o /dev/null -D - "http://127.0.0.1:$port/v1/blobs/setup-bucket-job/values.yaml" \
  | awk -F': ' 'tolower($1)=="etag"{gsub(/\r/,"",$2); print $2}')
if [ -z "$etag" ]; then
  echo "missing ETag" >&2; exit 1
fi
code=$(curl -s -o /dev/null -w "%{http_code}" \
  -H "If-None-Match: $etag" \
  "http://127.0.0.1:$port/v1/blobs/setup-bucket-job/values.yaml")
if [ "$code" != "304" ]; then
  echo "conditional GET expected 304, got $code" >&2; exit 1
fi

echo "--> ensure again is a no-op"
# Typed Ensure result emits "y-cluster serve <action> on :<port>" to
# stdout, where <action> is started / restarted / noop. A second
# ensure with no config drift must report noop.
"$Y_CLUSTER_BIN" serve ensure -c "$cfg" --state-dir "$state" | grep -q "noop"

echo "--> stop"
"$Y_CLUSTER_BIN" serve stop --state-dir "$state"

echo "--> stop is idempotent"
"$Y_CLUSTER_BIN" serve stop --state-dir "$state"

echo "OK"
