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
| `prepare-export` on hetzner | Returns `not supported on hetzner provider; use the qemu provisioner for disk-bound appliances` | Hetzner has no disk-upload API. The qemu→GCE/VirtualBox/Hetzner-as-target shape stays in qemu-land. |
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
