# Appliance maintenance

How y-cluster preserves customer data across appliance changes, and the
two mechanisms it provides for the appliance builder: **first-boot data
seeding** (handled at OS-level by a systemd unit) and **k3s manifests
staging** (build-time-staged manifests applied at customer-cluster
boot).

The doc is organised around the three phases an appliance goes through:
the customer's initial import, the supplier-side build of a new
appliance version, and the customer's later upgrade onto that new
version.

## Lifecycle overview

```
                ┌────────────────────────────────────────────────────┐
                │                                                    │
   build (run by us per customer)         customer (boots the disk)  │
                │                                                    │
   ┌────────────▼──────┐                ┌──────────────▼────────────┐│
   │ y-cluster         │                │ Customer attaches their   ││
   │   provision       │                │ persistent data drive at  ││
   │                   │                │ /data/yolean (per the     ││
   │ install workloads │                │ bundle README), boots.    ││
   │ (kubectl/yconverge)                │                           ││
   │                   │                │ ┌─────────────────────┐   ││
   │ /data/yolean now  │                │ │ y-cluster-data-seed │   ││
   │ holds DB schemas, │                │ │   detects empty     │   ││
   │ kafka topics,     │                │ │   external mount,   │   ││
   │ init markers...   │                │ │   extracts seed     │   ││
   │                   │                │ └─────────┬───────────┘   ││
   │ y-cluster         │                │           ▼               ││
   │   manifests add   │                │ ┌─────────────────────┐   ││
   │   <migration>     │                │ │ k3s starts          │   ││
   │ (stages a Job for │                │ │ auto-applies every  │   ││
   │ the customer's    │                │ │ manifest staged at  │   ││
   │ FIRST boot)       │                │ │ build time          │   ││
   │                   │                │ │ (Jobs are idempotent│   ││
   │ y-cluster stop    │                │ │  -- a Job named for │   ││
   │   prepare-export  │                │ │  v0.5.0 only runs   │   ││
   │   export          │                │ │  the v0.4.0->v0.5.0 │   ││
   │                   │                │ │  migration once)    │   ││
   │ -> tarball        │                │ └─────────────────────┘   ││
   └───────────────────┘                └───────────────────────────┘│
                                                                     │
                       Upgrade (new appliance disk, same customer):  │
                                                                     │
   ┌──────────────────────────────────┐                               │
   │ Customer boots v0.5.0 disk with  │                               │
   │ existing /data/yolean drive ───► │ y-cluster-data-seed sees      │
   │                                  │  marker, NO-OP                │
   │                                  │                               │
   │                                  │ k3s starts, auto-applies      │
   │                                  │  staged manifests; new        │
   │                                  │  Job names trigger their      │
   │                                  │  one-time migration logic.    │
   │                                  │  Already-applied Job names    │
   │                                  │  are no-ops.                  │
   └──────────────────────────────────┘                               │
                                                                     │
                                                                     ▼
```

The customer's lived experience: attach the appliance disk, attach the
data disk, boot. Subsequent upgrades = swap the appliance disk, keep
the data disk, boot. No commands. No state to migrate by hand.

## Phase 1: First import

The customer's first boot of a fresh appliance, ahead of the supplier
having shipped any subsequent upgrades.

### Supplier side

The supplier builds the v1 appliance disk:

1. `y-cluster provision -c <config>` to stand up a build-side cluster.
2. Install workloads (kubectl / yconverge / helm). The build cluster
   populates `/data/yolean/` with the build-time output of init Jobs:
   database schemas, Kafka topic configs, file-backed PVs, etc.
3. (Optional, normally none on a v1 build) `y-cluster manifests add`
   to stage any one-shot Jobs the customer's first boot should run.
4. `y-cluster stop` for a clean quiesce.
5. `y-cluster prepare-export` — virt-customize-driven identity reset
   (machine-id, ssh host keys, cloud-init clean), netplan generic-NIC
   match, systemd-timesyncd enable, **build the data seed** (see
   Mechanism 1 below), **move staged manifests** into k3s's
   auto-apply directory.
