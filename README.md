# y-cluster

Single Go binary for Kubernetes cluster lifecycle on a developer
machine or a rented host, image management, and declarative
convergence. Replaces a stack of shell scripts that previously drove
ystack and checkit's local clusters.

## What it does

```
$ y-cluster --help        # full subcommand list
```

Providers, picked by `provider:` in `y-cluster-provision.yaml` (or
discovered when omitted: multipass, then qemu, then docker):

| provider | cluster node | where |
|---|---|---|
| `qemu` | k3s in a KVM VM | Linux with `/dev/kvm`; the only provider that exports appliances |
| `docker` | k3s in a container | anywhere a docker daemon runs; what CI uses |
| `multipass` | k3s in a Multipass VM | macOS and Linux |
| `hetzner` | k3s on a Hetzner Cloud server | paid; tears itself down in-cluster when `lifetime` expires |
| `glesys` | k3s on a GleSYS KVM server (hosting, not appliance) | config and schema only so far: a config validates, `provision` says there is no provisioner yet |

Subcommand groups:

- **provision / teardown** -- bring a k3s cluster up with Envoy
  Gateway and local-path storage installed, or remove it. `teardown`
  without `-c` lists the clusters this host knows about.
- **stop / start / pause / resume** -- keep the disk, drop the
  compute. `stop` works on every provider; the rest are qemu-only
  (`start` also handles hetzner).
- **prepare-export / export / import, manifests add / replace / rm**
  -- the appliance path: ship a qemu cluster's disk as qcow2, raw,
  vmdk, ova or a GCE image tarball, with its `/data/yolean` as a
  first-boot seed and with staged manifests that apply on the
  customer's first boot rather than on the build cluster. See
  [APPLIANCE_MAINTENANCE.md](APPLIANCE_MAINTENANCE.md).
- **yconverge** -- apply a kustomize base with ordering by CUE
  imports and post-apply checks. Symlink the binary as
  `kubectl-yconverge` to use it as a kubectl plugin.
- **detect / ctr / crictl** -- discover the local cluster's
  backend by kubeconfig context and run `ctr` or `crictl` on the
  node through the right transport (Docker daemon API for the
  docker provisioner, SSH for qemu). Replaces ystack's
  `y-cluster-local-{detect,ctr,crictl}`.
- **images list / cache / load / push / remote** -- extract image
  refs from a YAML stream, pull a single ref into a local OCI cache,
  or stream an OCI archive into the cluster node's containerd. The
  airgap path for both system images (handled inside provision)
  and arbitrary user-built images. `push` and `remote list / stats`
  maintain the object-storage image cache the hetzner provider
  pre-loads from.
- **gateway state / hostnames / clear-dns-hint-ip / example, echo
  deploy / render, localstorage render** -- inspect what the bundled
  Envoy Gateway has reconciled (`hostnames --csv` feeds TLS
  automation), install a small test workload behind it, or print
  the manifests y-cluster applies so they can be reviewed or applied
  by hand.
- **cache info / purge** -- inspect or wipe y-cluster's shared
  download cache (k3s airgap bundles, image OCI layouts).
- **lifetime status / reap / extend / arm / disarm / gcp-flags** --
  cost-control auto-expiry. A `lifetime.maxRun` in the config gives
  the cluster a wall-clock budget counted from when it starts; on
  expiry a local cluster runs its `onExpiry` action (stop by
  default) via a host timer, and a GCP appliance is deleted by GCP
  itself (`gcp-flags` emits the `--max-run-duration` flags). See the
  "lifetime" idea below.
Every subcommand has its own `--help` with the flags and
context. The README is intentionally short -- when something is
discoverable from `y-cluster <cmd> --help`, that's where it
lives.

## Three ideas worth knowing before you start

