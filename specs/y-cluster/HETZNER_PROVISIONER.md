# Hetzner provisioner: dev-cluster shape

A new `provider: hetzner` for `y-cluster` that gives developers
remote dev clusters on Hetzner Cloud, with a per-developer shared
HTTPS LoadBalancer in front and an auto-teardown safety belt to
keep idle clusters from leaking billing. Lives at
`pkg/provision/hetzner/`, mirroring the qemu provisioner's API
surface so `cmd/y-cluster`'s dispatch picks it up uniformly.

The branch `hetzner-provisioner` (off `appliance-workflows`) hosts
this work. It does not modify the dev binary at
`~/Yolean/ystack/bin/y-cluster-v0.4.1-dev-bin`; that swap happens
on operator command after this branch lands.

## End goals

1. `y-cluster provision -c <dir>` with `provider: hetzner` brings
   up a Hetzner Cloud server with k3s + envoy-gateway, attached to
   a per-developer shared LoadBalancer terminating TLS on :443
   (self-signed by default). The cluster's `GatewayClass`
   `yolean.se/dns-hint-ip` annotation is set to the LB's public IP
   so ystack's `y-k8s-ingress-hosts` resolves the right host on
   first try.
2. Subcommand parity with qemu where it makes sense: `stop`,
   `start`, `pause`, `resume`, `teardown`, `ctr`, `crictl`,
   `RunShell`, `images load`. `prepare-export` returns a clean
   "not supported on hetzner" error -- Hetzner Cloud has no
   custom-disk-image upload API, so the appliance contract
   (local-built disk = disk that boots elsewhere) doesn't apply.
3. `y-cluster images load` accepts `--from-url=https://...` in
   addition to stdin / file. The URL stream is piped straight into
   the existing SSH-piped containerd-load path; no caching.
4. **Auto-teardown is unconditional and operator-overridable.**
   Default 8 hours, configurable via `autoTeardownHours`. `at(1)`
   on the operator's host fires `y-cluster teardown -c <dir>` at
   the deadline. Teardown removes the at job. Server is also
   labeled `expires-at=<unix-ts>` so a future server-side reaper
   can pick up clusters whose host went offline.
5. Validation is strict at config-load time:
   - `context` is required, must not equal `"local"`, must be
     >= 4 characters, must match `^[a-z][a-z0-9-]{2,}$` (DNS-label
     safe).
   - The Hetzner server name is forced to equal `context`.
   - `provider: hetzner` rejected without `context`.
6. **LoadBalancer is mandatory and per-developer-shared.**
   - `lbGroup` defaults to `$USER`. First Provision creates the
     LB; subsequent Provisions of different contexts add their
     server as a target. Teardown detaches; once the last target
     leaves, the LB itself is deleted.
   - Self-signed cert by default. Cert covers the union of all
     attached servers' FQDNs, regenerated on attach/detach.
7. Per-developer credentials. Each dev has their own `HCLOUD_TOKEN`
   in `~/Yolean/.yolean-bots-device/y-cluster-hetzner-$USER.env`
   (overridable). Project (`yo-sre-appliance-qa` for now) is shared.
8. **No interference with `appliance-workflows`.** New code only
   in `pkg/provision/hetzner/`, plus minimal dispatch additions
   in `pkg/provision/config/`, `pkg/cluster/lookup.go`,
   `cmd/y-cluster/main.go`. Existing qemu / docker / multipass
   tests stay green.

## Decisions (locked)

These are the answers to the open questions in the conversation
plan. Recorded here so the choices survive the iteration; commit
messages reference this section by phase.