6. `y-cluster export <bundle-dir> --format=...` packs the result for
   the target hypervisor (qcow2 / raw / vmdk / ova / gcp-tar).

### Customer side

1. Format an ext4 volume with the universal LABEL the appliance
   expects:
   ```sh
   sudo mkfs.ext4 -L y-cluster-data /dev/<device>
   ```
   Attach it to the imported VM. `prepare-export` pre-baked
   `LABEL=y-cluster-data /data/yolean ext4 defaults,nofail 0 2`
   into `/etc/fstab`, so first boot mounts the volume automatically;
   the customer does not edit fstab themselves. Cross-hypervisor:
   VMware / VirtualBox / Hetzner / GCP all expose ext4 labels the
   same way.
2. Boot the appliance disk.
3. `y-cluster-data-seed.service` runs Before=k3s.service,
   After=cloud-init.service, sees the external mount is empty,
   **extracts the seed** into the customer's drive. Writes
   `/data/yolean/.y-cluster-seeded` last (so a crashed extract is
   detectable on next boot).
4. k3s starts, scans `/var/lib/rancher/k3s/server/manifests/`, applies
   any staged manifests. (For a v1 build there typically aren't any.)
5. Workloads come up against the now-populated `/data/yolean`.

### What if the customer forgets to attach the volume?

`fstab` carries `nofail`, so boot continues. `data_seed_check.sh`
sees `/data/yolean` is not a mountpoint, fails the unit, and k3s
stays down. sshd is unaffected (no transitive dependency), so the
customer SSHes in and reads:

```sh
sudo journalctl -u y-cluster-data-seed.service -b
```

The journal carries the actionable resolution recipe (attach + label
the volume, reboot, or mount + restart the unit).

### Hosting-automation bypass (NOT for customers)

Hosting automation can override the mount-required gate by writing
`/run/y-cluster-seed-bypass` (tmpfs) before the seed unit runs. The
canonical path is via the appliance's user_data, which cloud-init
delivers via the NoCloud datasource (Hetzner Cloud, multipass, our
qemu provisioner, etc):

```yaml
#cloud-config
write_files:
  - path: /run/y-cluster-seed-bypass
    permissions: '0644'
    content: ""
```

When the bypass flag is present, the seed extracts into whatever
`/data/yolean` is (typically the boot disk's directory if the fstab
mount soft-failed). A sibling `/data/yolean/.y-cluster-seeded-via-bypass`
sentinel records that the bypass path was taken — the in-memory flag
itself is gone after the next reboot, but the marker on disk still
controls the seed unit's no-op decision.

The customer never sets this. `/run` is tmpfs (no on-disk persistence),
and the only entity with cloud-init injection access is the entity
that creates the VM. Bare-metal / VMware / VirtualBox imports have
no cloud-init datasource at all by default, so the branch is
unreachable.

## Phase 2: Upgrade (supplier side)

How the supplier builds v(N+1), assuming customers exist on v(N).

The build flow is the SAME provision → install → manifests add →
stop → prepare-export → export sequence as Phase 1. What's
different:

- The supplier runs the v(N) testdata against the v(N+1) workload set,
  exercising whatever schema/topic/initContainer changes need to be
  smooth-migrated.
- Migration Jobs go in via `y-cluster manifests add migrate-vN-vN1-... <path>`.
  These accumulate across versions: a v0.6.0 build can stage *both*
  the v0.4→v0.5 and the v0.5→v0.6 migration Jobs by name; k3s on the
  customer side applies whichever ones haven't already run.
- The data seed re-built on this build represents v(N+1)'s baseline.
  Customers with existing data ignore it (marker present → no-op);
  fresh customers (a NEW customer importing for the first time on
  v(N+1)) get v(N+1)'s baseline directly.

### Migration Job authoring contract

A migration Job is the supplier's vehicle for changing customer data
from v(N) to v(N+1). Shape:

- `metadata.name` is the source-target version pair, e.g.
  `migrate-v0.5.0-userdb-add-tenants`. K3s's apply-on-restart
  semantics give one-time execution per name (already-Completed Jobs
  with that name are not recreated).
- Pre-gated by an InitContainer that waits for the workloads it depends
  on (`kubectl wait deployment/mariadb --for=condition=Available`).
- Idempotent in its own logic: the migration script checks for a marker
  (a ConfigMap, or a file under `/data/yolean/.migrations/`) before
  doing anything destructive.
- Optional: the Job's pod mounts `/data/yolean` directly (via a PVC
  bound to a host-path, OR via a `hostPath` volume) when the
  migration needs raw filesystem access AND the workloads are still
  down.

