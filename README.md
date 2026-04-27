# y-cluster

Single Go binary for local Kubernetes cluster lifecycle, image
management, and declarative convergence. Replaces a stack of
shell scripts that previously drove ystack and checkit's local
clusters.

## What it does

```
$ y-cluster --help        # full subcommand list
```

Subcommand groups:

- **provision / teardown / export / import** — bring up a local
  k3s cluster (qemu VM or k3s-in-docker), tear it down, or move
  the disk between hosts as a VMware appliance.
- **yconverge** — apply a kustomize base with ordering by CUE
  imports and post-apply checks. Symlink the binary as
  `kubectl-yconverge` to use it as a kubectl plugin.
- **detect / ctr / crictl** — discover the local cluster's
  backend by kubeconfig context and run `ctr` or `crictl` on the
  node through the right transport (Docker daemon API for the
  docker provisioner, SSH for qemu). Replaces ystack's
  `y-cluster-local-{detect,ctr,crictl}`.
- **images list / cache / load** — extract image refs from a
  YAML stream, pull a single ref into a local OCI cache, or
  stream an OCI archive into the cluster node's containerd. The
  airgap path for both system images (handled inside provision)
  and arbitrary user-built images.
- **cache info / purge** — inspect or wipe y-cluster's shared
  download cache (k3s airgap bundles, image OCI layouts).
- **serve / serve ensure / serve stop / serve logs** — a
  lightweight HTTP server that exposes config assets to the
  cluster: kustomize-built Secrets named
  `y-kustomize.{group}.{name}` become `/v1/{group}/{name}/{key}`
  URLs. Replaces the y-kustomize service in ystack.

Every subcommand has its own `--help` with the flags and
context. The README is intentionally short — when something is
discoverable from `y-cluster <cmd> --help`, that's where it
lives.

## Two ideas worth knowing before you start

**yconverge: ordering vs checks come from different places.**
CUE imports in `yconverge.cue` declare ordering — each import is
a *separate* yconverge invocation that runs its own apply and
checks before yours. Kustomize tree traversal collects checks
across the whole base, so an overlay's checks include the base's.
The two mechanisms are deliberately separate:
*ordering across modules* uses CUE; *checking after one apply*
uses traversal. `y-cluster yconverge --help` has the rule.

**serve: the URL is derived from the Secret name.** A Secret
called `y-kustomize.kafka.setup-topic-job` with a data key
`base-for-annotations.yaml` is served at
`/v1/kafka/setup-topic-job/base-for-annotations.yaml`. This is
true whether the Secret comes from `kustomize build` of a local
source (`type: y-kustomize-local`) or a Kubernetes informer
(`type: y-kustomize-incluster`). The two modes are
interchangeable; switch by changing `type:` in
`y-cluster-serve.yaml`.

## Specs

Design notes, migration recipes, and the still-pending feature
spec live in [`../specs/y-cluster/`](../specs/y-cluster). The
binary's behaviour is the source of truth for what's
implemented; the specs are kept for design rationale and
in-flight scope.

## Issues / feedback

[github.com/Yolean/y-cluster/issues](https://github.com/Yolean/y-cluster/issues)
