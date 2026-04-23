# y-cluster

Idempotent Kubernetes convergence with dependency ordering and checks.

## Core concept

y-cluster has one fundamental operation:

```
y-cluster yconverge -k path/to/base/
```

This applies a kustomize base to the cluster and runs checks defined
in `yconverge.cue` files found in the base's directory tree.

Two separate mechanisms control **what gets checked** and **what gets
converged first**. Understanding the difference is essential.

### Checks: kustomize tree traversal

After applying a base, y-cluster walks the kustomize directory tree
to find all `yconverge.cue` files. Checks from every local base
directory run after the apply. This is **check aggregation** — it
answers "what must be true after this apply?"

Example: `site-apply-namespaced/` references `../site-apply/` which
has a `yconverge.cue` with a rollout check. The check runs after the
combined kustomize output is applied, because the check belongs to
the resources that were applied.

Traversal only follows local directories. Remote refs (github URLs,
HTTP resources) are skipped — they contribute resources to the
kustomize build but their checks are not aggregated.

### Dependencies: CUE imports

Before applying a base, y-cluster reads CUE import statements in
`yconverge.cue` to build a dependency graph. Each dependency is
converged as a **separate yconverge invocation** — its own apply
and its own checks — before the target base.

Example: keycloak's `yconverge.cue` imports the mysql CUE module.
y-cluster converges mysql first (apply mysql resources, run mysql
checks), then converges keycloak (apply keycloak resources, run
keycloak checks). These are two separate apply+check cycles.

### Why the distinction matters

Kustomize apply is atomic — all resources in the kustomize output
are applied at once. Checks run after the entire apply completes.
There is no way to check an intermediate state within a single
kustomize apply.

CUE imports create separate convergence steps. Each step has its
own apply and checks. This is how you express "mysql must be healthy
before keycloak starts."

The rule:

- **kustomize resources** are for customization — overlays, patches,
  namespace scoping, image overrides. They produce a single atomic
  apply. Checks from the entire tree verify the result.

- **CUE imports** are for ordering — they declare dependencies
  between independently convergeable bases. Each dependency is
  a separate yconverge invocation with its own checks.

Do not use kustomize `resources:` to bundle independent modules
that need ordered convergence. Use CUE imports instead.

### Super bases

A convergence target can have an empty `kustomization.yaml` (no
resources to apply) and a `yconverge.cue` that imports multiple
bases. Running yconverge on it converges all imports in dependency
order, applies nothing (empty kustomization), and runs any
top-level checks.

This is a clean way to define "converge these bases together":

```yaml
# converge-default/kustomization.yaml
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
# No resources — this is a convergence orchestration target
```

```cue
// converge-default/yconverge.cue
package converge_default

import (
    "yolean.se/ystack/k3s/29-y-kustomize:y_kustomize"
    "yolean.se/ystack/k3s/30-blobs:blobs"
    "yolean.se/ystack/k3s/60-builds-registry:builds_registry"
)

_dep_kustomize: y_kustomize.step
_dep_blobs:     blobs.step
_dep_registry:  builds_registry.step

step: verify.#Step & {
    checks: []
}
```

```
y-cluster yconverge -k converge-default/
```

This replaces a comma-separated list of targets with a declarative
dependency graph. The tool resolves the imports, converges each base
in topological order, and exits.

## Usage

```
# Apply a base with checks
y-cluster yconverge --context=local -k path/to/base/

# Check only (no apply)
y-cluster yconverge --context=local --checks-only -k path/to/base/

# Print dependency order
y-cluster yconverge --context=local --print-deps -k path/to/base/

# Dry run (validate against API server, no mutation)
y-cluster yconverge --context=local --dry-run=server -k path/to/base/

# Inspect kustomization tree
y-cluster traverse -k path/to/base/
y-cluster traverse -k path/to/base/ --namespace

# Image management
y-cluster images list -k path/to/base/
y-cluster images cache -k path/to/base/
y-cluster images load -k path/to/base/

# Cluster provisioning
y-cluster provision --provider=qemu
y-cluster teardown
```

## Check types

Checks are defined in `yconverge.cue` next to `kustomization.yaml`:

```cue
package my_base

import "yolean.se/ystack/yconverge/verify"

step: verify.#Step & {
    checks: [
        {
            kind:     "rollout"
            resource: "deployment/my-app"
            timeout:  "120s"
        },
        {
            kind:        "exec"
            command:     "curl -sf http://$NAMESPACE.example.com/"
            timeout:     "60s"
            description: "app responds"
        },
    ]
}
```

Three check types:
- **wait** — `kubectl wait --for=<condition>` on a resource
- **rollout** — `kubectl rollout status` on a deployment/statefulset
- **exec** — arbitrary shell command, retried until timeout

Environment variables available to exec commands:
- `$CONTEXT` — Kubernetes context name
- `$NAMESPACE` — resolved namespace for this base

## Suggested ystack super bases

These replace the `--converge=LIST` bash pattern with declarative
CUE dependency graphs. Each is an empty kustomization + yconverge.cue.

### converge-default

The minimal ystack infrastructure. Equivalent to the current
default `y-kustomize,blobs,builds-registry`.

Since `builds-registry` already imports `blobs`, `kafka-ystack`,
and `y-kustomize` via CUE, a single yconverge call resolves the
full chain. The super base just makes this explicit:

```cue
// k3s/converge-default/yconverge.cue
import "yolean.se/ystack/k3s/60-builds-registry:builds_registry"
_dep: builds_registry.step
step: verify.#Step & { checks: [] }
```

### converge-with-kafka

Default infrastructure plus kafka (redpanda). For dependents
that need topic creation.

```cue
// k3s/converge-with-kafka/yconverge.cue
import (
    "yolean.se/ystack/k3s/60-builds-registry:builds_registry"
    "yolean.se/ystack/k3s/40-kafka:kafka"
)
_dep_registry: builds_registry.step
_dep_kafka:    kafka.step
step: verify.#Step & { checks: [] }
```

### converge-with-buildkit

Full build infrastructure. For dependents that build images
via skaffold/buildkitd.

```cue
// k3s/converge-with-buildkit/yconverge.cue
import (
    "yolean.se/ystack/k3s/62-buildkit:buildkit"
    "yolean.se/ystack/k3s/40-kafka:kafka"
)
_dep_buildkit: buildkit.step
_dep_kafka:    kafka.step
step: verify.#Step & { checks: [] }
```

Since `buildkit` imports `builds-registry` which imports `blobs`
and `y-kustomize`, the full chain is resolved from two leaf imports.

### Usage from a dependent

```bash
y-cluster yconverge --context=local -k "$YSTACK_HOME/k3s/converge-with-kafka/"
```

One command, full dependency resolution, per-step checks.
