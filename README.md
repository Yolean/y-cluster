# y-cluster

Single Go binary for Kubernetes cluster lifecycle on a developer
machine or a rented host, image management, and declarative
convergence. Replaces a stack of shell scripts that previously drove
ystack's local clusters.

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
| `glesys` | Talos Linux on a GleSYS KVM server | paid; the node and a kubeconfig context, nothing installed on top; `talosctl` for node access |
| `host` | k3s on a machine y-cluster does not create | the machine's own tooling applies `k3s bundle` as root; `kubeconfig import` and `platform install` do the rest; `provision` and `teardown` refuse it |

Subcommand groups:

- **provision / teardown** -- bring a k3s cluster up with Envoy
  Gateway and local-path storage installed, or remove it. `teardown`
  without `-c` lists the clusters this host knows about. qemu and
  docker clusters also get cert-manager with y-cluster's CA
  ClusterIssuer (`y-cluster-ca`); `certManager: {skip: true}` in
  `y-cluster-provision.yaml` leaves it out, as `gateway: {skip: true}`
  does Envoy Gateway.
- **k3s bundle / kubeconfig import / platform install** -- the
  three steps qemu provision makes after the VM is up, for a k3s host
  y-cluster did not create (`provider: host`, see below): the files a
  server node needs with a manifest of where they go, the node's
  `k3s.yaml` merged under a context, and Cilium, local-path storage,
  Envoy Gateway and cert-manager installed through that context.
  `platform install --check` reports drift without applying.
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
- **dockerhost provision / env / status / teardown** -- dockerd and
  buildkitd in one KVM guest per machine, for hosts that must not run
  a container daemon: `eval "$(y-cluster dockerhost env)"` points
  docker, Testcontainers and buildctl at it over TLS with client
  certificates. Every session of the user shares the daemon, its
  images and its build cache. The guest's tap device is a one-time
  root step y-cluster never takes. Advisories for its pinned dockerd,
  containerd and buildkitd are followed by the y-cluster maintainer,
  and new pins ship as y-cluster releases. See
  [DOCKERHOST.md](DOCKERHOST.md): the design, the root setup,
  patching, logging and exposure checks.
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
`scripts/qemu-tap-host-setup.sh yct0 192.168.122.1/24` creates the device;
`scripts/qemu-tap-host-setup.sh nft <public-if> 192.168.122.2 192.168.122.0/24`
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
  ifname: yct0                # pre-created, owned by the invoking user
  guestAddress: 192.168.122.2/24
  # gateway: 192.168.122.1        # default: first host address of the subnet
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

**docker: published ports listen on loopback too.** The docker
provider publishes `portForwards` on `bindAddress`, default
`127.0.0.1`, so the k3s API and the ingress on 80/443 are not reachable
from other networks the host is on. `0.0.0.0` publishes them on every
interface; a specific host address works too, and the kubeconfig then
names that address. Each cluster keeps its k3s state in volumes named
after it (`<name>-k3s`, `<name>-kubelet`, `<name>-cni`,
`<name>-varlog`), which `teardown` and the next `provision` of the same
name remove.

**A k3s host y-cluster did not create: three steps, each its own
verb.** `provider: host` describes a machine that other tooling
manages, with privileges y-cluster never gets (a sudo that asks for a
password, an API reachable only through an ssh tunnel). y-cluster
writes and installs, the operator's tooling does what needs root:

```yaml
provider: host
context: edge                     # required: the context the steps write and use
tlsSANs: [192.0.2.10]             # addresses besides 127.0.0.1 and the node's own
k3s:
  serverConfig:                   # merged over y-cluster's server settings
    secrets-encryption: true
    write-kubeconfig-mode: "0600"
gateway:
  dnsHintIP: 192.0.2.10           # where clients reach 80/443; published on the GatewayClass
```