**lifetime: the budget is counted from start, and the trigger lives
where the cost is.** `lifetime.maxRun` is a wall-clock budget that
begins when the cluster *starts* (re-anchored on every `y-cluster
start`), not when it was provisioned -- an appliance disk may boot
days after it was built. Locally the host *is* the cost, so a host
timer fires `y-cluster lifetime reap`, which stops the cluster (or
the configured `onExpiry` action). On a GCP appliance the host
mustn't be the trigger (it may be offline), so `lifetime gcp-flags`
hands the duration to GCP's native `--max-run-duration`, and GCP
deletes the instance on its own -- the attached data disk survives.
`reap` re-checks the persisted deadline and re-arms if it isn't due,
so `lifetime extend 2h` is safe and a stale timer is harmless.

**yconverge: ordering vs checks come from different places.**
CUE imports in `yconverge.cue` declare ordering -- each import is
a *separate* yconverge invocation that runs its own apply and
checks before yours. Kustomize tree traversal collects checks
across the whole base, so an overlay's checks include the base's.
The two mechanisms are deliberately separate:
*ordering across modules* uses CUE; *checking after one apply*
uses traversal. `y-cluster yconverge --help` has the rule.

**qemu: two network modes, and only one of them shows workloads who
is calling.** `network.mode: user` (default) is qemu user-mode
networking: no host setup, no privilege, identical on a laptop and a
server. The guest has no address the host network can reach, and
everything (ssh, the k3s API, ingress on 80/443) goes through host
port forwards. Every connection reaches the guest from `10.0.2.2`, so
access logs, `X-Forwarded-For`, rate limits and allowlists in the
cluster never see a real client. `network.mode: tap` attaches the VM
to a tap device on a host-only subnet that the host routes: the guest
has its own address, `sshPort`/`portForwards` do not apply, and a
client address survives all the way to a backend behind the bundled
Envoy Gateway. Pick tap when the cluster serves real traffic (QA on a
rented host); stay on user everywhere else.

Tap mode needs the host prepared once, by root, and y-cluster never
does that itself: it attaches to the device it is given and, when
something is missing, prints the exact commands.
`scripts/qemu-tap-host-setup.sh ycl0 10.88.0.1/24` creates the device;
`scripts/qemu-tap-host-setup.sh nft <public-if> 10.88.0.2 10.88.0.0/24`
prints an nftables table (its own, no `flush ruleset`) that DNATs
80/443 to the guest and masquerades the guest's outbound traffic. DNAT
keeps the client address; never masquerade traffic going into the tap.
For persistence across reboots use systemd-networkd (`.netdev` +
`.network`), `/etc/sysctl.d` and `/etc/nftables.d`. Teardown leaves
the device alone. One tap device and one subnet per VM.

```yaml
provider: qemu
network:
  mode: tap
  ifname: ycl0                # pre-created, owned by the invoking user
  guestAddress: 10.88.0.2/24
  # gateway: 10.88.0.1        # default: first host address of the subnet
  # dns: [1.1.1.1, 9.9.9.9]   # default; the host is not assumed to run a resolver
```

The real client address depends on `externalTrafficPolicy: Local` on
Envoy Gateway's Service, which y-cluster sets: k3s ServiceLB then lets
kube-proxy deliver to the envoy pod without SNAT. Two consequences
worth knowing: a connection opened in the first seconds after the
Gateway comes up (before ServiceLB has published the node address)
goes through ServiceLB's own pod instead and shows that pod's
`10.42.x.y` address for as long as the connection lives; and a Service
you create yourself with the default `Cluster` policy always will.

**qemu user mode: port forwards listen on loopback unless you say
otherwise.** The forwards bind `network.bindAddress`, default
`127.0.0.1`. Set it to `0.0.0.0` to reach the cluster from other
machines, and know what that means: on a host with a public address
and no firewall it publishes the VM's sshd and the k3s API to the
internet. A specific host address works too; the kubeconfig then names
that address and k3s gets it as a TLS SAN. A cluster provisioned
before this option existed keeps its wildcard forwards across
`stop`/`start`; re-provision to move it to loopback.

## Specs

Design notes, migration recipes, and the still-pending feature
spec live in [`../specs/y-cluster/`](../specs/y-cluster). The
binary's behaviour is the source of truth for what's
implemented; the specs are kept for design rationale and
in-flight scope.

## Issues / feedback

[github.com/Yolean/y-cluster/issues](https://github.com/Yolean/y-cluster/issues)