| Decision | Choice | Trade |
|---|---|---|
| Auto-teardown mechanism | In-cluster reaper Job (`hetznercloud/cli` image, sleeps `autoTeardownHours`, then `hcloud server delete` + conditional LB delete) | Survives operator-host loss; cluster reboot resets the timer (acceptable for dev). Trade vs. an earlier `at(1)`-on-host attempt: that approach got reverted in commit `7a78e3f` because a wiped/retired laptop strands paid resources. |
| FQDN domain default | `<context>.<lbGroup>.local.test` | RFC 6761 reserved TLD; never routes anywhere if /etc/hosts is missing. Operator overrides via `HETZNER_FQDN_DOMAIN` / `fqdnDomain` config field for real DNS. |
| Cert rotation on attach | Re-issue self-signed covering the new SAN union | Brief window where the new server's FQDN isn't yet covered by the old cert. Self-signed already warns; not worse than the baseline. |
| DNS plumbing | Out of scope | Each dev hits their own cluster via `/etc/hosts` written by `y-k8s-ingress-hosts`. Real-DNS integration is a separate (later) feature. |
| LB scope | Per-developer (`$USER` keyed) | Two devs in the same shared project get two LBs. Cleaner isolation than per-project; cost per dev is one LB (€5.39/mo flat for LB11). |
| `prepare-export` on hetzner | Returns `not supported on hetzner provider (Hetzner Cloud has no custom-disk-image upload API); use the qemu provisioner for disk-bound appliances` | Hetzner has no disk-upload API. The qemu→GCE/VirtualBox/Hetzner-as-target shape stays in qemu-land. |
| `stop` / `start` on hetzner | Map to `hcloud Server.Shutdown` / `Server.Poweron`. Stop is graceful ACPI; start re-fetches the public IPv4. | Billing for the disk continues while stopped — Hetzner has no "frozen, not billed" state for cx-class servers. The value of stop/start is preserving cluster identity (server ID, IPv4, kubeconfig context) across breaks; for billing relief use `teardown`. |
| `pause` / `resume` on hetzner | Clean refusal: `not supported on hetzner provider (Hetzner Cloud has no pause/resume primitive); use stop/start or teardown` | No SIGSTOP/SIGCONT analog at the Hetzner API; surfacing it as "not yet implemented" would invite false expectation. |
| `images load --from-url` cache | Stateless | Re-running re-downloads. Operators can wrap with mtime checks if they want. Caching adds state surface for negligible win. |
| Server type default | `cx23` | Matches `scripts/appliance-build-hetzner.sh` already-tested config. Operator overrides via `serverType`. |
| Location default | `hel1` | Stockholm-equivalent latency for Yolean ops; same default as the existing scripts. |
| Image | `ubuntu-24.04` | k3s airgap install on Ubuntu cloud image; same shape as qemu. |
| SSH key | Rotated per-context, stored in `~/.cache/y-cluster-hetzner/<context>-ssh{,.pub}` | Mirrors qemu's per-VM key isolation. |
| `PortForwards` from CommonConfig | Ignored on hetzner (the server is on the public internet; LB does the routing) | Validate accepts but warns when set. |

## Architecture

```
                                        Hetzner Cloud project
                                        (yo-sre-appliance-qa)
                                                │
                                                ▼
        ┌──────────────────────────────────────────────────────┐
        │                                                      │
        │  Per-developer LB (lbGroup=$USER):                   │
        │   - one Hetzner Load Balancer (LB11)                 │
        │   - one uploaded SSL cert (self-signed by default)   │
        │   - HTTPS:443 -> HTTP:80                             │
        │   - targets = the dev's currently-active servers     │
        │                                                      │
        │      ┌─────────────────┐    ┌─────────────────┐      │
        │      │  Server A       │    │  Server B       │      │
        │      │  ctx=alice-dev1 │    │  ctx=alice-dev2 │      │
        │      │  k3s + EG       │    │  k3s + EG       │      │
        │      │  expires-at=... │    │  expires-at=... │      │
        │      └────────┬────────┘    └────────┬────────┘      │
        │               │ tag: y-cluster                       │
        │               │      hetzner                         │
        └───────────────┼──────────────────────┼───────────────┘
                        │                      │
                        ▼                      ▼
               ┌──────────────────────────────────────┐
               │  cluster.Lookup hits Hetzner API     │
               │   - hcloud server list -l           │
               │   - matches `name == context`        │
               │   - SSH host = public IPv4           │
               │   - SSH user = ystack                │
               │   - SSH key from cacheDir            │
               └──────────────────────────────────────┘

        Operator's host:
          ~/.cache/y-cluster-hetzner/<context>-ssh           # rotated per-provision
          ~/.cache/y-cluster-hetzner/<context>.json          # state sidecar
          atq job -> `y-cluster teardown -c <cfg-dir>`       # auto-teardown
          /etc/hosts entries via `y-k8s-ingress-hosts`
```

### Cloud-init delivery

Hetzner's `user_data` lands as a NoCloud cidata volume from the
guest's POV. The existing `datasource_list: [NoCloud, None]` pin
in `pkg/provision/qemu/qemu.go`'s `renderCloudInitUserData` keeps
working. The hetzner provisioner reuses the same renderer (or a
near-clone) for the airgap install.

### Auto-teardown