Skeleton:

```yaml
apiVersion: batch/v1
kind: Job
metadata:
  name: migrate-v0.5.0-userdb-add-tenants
  namespace: customer-app
spec:
  ttlSecondsAfterFinished: 3600
  template:
    spec:
      restartPolicy: OnFailure
      initContainers:
        - name: wait-mariadb
          image: bitnami/kubectl
          command: ["kubectl","wait","deployment/mariadb",
                    "--for=condition=Available","--timeout=5m",
                    "--namespace=customer-app"]
      containers:
        - name: migrate
          image: <appliance-builder-supplied-migration-image>
          env:
            - name: FROM_VERSION
              value: v0.4.0
            - name: TO_VERSION
              value: v0.5.0
          command: ["/migrate.sh"]
```

## Phase 3: Upgrade

The customer swaps the appliance disk while keeping the data drive.

### Customer side

1. Power the appliance off (`shutdown`, ideally graceful — see drain
   note in "Open considerations" if Galera-class StatefulSets are in
   the stack).
2. Detach the v(N) appliance disk; attach the v(N+1) appliance disk.
   The data drive at `/data/yolean` stays put.
3. Boot.
4. `y-cluster-data-seed.service` sees the marker on `/data/yolean`,
   no-ops (the existing data is what we want preserved).
5. k3s starts, reads `/var/lib/rancher/k3s/server/manifests/`. New
   migration Job names trigger; existing names are no-ops.
6. Workloads come up against the customer's preserved `/data/yolean`,
   migration Jobs apply their changes.

The customer issues no commands.

### Rollback

If a v(N+1) migration fails, the customer reattaches the v(N) disk
with the same data drive. The seed mechanism's marker-respect logic
means the data drive is untouched on either appliance. Workloads
resume against whatever state the migration left behind — which means
a partial / broken migration is on the supplier to design defensively
(idempotent + marker-gated; see Migration Job contract above).

A more explicit rollback marker pattern is on the open list.

## Mechanism 1: data-dir seeding (`y-cluster-data-seed.service`)

### Problem

The build cluster populates `/data/yolean/` with the build-time output
of init Jobs (database schemas, Kafka topic configs, file-backed PVs
from echo / VersityGW / customer workloads). The appliance disk ships
with that data on its boot filesystem.

When the customer boots and mounts THEIR persistent data drive at
`/data/yolean`, the mount obscures the boot-disk's `/data/yolean`. The
customer's drive starts empty. Workloads find nothing; init Jobs that
were "already done" on the build side haven't run on the customer
side. Setup is lost.

### Solution

