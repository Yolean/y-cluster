# shellcheck shell=bash
# Sourced by the appliance workflow scripts. The hook contract lets a
# downstream repo run its own converge and verification inside a
# y-cluster workflow, against the y-cluster binary the workflow built,
# without y-cluster knowing anything about that repo.
#
# The Y_CLUSTER_CURRENT_* names are an interface with consumers outside
# this repo. testdata/appliance-hooks/ asserts every one of them; a
# rename has to be coordinated with those consumers.
#
# Values not known yet at the call site are exported EMPTY rather than
# left unset, so a hook can read them under `set -u`.

# hooks_env_local exports the part of the contract every workflow
# shares. Expects NAME, KUBECTX, Y_CLUSTER and CACHE_DIR to be set.
hooks_env_local() {
    export Y_CLUSTER_CURRENT_BIN="$Y_CLUSTER"
    export Y_CLUSTER_CURRENT_NAME="$NAME"
    export Y_CLUSTER_CURRENT_KUBECTX="$KUBECTX"
    export Y_CLUSTER_CURRENT_LOCAL_HTTP_PORT="${APP_HTTP_PORT:-80}"
    export Y_CLUSTER_CURRENT_LOCAL_HTTPS_PORT="${APP_HTTPS_PORT:-443}"
    export Y_CLUSTER_CURRENT_LOCAL_API_PORT="${APP_API_PORT:-6443}"
    export Y_CLUSTER_CURRENT_LOCAL_SSH_PORT="${APP_SSH_PORT:-2222}"
    export Y_CLUSTER_CURRENT_LOCAL_SSH_KEY="${CACHE_DIR:-}/${NAME}-ssh"
    export Y_CLUSTER_CURRENT_BUNDLE_DIR="${BUNDLE_DIR:-}"
}

# hooks_run <label> <cmd> runs a caller-supplied command string, or
# nothing when it is empty. pipefail is on so a `cmd | tee log` in the
# string cannot hide the failure of cmd. The caller exports the env
# first; a non-zero exit aborts the workflow through set -e.
hooks_run() {
    local label=$1 cmd=$2
    [[ -n "$cmd" ]] || return 0
    stage "hook: $label"
    bash -c "set -o pipefail; $cmd"
}