Lives **inside the cluster**, not on the operator's host. See
`pkg/provision/hetzner/reaper.go` and `reaper-job.yaml`.

- Provision: after k3s + envoy-gateway are up, applies a
  `Job` in namespace `y-cluster-reaper` running
  `hetznercloud/cli:v1.64.1`. The container `sleep`s
  `autoTeardownHours`, then issues `hcloud server delete
  <serverID>` and (only when this server was the last
  `lb-group=$grp` member) `hcloud load-balancer delete <lbID>`.
- Token: the operator's `HCLOUD_TOKEN` is captured at provision
  time and stored as a `Secret` in the reaper namespace. Token
  rotation on the operator's side breaks the in-cluster reaper;
  documented as a known limitation.
- Teardown (interactive, by the operator): the in-cluster Job
  becomes redundant on a manual `y-cluster teardown -c <dir>`,
  but is harmless -- the server it would delete is already gone,
  and the LB-emptiness check it does is the same one Teardown
  ran. No `kubectl delete job` is required.
- Limited blast radius: the reaper only deletes resources whose
  IDs were captured at provision time. A later, unrelated cluster
  re-using the same `lb-group` cannot be touched.
- Out of scope for the in-cluster reaper: Hetzner Certificate
  cleanup (needs LB detach-then-delete sequencing; left to the
  interactive Teardown) and SSH-key cleanup (free; sweep
  periodically).

### Shared LB lifecycle

```
  Provision context=alice-dev1
    ├─ ensure LB `dev-alice` exists (create if absent)
    ├─ create server `alice-dev1`
    ├─ add server as LB target
    ├─ regenerate cert: SAN list = [alice-dev1.alice.local.test]
    └─ set GatewayClass annotation = <LB public IP>

  Provision context=alice-dev2
    ├─ LB `dev-alice` exists, reuse
    ├─ create server `alice-dev2`
    ├─ add server as LB target
    ├─ regenerate cert: SAN list = [alice-dev1.alice.local.test, alice-dev2.alice.local.test]
    └─ set GatewayClass annotation = <LB public IP>     (same)

  Teardown context=alice-dev1
    ├─ remove server from LB targets
    ├─ remaining targets = [alice-dev2]
    ├─ regenerate cert: SAN list = [alice-dev2.alice.local.test]
    ├─ delete server `alice-dev1`
    └─ keep LB

  Teardown context=alice-dev2
    ├─ remove server from LB targets
    ├─ remaining targets = []
    ├─ delete LB cert
    ├─ delete LB
    └─ delete server `alice-dev2`
```

### `images load` URL flow

The CLI takes a single positional source, dispatching by shape:

```
  y-cluster images load <archive|-|url>

  -> if URL  (https://...):  curl -fsSL <url> | <existing SSH-piped containerd load>
  -> if file (path):         cat <file>       | <existing SSH-piped containerd load>
  -> if "-"  (stdin):        existing stdin path
```

No caching at this layer. The URL stream IS the stdin from the
SSH-pipe's POV. (For *durable* caching across provisions, see the
"Image cache via Hetzner S3" section below.)

## Phases

| # | Deliverable | Tests | Branch state at end |
|---|---|---|---|
| 0 | Scaffolding: this doc, ProviderHetzner const, HetznerConfig + Validate, AllProviders / AllBackends extension | Unit tests for HetznerConfig validation | Compiles; existing tests green; no Hetzner API calls |
| 1 | Bare Provision/Teardown over Hetzner Cloud | e2e (hetzner build tag) skipped without HCLOUD_TOKEN | Working bare provisioner |
| 2 | Auto-teardown via at(1) | Unit tests for at-spec generation | Critical safety against billing leaks |
| 3 | Shared LB + TLS + dns-hint-ip | e2e: provision two contexts share LB; teardown decommissions | Multi-server LB working |
| 4 | `images load --from-url` | Unit tests for URL detection | All `images load` inputs symmetric |
| 5 | Docs + per-dev .env defaults + polish | All tests green | PR-ready |

## Per-phase commit shape

Each phase lands as one or more focused commits. Each commit
message references the relevant **Decisions** row when a choice
was made and explains the trade. The pattern (rough):

```
feat(provision/hetzner): <phase-N concern>

<what changed>

Decisions referenced:
  - "Auto-teardown mechanism: at(1)"  (HETZNER_PROVISIONER.md)
  - "Cert rotation on attach: re-issue covering union"

<learnings>

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
```

