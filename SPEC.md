# kustomize-traverse

Walk a kustomization directory tree and report structural metadata
that `kustomize build` does not expose: the set of local directories
visited and the namespace resolution at each level.

## Motivation

`kubectl-yconverge` needs to find `yconverge.cue` check files that
live next to `kustomization.yaml` in base directories. It also needs
the resolved namespace for each target so check commands can reference
`$NAMESPACE`. Today this is approximated with bash heuristics that
break when a kustomization has multiple local directory resources.

Using `sigs.k8s.io/kustomize/api/types` to parse `kustomization.yaml`
gives us the same directory and namespace resolution that kustomize
itself uses, without rendering resources.

## Usage

```
kustomize-traverse [flags] <path>
```

`<path>` is a directory containing `kustomization.yaml`.

## Flags

```
  -o, --output FORMAT   Output format (default: dirs)
                        dirs       One local directory per line, depth-first
                        namespace  Print only the resolved namespace
                        json       JSON object with dirs and namespace
  -q, --quiet           Suppress warnings (e.g. unresolvable remote refs)
```

## Output formats

### `-o dirs` (default)

One line per local directory visited during traversal, depth-first
order (bases before the referencing overlay). Includes only directories
that contain a `kustomization.yaml`. Remote refs (github URLs, HTTP)
are skipped silently.

```
$ kustomize-traverse -o dirs gateway-v4/site-apply-namespaced/
../../site-chart-v1/generated/modules/settings-sitevalues
../../site-chart-v1/generated/modules/settings-auth
../site-apply
.
```

Paths are relative to `<path>`. The final `.` is the target itself.

Shell consumption:

```bash
for dir in $(kustomize-traverse -o dirs "$KUSTOMIZE_DIR"); do
  abs="$KUSTOMIZE_DIR/$dir"
  [ -f "$abs/yconverge.cue" ] && echo "$abs"
done
```

### `-o namespace`

Print only the resolved namespace and exit. Resolution follows
kustomize semantics:

1. The outermost `kustomization.yaml` `namespace:` field wins
2. If unset, walk into the single resource base (if exactly one)
3. If still unset, print nothing (exit 0, empty output)

```
$ kustomize-traverse -o namespace gateway-v4/site-apply-namespaced/
dev
```

Shell consumption:

```bash
NAMESPACE=$(kustomize-traverse -o namespace "$KUSTOMIZE_DIR")
```

### `-o json`

Single JSON object combining both:

```json
{
  "namespace": "dev",
  "dirs": [
    "../../site-chart-v1/generated/modules/settings-sitevalues",
    "../../site-chart-v1/generated/modules/settings-auth",
    "../site-apply",
    "."
  ]
}
```

Shell consumption via jq, or direct use from Go callers.

## Traversal rules

1. Parse `kustomization.yaml` (or `kustomization.yml`, `Kustomization`)
   using `sigs.k8s.io/kustomize/api/types`.
2. Collect `resources` and `components` entries.
3. For each entry that resolves to a local directory containing a
   kustomization file, recurse (depth-first).
4. Skip remote refs (HTTP URLs, `github.com/...`), file refs
   (entries that resolve to files, not directories), and entries
   whose target directory does not exist.
5. Emit each visited directory exactly once (deduplicate by
   resolved absolute path).
6. The target directory itself is always the last entry.

## Namespace resolution

Read the `namespace:` field from the outermost kustomization.
This matches kustomize behavior: the outermost overlay's namespace
overrides all bases.

If the outermost has no `namespace:` field, fall back to the first
base directory that has one (depth-first). This covers the indirection
case where `site-apply-namespaced/` (generated, may lack namespace)
references `../site-apply/` (which declares namespace).

This is not a full reimplementation of kustomize namespace
transformation — it's a static read of the `namespace:` field from
kustomization files, which is sufficient for yconverge's needs.

## Exit codes

- 0: success
- 1: `<path>` does not contain a kustomization file
- 2: flag parse error

Unresolvable remote refs or missing directories are not errors —
they are skipped (with a warning unless `-q`).

## Build

```
go build -o kustomize-traverse .
```

Single dependency: `sigs.k8s.io/kustomize/api` for the types.
No need for the full krusty engine — only `types.Kustomization`
unmarshaling and local filesystem access.

## Integration with kubectl-yconverge

Replace `_find_cue_dir()` in `kubectl-yconverge` with:

```bash
_find_cue_dirs() {
  kustomize-traverse -o dirs "$1" | while read -r rel; do
    abs="$1/$rel"
    [ -f "$abs/yconverge.cue" ] && echo "$abs"
  done
}

NAMESPACE=$(kustomize-traverse -o namespace "$KUSTOMIZE_DIR")
export NAMESPACE
```

This replaces both the namespace guessing logic and the
single-level CUE file lookup with kustomize-native resolution.