```
y-cluster k3s bundle -c <dir> --out bundle/      # no root, no node contact
# as root on the host: install bundle/ per manifest.yaml (files with
# dest, mode and sha256; then install.sh with install.env; then
# postInstall)
ssh -N -L 16443:127.0.0.1:6443 <host> &
ssh <host> sudo cat /etc/rancher/k3s/k3s.yaml | \
  y-cluster kubeconfig import --context edge --server https://127.0.0.1:16443
y-cluster platform install -c <dir>              # again or --check: no change
```

The bundle's `config.yaml` holds every server option, so the k3s unit
runs plain `k3s server`; `config.yaml.d/` is left to the host's
tooling. `registries.yaml` is in the bundle only when `registries` is
configured (and `--without-registries` is not given); a host whose
tooling writes that file keeps it. qemu provision applies the same
bundle over ssh and calls the same platform install, and both verbs
also take a qemu config. `k3s.serverConfig` works for qemu too; keys
the platform depends on (`flannel-backend`, `disable-network-policy`,
`data-dir`, `write-kubeconfig`, `https-listen-port`) are refused.

**Envoy Gateway: y-cluster's pin is a default, and a cluster's config
can move past it.** A y-cluster release installs the Envoy Gateway
release it pins. `gateway.version` names another, with
`gateway.installSha256`, the sha256 of that release's `install.yaml`.
Both belong in the cluster's reviewed config: a patch release then
reaches the cluster through a reviewed change, with no new y-cluster.

```yaml
gateway:
  version: v1.9.2
  installSha256: 0412a72907e57ff9b73c56a7bf6df5190bf0f6e4f8bb4bba34e38630bbab5778
  # proxyImage: docker.io/envoyproxy/envoy:distroless-v1.39.5@sha256:9d84a586119a27b50437dbb01eccd4722832741539b1ed53c8e9d6da6067a6e0
```

```
curl -sL https://github.com/envoyproxy/gateway/releases/download/v1.9.2/install.yaml | sha256sum
```

The download is checked against the digest before it enters the cache,
and the cached file again before every use. A cached file that does not
match is an error that says to remove it, never a silent download.
y-cluster's own pin is checked the same way.

Before anything is applied, `platform install` (and provision) reads
the release the cluster runs, from the tag of the `envoy-gateway`
Deployment's image, and allows: an install where there is none, the
same release again (which changes nothing), or a newer patch of the
same minor. A newer minor of the same major is refused unless
`platform install --gateway-allow-minor` is given, and is best-effort
then: CRDs and defaults may change. That flag is the only way a running
cluster moves to a newer minor; provision has none, since it installs
on a new cluster. A downgrade or another major is refused. A refusal
names both releases and changes nothing, and `--check` reports it as a
failure, not as a diff. Nothing is pruned: an object that an older
release had and a newer one no longer ships stays on the cluster, which
is one reason minors are best-effort.

A config without `gateway.version` follows the pin: after a y-cluster
upgrade that bumps it, the next converge moves the cluster, by the same
rule. A pin that moves to a newer minor then fails every converge
without `--gateway-allow-minor` until someone decides. Pin
`gateway.version` in a production config, so that a y-cluster upgrade
does not move its gateway.

The image's repository must end in `envoyproxy/gateway` for its tag to
be read as the release. A mirror that keeps that path works
(`registry.example/envoyproxy/gateway:v1.9.2`); one that renames the
repository makes y-cluster refuse to apply, which is safe, but an
airgapped setup needs to keep the name.

`gateway.proxyImage`, `repository:tag@sha256:<digest>`, replaces the
envoy image that the release runs for the proxy, for an Envoy fix that
no Envoy Gateway release ships yet. It overrides the default of every
later Envoy Gateway release too, until it is removed. A patch release
is the ordinary path; `proxyImage` and minor upgrades are for when a
patch release is not enough.

## Specs

Design notes, migration recipes, and the still-pending feature
spec live in [`../specs/y-cluster/`](../specs/y-cluster). The
binary's behaviour is the source of truth for what's
implemented; the specs are kept for design rationale and
in-flight scope.

## Issues / feedback

[github.com/Yolean/y-cluster/issues](https://github.com/Yolean/y-cluster/issues)