## Image cache via Hetzner S3 (proposed phase 6)

The end goal is a remote dev cluster that *feels* like a local
one. On local, image pulls hit a kind/multipass disk in
microseconds. On Hetzner, image pulls cross the public internet
to Docker Hub / quay / GHCR and are subject to rate-limiting
and ~hundreds-of-MB-per-image latency. That's the dominant
"it's slow" experience we're trying to delete.

Solution shape: keep all required images in **Hetzner Object
Storage in the same region as the cluster** (`hel1`). Pulls
become intra-region GETs, ~ms-class. Layered on top: an opt-in
**pull-rejection mode** that makes upstream pulls a hard error,
forcing the cache to stay complete.

### End goals

1. Operator runs `y-cluster images push <ref>` and the image
   lands in S3 in a containerd-loadable shape, idempotent on
   the resolved digest.
2. `y-cluster provision` (when `imageCache.bucket` is set) lists
   the bucket's image index and pre-loads every entry into
   the node's containerd before kubelet ever schedules a pod.
   No upstream-registry traffic during the dev loop.
3. Optional `imageCache.rejectUpstream: true` drops a k3s
   `registries.yaml` that maps every registry to the empty
   mirror set. Any image-ref that isn't already in containerd's
   store fails loudly. The dev iterates against the cache, not
   around it.

### Decisions (locked, image-cache scope)

| Decision | Choice | Trade |
|---|---|---|
| Storage backend | Hetzner Object Storage (S3-compat), region `hel1`, default bucket `y-cluster-examples`. Endpoint `https://<region>.your-objectstorage.com`. | Same blast-radius scope as the cluster itself; latency negligible intra-region. Outbound from S3 is free for traffic to Hetzner Cloud servers in the same project. |
| On-S3 layout | Per-image OCI v1 image-layout under `s3://<bucket>/oci/<safe-ref>/<digest>/` (mirrors the local `<cacheRoot>/images/<digest>/` shape `pkg/images.Cache` already produces). Layer dedup is a phase-7 follow-up if needed. | Each `images push` uploads one self-contained directory. Listing the bucket with prefix `oci/` is the index. |
| Index | A flat object at `s3://<bucket>/index.json` mapping `<original-ref>` → `<digest>` → `<oci-layout-prefix>`. Updated on every push. | One source of truth for "what's available"; no listing-API parsing. Concurrent pushes need an etag-based update (CAS); v1 ships single-writer. |
| Operator-side S3 client | `github.com/minio/minio-go/v7`. Minimum-surface S3 client (no AWS SDK chains), Hetzner-tested, ~few MB module. | Adds one direct dep; we already have `go-containerregistry` for the OCI side. |
| Cluster-side S3 fetch | Use the `hetznercloud/cli` image (already pulled by the reaper) plus `wget`/`curl` against pre-signed URLs generated at provision time. | No AWS CLI install on the node; signed URLs scope cluster credentials away from the host. URLs expire (default 1h); we issue them just before the load step. |
| Sideload mechanism | For each image: download the OCI layout to a tmpdir on the node, `tar -cf - .` over the layout dir, pipe to `ctr -n k8s.io image import -`. Same pipeline `images load` already drives — symmetry over re-implementation. | Tar-of-layout is `ctr image import`-compatible. Parallelism: serial for v1 (one image at a time); we can parallelize once the slow case is measured. |
| Config field | `imageCache: { bucket: "...", region: "hel1", indexKey: "index.json", rejectUpstream: false }` on `HetznerConfig`. Empty `bucket` disables the whole feature. | Per-cluster opt-in; defaults preserve the current "pull from upstream" behavior. |
| Lockdown mechanism | Drop `/etc/rancher/k3s/registries.yaml` with every wildcard mirror set to `endpoint: []` and `rewrite` blank. K3s's documented knob; survives k3s restart. | k3s-specific (acceptable: hetzner provisioner ships k3s anyway). Errors during a missed cache fall on the operator immediately, which is the goal. |
| Credentials shape | Operator: same `~/Yolean/.yolean-bots-device/y-cluster-hetzner.env` already carrying `HCLOUD_TOKEN`. New keys: `H_S3_ACCESS_KEY`, `H_S3_SECRET_KEY`, `H_S3_REGION`, `H_S3_BUCKET`. Cluster: receives pre-signed URLs (no key material on disk). | Single source of truth for credentials. Pre-signed URLs avoid baking long-lived keys into a node Secret. |