`prepare-export` snapshots `/data/yolean/` into
`/var/lib/y-cluster/data-seed.tar.zst` (which lives on the appliance
disk's root, NOT under `/data/yolean`, so it's not obscured by the
customer's mount). It also pre-bakes
`LABEL=y-cluster-data /data/yolean ext4 defaults,nofail 0 2` into
`/etc/fstab` so the customer's only attach step is `mkfs.ext4 -L
y-cluster-data /dev/...` — no fstab edit, no hypervisor-specific
device path.

At boot, a oneshot systemd unit runs Before=k3s.service,
After=cloud-init.service, with the following decision (in order):

1. **`/run/y-cluster-seed-bypass` exists** → bypass branch: extract
   regardless of mount state, drop a sibling `.y-cluster-seeded-via-bypass`
   sentinel. Hosting-automation only; customers never get here.
2. **`/data/yolean` is NOT a mountpoint** → fail the unit. k3s stays
   down. The customer is meant to attach a labeled volume; the
   journal explains how. Eliminates the customer-mounts-after-k3s
   race (the original GCP-appliance failure mode).
3. **Marker `/data/yolean/.y-cluster-seeded` present** → no-op
   (respect existing state, upgrade fast path).
4. **Mountpoint empty (excluding `lost+found`)** → extract the seed,
   then write the marker.
5. **Mountpoint non-empty, no marker** → REFUSE TO SEED. Fail the
   unit loudly. k3s does not start.

sshd has no dependency on this unit and starts normally regardless
of seed outcome — the customer can always SSH in to recover.

### Empty defined

A directory is "empty" iff it has no entries other than `lost+found`
(the kernel creates this on every freshly-formatted ext4). Anything
else is a conflict — we won't clobber data the customer didn't tell us
about.

### Marker

`/data/yolean/.y-cluster-seeded` is JSON:

```json
{
  "schemaVersion": 1,
  "seeded_at": "2026-05-04T12:30:00Z",
  "seeded_by": "y-cluster v0.4.0 (abc1234)",
  "appliance_name": "appliance-gcp-build",
  "seed_sha256": "sha256:c7e3...8a2f"
}
```

`seed_sha256` is the digest of `/var/lib/y-cluster/data-seed.tar.zst`
that was the source of this seed. Future appliance versions can
compare the customer's marker against the new seed's sha to detect
whether the data they're upgrading has the same baseline as the new
appliance was built against (decision input for migration jobs; see
Mechanism 2).

### Never overwrites actual data

Four layers, in order:

1. **Marker check first.** Marker present → unconditional no-op. The
   upgrade fast path.
2. **Conflict detection.** No marker + non-empty dir → fail unit, log
   conflict-resolution recipes (see Troubleshooting).
3. **k3s blocks on the unit.** A drop-in adds
   `Requires=y-cluster-data-seed.service` to k3s.service. If seed
   fails, k3s won't start — the customer SSHes in and fixes the
   situation instead of getting silent partial state.
4. **Marker is written LAST.** A crashed extract leaves no marker; the
   next boot detects "non-empty without marker" → conflict mode. The
   customer sees something's wrong instead of getting silent
   half-seeded state.

### Troubleshooting

The customer-side troubleshooting surface, from least to most
intrusive:

```sh
# What did seed do on the most recent boot?
sudo journalctl -u y-cluster-data-seed.service -b

# Has the volume been seeded? When? By what?
cat /data/yolean/.y-cluster-seeded

# Standalone status helper -- prints marker + seed + k3s state.
sudo /usr/local/bin/y-cluster-seed-status

# Conflict mode: the unit's stderr lists the conflicting entries
# and the two recovery recipes:
#   - if data is correct: write the marker manually
#       echo '{"schemaVersion":1,...}' | sudo tee /data/yolean/.y-cluster-seeded
#       sudo systemctl restart y-cluster-data-seed.service
#   - if data is junk: wipe and re-seed
#       sudo rm -rf /data/yolean/* /data/yolean/.[!.]*
#       sudo systemctl restart y-cluster-data-seed.service
```

### Trade-off

The seed tar adds `du -sh /data/yolean` (compressed via zstd) to the
appliance disk shipped to the customer. For a heavy build (kafka +
mariadb + keycloak + customer workloads with init data) that's
typically 1-3 GB on top of the boot disk. Mitigation if it becomes
painful: selective seeding of ONLY init markers and small config files
(workload entrypoints' detect-and-init logic re-creates the bulk on
first boot).

## Mechanism 2: manifests staging (`y-cluster manifests add`)

### Problem

The appliance builder needs to ship Kubernetes manifests (typically
migration `Job`s, but also `ConfigMap`s, `Secret`s, etc.) that should
apply to the customer's cluster on its first boot — NOT on the build
cluster.

Naive: `kubectl apply` during build → applies to the build cluster
immediately. Init Jobs run, write to /data/yolean. Migration Jobs that
expect "v0.4.0 schema" fail because the build cluster is freshly
initialized at v0.5.0 schema. Wrong cluster, wrong state.

### Solution

`y-cluster manifests add <name> <path|->` writes the manifest into a
staging directory on the cluster node:

```
/var/lib/y-cluster/manifests-staging/<name>.yaml
```

This directory is NOT auto-applied by k3s. The build cluster doesn't
react. `prepare_inguest.sh` (run during prepare-export) moves the
staged manifests into k3s's auto-apply directory:

```
/var/lib/rancher/k3s/server/manifests/<name>.yaml
```

On the customer's first boot, k3s scans `manifests/` and applies
everything. Subsequent boots re-apply (server-side apply is idempotent
for non-Job resources, and for Jobs k3s observes the existing
Completed state and doesn't recreate the pod).

### `<name>` semantics

The name is the file basename (without `.yaml`). It MUST:
- Match `[a-zA-Z0-9][a-zA-Z0-9._-]*` (no path separators, no `..`).
- Not already exist in the staging directory (the subcommand bails).

The name is also the source-of-truth identifier for the migration. We
recommend a versioned shape like `migrate-v0.5.0-userdb-add-tenants`.
An identical name in a future appliance build → idempotent re-apply
(no-op). A different name → new migration runs once.

### Trade-off

The customer's first boot of a new appliance applies EVERY staged
manifest at once. If a migration Job has a long pre-gate
(`kubectl wait` for slow-starting workloads), that delays the cluster
becoming "ready" for traffic. Mitigation: scope migration Jobs to
non-blocking work where possible; keep the appliance's own startup
fast and leave heavy data migrations to a workload-side scheduled
process.

## What y-cluster owns vs. what the appliance builder owns

| | y-cluster | appliance builder |
|---|---|---|
| Build the data seed                        | ✓ | |
| Boot-time seed extraction                  | ✓ | |
| Marker semantics, conflict detection       | ✓ | |
| Stage manifests on the cluster filesystem  | ✓ | |
| Move staged manifests on prepare-export    | ✓ | |
| Decide WHAT to migrate                     |   | ✓ |
| Author the migration Job manifest          |   | ✓ |
| Ship the migration container image         |   | ✓ |
| Choose the migration name (= idempotency key) |   | ✓ |

y-cluster does not invent a new migration framework. The Kubernetes
Job resource + idempotent name + InitContainer wait pattern is the
contract; y-cluster's job is just to make it possible to ship those
Jobs in an appliance disk that's been built ON ONE CLUSTER but
TARGETED AT ANOTHER.

## Open considerations (not blocking the first cut)

- **Selective seeding** — a flag on `prepare-export` to seed only
  specific subdirs of `/data/yolean` (e.g., init markers and
  config-only files). Lets workloads re-do bulk init on first boot to
  shrink the appliance.
- **Migration ordering across multiple Jobs** — if multiple staged
  Jobs need to run in order, the appliance builder uses K8s-native
  dependency primitives (a wait-for-completion Init container on the
  second Job). y-cluster doesn't try to model an ordering DAG itself.
- **Customer-side rollback marker** — today rollback = swap back to
  the prior appliance disk. A more explicit "rollback marker" pattern
  would let the supplier signal "this migration is reversible by
  Job-X" and the customer trigger that without disk swap.