### `images push` flow

```
  y-cluster images push <ref> [--bucket=...] [--region=hel1]

  -> resolve <ref> via go-containerregistry remote.Head  (same as `images cache`)
  -> if local OCI layout under <cacheRoot>/images/<digest>/ doesn't exist:
       run pkg/images.Cache to populate it
  -> walk the layout, PUT every blob + manifest under s3://<bucket>/oci/<safe-ref>/<digest>/
  -> read+update s3://<bucket>/index.json: add (ref, digest, prefix)
  -> log the digest-pinned ref (so the operator can paste it into kustomize / a deployment)
```

Idempotent: a push of the same digest is a no-op (object existence check).

### Provision-time pre-load flow

```
  y-cluster provision  (with imageCache.bucket set)

  -> SSH into the new node
  -> for each (ref, digest, prefix) in s3://<bucket>/index.json:
       generate a presigned GET URL for the manifest + each blob
       wget/curl the OCI layout directory tree into /tmp/oci-load/<digest>/
       tar -cf - -C /tmp/oci-load/<digest> . | sudo k3s ctr -n k8s.io image import -
       ctr -n k8s.io image tag <ref> <ref>@<digest>   (same digest-alias step as `images load`)
  -> if imageCache.rejectUpstream:
       write /etc/rancher/k3s/registries.yaml with empty-mirror wildcard
       sudo systemctl restart k3s
```

### `imageCache.rejectUpstream` semantics

Implemented (post-design): the wildcard mirror in
`/etc/rancher/k3s/registries.yaml` alone wasn't sufficient. K3s
v1.35.4 still falls back to the original registry in practice --
verified via e2e: a `crictl pull alpine:3.20` succeeded despite
the wildcard reject mirror. The shipped lockdown drops a
containerd `hosts.toml` directly under
`/var/lib/rancher/k3s/agent/etc/containerd/certs.d/<reg>/`
for `_default` plus the major upstream registries:

```toml
server = "http://reject-upstream-by-y-cluster.invalid:9999"

[host."http://reject-upstream-by-y-cluster.invalid:9999"]
  capabilities = ["pull", "resolve"]
```

When containerd finds a `hosts.toml` in a registry's certs.d
directory, it treats the file as authoritative and does NOT fall
back to the registry's real hostname. DNS on the `.invalid` URL
fails (RFC 6761 reserved), and the pull errors out instead of
being silently satisfied by docker.io.

`registries.yaml` is still written (with the `"*"` wildcard) so
a future `k3s server restart` regenerates certs.d the same way.
Both files are in sync; either one is sufficient on its own,
together they survive the restart edge case.

Race fix: applyRejectUpstream waits for the in-cluster reaper
Pod to reach Running before dropping hosts.toml. Without that
gate, the lockdown lands mid-pull of `hetznercloud/cli` and the
reaper ends up ImagePullBackOff -- killing the auto-teardown
safety net. The wait costs ~6s on a fresh node.

This option is OFF by default. Its purpose is to enforce a "no
cache miss = no provisioning surprise" workflow during e2e runs
that are sensitive to upstream registry availability.

### Phase split

| # | Deliverable | Tests |
|---|---|---|
| 6.a | `imageCache` config field + validation (no behavior change yet) | Unit tests for config defaults + empty-bucket-disables-everything |
| 6.b | `y-cluster images push` against Hetzner S3 (operator side); minio-go dep added | Unit tests for OCI-layout-walking + S3 key construction; e2e `images-push` tag for one round-trip |
| 6.c | Provision-time pre-load when `imageCache.bucket` is set | e2e: push-then-provision-then-`crictl images` shows the cached set; no upstream pulls in the kubelet log |
| 6.d | `rejectUpstream` toggle (k3s registries.yaml) | e2e: a Pod referencing an uncached image stays `ImagePullBackOff` instead of pulling |

## Out of scope on this branch

- Real-cert path via cert-manager → upload. The hooks are in
  place (LB cert rotation + `--ssl-certificate` flag); the cert
  source stays self-signed for v1.
- Real-DNS plumbing for the LB IP (cloudflare / route53 / etc).
- A custom-Hetzner-image build pipeline (we use the upstream
  Ubuntu image and install k3s at first boot, same as qemu).
- The `pkg/provision/hetzner/` provisioner running ON Hetzner
  itself (i.e., a CI runner inside the project building these
  clusters). All Provision / Teardown calls run on the operator's
  laptop / workstation.
