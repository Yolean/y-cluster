# dockerhost: a container daemon in a KVM guest

`y-cluster dockerhost` runs dockerd and buildkitd in a KVM guest and tells clients how to reach
them, so that builds and Testcontainers suites run unchanged on the host as clients.

It is for machines that must not run a container daemon on the host: hardened hosts, where
the daemon would be root-equivalent for the bot user, and the appliance QA machine (checkit
#7986: "we don't run docker directly on that machine"). Those hosts have KVM.

Status: implemented. The guest, its daemons, TLS, the lifecycle and the idle reaper are tested
on a real KVM guest behind the test harness's loopback forwards (Testing). The product path,
a guest on a tap device that root prepared, had its first run on gle01 on 2026-10-07, with the
CLI and the exposure checks by hand (Exposure checks); `TestDockerhost_Tap` has not run yet.

## Using it

Once per machine, root prepares a tap device (One-time root setup), and the user who runs the
guest names it in `~/.config/y-cluster/dockerhost.yaml`:

```yaml
network:
  ifname: ycl1                # tap owned by this user; may be a port of a bridge
  guestAddress: 10.88.1.2/24  # static; clients reach the daemons here
  # gateway: 10.88.1.1        # default: first host address of the subnet
  # dns: [1.1.1.1, 9.9.9.9]   # default; must be reachable through the host's NAT
# memory: 6144                # MB
# cpus: 4
# diskSize: 60G               # sparse qcow2: the image store and build caches
# idleTimeout: 8h             # the guest powers itself off after this long unused; "0" = never
# maxAge: 336h                # the guest is recreated from the newest image after this (at most 1440h)
```

The network has no default: which device and subnet are the guest's is the host's decision.
The values above are gle01's.

Then, in any session:

```sh
y-cluster dockerhost provision        # idempotent; cheap when the guest is healthy
eval "$(y-cluster dockerhost env)"
docker run ...; mvn verify ...       # docker and Testcontainers
y-cluster buildctl build ...          # BuildKit, finds the guest without the env
y-cluster dockerhost status           # exits non-zero unless the guest runs and both daemons answer
y-cluster dockerhost teardown         # guest, disk, certificates and state
```

State lives in `~/.cache/y-cluster-dockerhost` (0700); `$Y_CLUSTER_DOCKERHOST_DIR` overrides it,
which y-cluster's tests use. `y-cluster teardown` without `-c` lists the dockerhost with its own
teardown command.

### Sharing

There is one dockerhost per machine, run by the user who owns its tap device, as host Docker has
always been one daemon. Every session of that user shares the daemon, its images, its containers
and its build cache, and the one client certificate. The certificate is root in the guest for every session of the same uid;
that is accepted with the one-daemon decision. Another user on the machine has no certificate,
and both daemon ports refuse a client without one.

### Secrets

No secrets are baked into images or build contexts; build secrets go through BuildKit's secret
mounts only. y-cluster mounts no host directory into the guest and copies nothing into it but
the files it generates (the guest definition below). Nothing from a credentials directory such as
the bots device directory (`~/Yolean/.yolean-bots-device`) is ever mounted or copied into the
guest, by y-cluster or by a build: build contexts are what a client sends, and the client
decides what that is.

## End goals

1. `y-cluster dockerhost provision` brings up one guest with dockerd and buildkitd; `teardown`
   removes it. Provision is idempotent: a healthy guest is reused.
2. `y-cluster dockerhost env` prints the environment a client needs, for whatever daemon this
   machine has. Clients do `eval "$(y-cluster dockerhost env)"`. Docker is one of the outputs,
   not the abstraction.
3. A machine with plain Docker (Linux dockerd, Docker Desktop) behaves exactly as today: `env`
   prints nothing, no repo depends on a dockerhost, no test or build gets slower.
4. Builds and tests that already follow the portability rules below need no change to run
   against the guest.

## Decisions

| Decision | Choice | Why |
|---|---|---|
| Instances | At most one dockerhost per machine; no name option | As host Docker has always been: one daemon shared by all sessions. Random mapped ports never collide on one daemon; image and build caches stay warm. (Staffan, 2026-10-04) |
| Discovery | Environment variables, printed by `dockerhost env` | The only mechanism every client honours. |
| Docker contexts | Not used | Testcontainers Java 2.0.5, Go v0.42.0 and Node 11.13.0 all ignore them and silently use `/var/run/docker.sock`, while the docker CLI follows the context. A context pointing at the guest would split CLI and tests between two daemons. |
| Daemon access | tcp + TLS client certificates | A plain TCP port is open to every local user, and the Docker API is root in the guest. tcp+TLS needs only `DOCKER_HOST DOCKER_TLS_VERIFY DOCKER_CERT_PATH`, which checkit's turbo.json already passes through. |
| Published ports | Reached at the guest's own address, found through `DOCKER_HOST` | Testcontainers derives `getHost()` from a tcp `DOCKER_HOST`. No 127.0.0.1 assumption, no port forwarding of published ports. |
| Network | A tap device root prepared, by name and subnet | y-cluster never creates network devices, never runs as root and never changes the host's firewall (compliance review D-a, D-g). |
| buildkitd | Rootful in the guest | Guest root is not host root, as for dockerd; the rootless variant buys nothing inside a disposable guest and depends on the guest kernel's unprivileged user namespaces. |
| Idle reaper | In the guest | Needs no host scheduler: hardened hosts have no systemd user manager for the user (no linger) and no `at`. |
| In-cluster BuildKit | ystack's rootless buildkitd stays as it is | dockerhost is for builds outside a cluster. |
| Testcontainers Cloud, Docker Build Cloud | Out | New supplier, outside the EU by default (compliance recommendation). |

## The environment contract

`y-cluster dockerhost env` prints, when the guest runs:

```
export DOCKER_HOST='tcp://<guest address>:2376'
export DOCKER_TLS_VERIFY='1'
export DOCKER_CERT_PATH='<client dir>'
export BUILDKIT_HOST='tcp://<guest address>:8547'
export BUILDKIT_TLS_DIR='<client dir>'
```

- `DOCKER_CERT_PATH` and `BUILDKIT_TLS_DIR` are the same directory, `~/.cache/y-cluster-dockerhost/client`:
  `ca.pem`, `cert.pem`, `key.pem`, the directory 0700 and the files 0600.
- 2376 is Docker's port for TLS. 8547 is the port of ystack's in-cluster buildkitd.
- `BUILDKIT_TLS_DIR` is read by `y-cluster buildctl`, and by ystack's `y-buildctl` (branch
  `YoleanAgents/ystack:y-build-tls`, PR to Yolean/ystack pending); both pass it as
  `buildctl --tlsdir`, since buildctl takes TLS files only as flags.
- **`y-cluster buildctl`** is BuildKit's buildctl compiled into y-cluster, at the `BuildKitVersion`
  the guest runs (`pkg/dockerhost/versions.go`), so the client never drifts from the server and
  nothing is downloaded. Arguments pass through untouched. Without `--addr` or `BUILDKIT_HOST` it
  dials this machine's guest, and when the guest does not run (after the idle poweroff, say) it
  fails with the provision command instead of buildctl's connection error.
  `scripts/buildctl-refresh.sh` copies only buildctl's own source (`cmd/buildctl`) into
  `pkg/buildctl` after a `BuildKitVersion` bump; the rest of BuildKit is the module at that version.
- **Builds go through BuildKit, not `docker build`.** The docker client y-cluster expects has no
  buildx plugin, and without it `docker build` falls back to the legacy builder, which rejects
  `RUN --mount` (mirror-v3's Dockerfile, for one). Repos build with `y-cluster buildctl build
  --frontend dockerfile.v0 ...` (or y-build) and adapt where they assumed `docker build`.
- No `TESTCONTAINERS_*` variables are needed. `TESTCONTAINERS_RYUK_DISABLED` stays unset: Ryuk works.
- **Without a running guest, `env` prints nothing** (open question 5, decided). A machine with
  plain Docker, or with a `DOCKER_HOST` someone set on purpose (cicd-v1's `tcp://dockerd:2375`),
  keeps what it has; `unset` lines would take that away from every shell that runs `env`. The one
  exception: a shell that still carries this dockerhost's own variables (`DOCKER_CERT_PATH` or
  `BUILDKIT_TLS_DIR` is this machine's client directory) gets
  `unset DOCKER_HOST DOCKER_TLS_VERIFY DOCKER_CERT_PATH BUILDKIT_HOST BUILDKIT_TLS_DIR`,
  so after a teardown or an idle poweroff it falls back to the local socket instead of dialing a
  guest that is gone. A guest still in its first boot is not announced.
- `env` reads files only: fast, and safe in a shell profile.

Rejected forms, with the reason:

| form | problem |
|---|---|
| `DOCKER_HOST=unix://<ssh-forwarded socket>` | Needs `TESTCONTAINERS_HOST_OVERRIDE=<guest address>` and `TESTCONTAINERS_DOCKER_SOCKET_OVERRIDE=/var/run/docker.sock` (Ryuk otherwise bind-mounts the client-side socket path, which does not exist in the guest, and fails to start in all three languages). Neither is in turbo's passthrough. |
| plain `tcp://...:2375` | Unauthenticated root API on a port every local user can reach. |
| published ports forwarded to the host's 127.0.0.1 | Hides the address problem instead of fixing it; ports from one daemon could collide with host services; needs a forwarder process. |

## The guest

Made by the qemu provider (`pkg/provision/qemu`, `ProvisionGuest`): the same disk, cloud-init
NoCloud seed (`cloud-localds`), tap attachment and preflight, process handling and teardown as a
cluster node, without k3s and without a kubeconfig context. The guest definition is
`pkg/dockerhost/guest/` and `pkg/dockerhost/guest.go`.

| aspect | built |
|---|---|
| image | Ubuntu 24.04 (noble) cloud image, `noble/current`. Every new guest starts from the newest one: provision fetches `SHA256SUMS` and `SHA256SUMS.gpg`, verifies the signature against Ubuntu's cloud image keys embedded in y-cluster (from the `ubuntu-keyring` package, fingerprints pinned in a test), and downloads the image only into a file named by the signed digest, checking the digest as it writes. Booted with `-machine accel=kvm` as the unprivileged user (group `kvm`). |
| network | A tap device by name, with a static address on its subnet (`network-config` in the seed); the tap may be a port of a bridge that carries the gateway address. The guest reaches the internet through the host's NAT and resolves through public resolvers (default 1.1.1.1 and 9.9.9.9, the same as gle01's cluster guest on `ycl0`; the host's own resolver is unreachable from the guest by design). |
| dockerd | `docker-ce` 29.8.2 and `containerd.io` 2.3.6 from Docker's apt repository, whose signature apt checks with Docker's key (written by cloud-init, fingerprint `9DC8 5822 9FC7 DD38 854A E2D8 8D81 803C 0EBF CD88` pinned in a test). The packages are held. Rootful in the guest; `-H fd:// -H tcp://0.0.0.0:2376` with `tls`, `tlsverify` and the CA in `daemon.json`. The unix socket stays root's, inside the guest. Logs rotate (json-file, 3 × 20 MB per container). |
| buildkitd | BuildKit v0.33.1 from the GitHub release, unpacked only after its SHA-256 matched the pin. Rootful in the guest, OCI worker (runc), garbage collection on; `tcp://0.0.0.0:8547` with the server certificate and client verification against the same CA. RUN steps use the guest's network namespace (BuildKit's default without CNI). |
| BuildKit in dockerd | Serves `docker build`/`buildx` and `docker compose build` with the client's buildx. |
| certificates | Issued by provision for each new guest: a CA, a server certificate with the address clients dial as its only SAN, one client certificate; ECDSA P-256, valid 90 days. **The CA's private key is never written anywhere** -- not to the guest, not to the host; it signs the two certificates in memory and is dropped. Re-issuing means recreating the guest, which issues everything under a new CA. The server key reaches the guest in the NoCloud seed; the seed and the rendered user-data are 0600 in the 0700 state directory and are deleted once the first boot is over. The client key never enters the guest (tested). |
| disk | The guest's own qcow2, backed by the verified image, 60G sparse by default: the image store and the build caches. No host directory is shared. BuildKit garbage-collects its cache; the recreate cycle bounds the rest. |
| ssh | cloud-init's `ystack` user with a key generated per guest (in the state directory), for provision's checks and a graceful poweroff; `status` prints the command. |
| registry | The guest cannot reach `builds-registry.ystack.svc.cluster.local`; y-build needs `IMPORT_CACHE=false EXPORT_CACHE=false` (skaffold configs already set them). |

Guest-to-host connections are not needed by anything verified: Testcontainers' host access
(`HostAccessPorts`) dials its sshd sidecar from the test process and tunnels back over that
connection.

### Lifecycle

`provision` holds a lock (`flock` on the state directory's `lock`) for its whole run, so
concurrent sessions wait for one boot instead of starting two. For an existing guest it decides:

| the guest | provision |
|---|---|
| runs and both daemons answer over TLS | reuses it (about 0.3 s) and renews its lease |
| was powered off by its idle reaper | boots it again from its disk (about 25 s); images and build cache are still there |
| is older than `maxAge` | replaces it with a new one from the newest signed image once nobody uses it; a day past `maxAge` in any case |
| runs daemon versions this y-cluster no longer pins | replaces it once nobody uses it |
| was made with another network or size, does not answer, or never finished its first boot | replaces it |

A new guest takes about a minute and a half (measured; see Testing). Replacing a guest discards
its images and build cache and issues new certificates under a new CA. A failed provision tears
the half-made guest down and keeps its console log as `last-failure-console.log`.

Health is what a client sees: `GET /_ping` on dockerd and `Control.ListWorkers` on buildkitd
(gRPC over HTTP/2, at least one worker), both over TLS with the client certificate.

The host inventory (`~/.cache/y-cluster-clusters`) records the guest, so `y-cluster teardown`
without `-c` lists it with `y-cluster dockerhost teardown`.

### Idle reaper

The guest powers itself off when nobody uses it: a systemd timer in the guest checks every minute
(from five minutes after boot) and runs `systemctl poweroff` when the guest has been idle for
`idleTimeout` (default 8h), or when it is older than `maxAge` and idle right now. qemu then exits
and frees the memory; the disk stays for the next provision.

- In use means an established TCP connection in the guest's own network namespace other than
  ssh and loopback: a docker or buildctl client, a client of a published port (Testcontainers'
  Ryuk connection lasts the whole test session), a pull or push under way.
- Running containers alone do not count, so a forgotten `docker run -d` does not keep the guest
  up forever.
- Every `provision` renews the lease (over ssh), so a session that provisions before each run keeps
  the guest.
- A service stack left running in containers (a Prometheus, an S3 stand-in) is not use either, so
  it stops with the guest after `idleTimeout`. Raise `idleTimeout` in the configuration for such
  use: the next `provision` applies it with the lease, without recreating the guest.
- The reason goes to the guest's console, which the host keeps in the VM's console log.

## Patching (compliance review D-c, HST-20..23)

- **The guest is recreated on a cycle**, from the newest signed cloud image: `maxAge`, default 14
  days, at most 60, enforced by provision (as soon as the guest is idle, and a day later in any
  case) and by the guest's idle reaper (it powers an over-age guest off when idle, and the next
  provision recreates it).
- **dockerd, containerd and buildkitd are pinned** in `pkg/dockerhost/versions.go` and change only
  through y-cluster releases. The packages are held in the guest, so unattended-upgrades never
  moves them; a guest whose versions a newer y-cluster no longer pins is replaced at its next idle
  provision.
- **Inside the guest, unattended-upgrades stays on** (the cloud image's default: daily, Ubuntu's
  security origins), for the Ubuntu packages around the pinned ones. It does not reboot the guest;
  a new kernel arrives with the recreate cycle.
- **Who follows advisories for the pinned versions (HST-23): the y-cluster maintainer** -- the
  owner of the y-cluster repository, Staffan Olsson -- for Docker Engine (moby), containerd and
  BuildKit (their GitHub security advisories and release notes), with a y-cluster release that
  bumps the pins as the response. Ubuntu's own advisories are covered by unattended-upgrades in
  the guest and by the recreate cycle.

## Logging (compliance review D-d, HST-39, OPS-LOG-STD-001 R11)

The guest's journal is not shipped: the host's pipeline does not exist yet on gle01. The
dockerhost is to be listed in OPS-BMH-001 as an exception, with the reason: a development guest,
no customer data, recreated at least every 14 days. The guest keeps its own journal (persistent,
until the guest is recreated); the host keeps the guest's console log next to its disk until
teardown.

## Exposure checks (compliance review D-e)

To run before first use on a machine, and the results kept. `TestDockerhost_Tap` (Testing) runs
them on the prepared tap and logs each result:

- `ss -tulpn` on the host before and after provision: no new listener on `0.0.0.0` or `[::]`, and
  qemu on a tap listens on nothing;
- from a container in the guest: the host at its tap address and at its public address, the
  private ranges (10/8, 172.16/12, 192.168/16, 100.64/10) and the metadata address
  169.254.169.254 are unreachable, while the internet is reachable (the control that makes the
  refusals mean something); a published port is reachable from the host at the guest's address;
- both TLS ports refuse a client without a certificate, a certificate from another CA, and plain
  HTTP.

Other guests on the host (the cluster guest at 10.88.0.2 on gle01) are not probed: the private
ranges check covers their subnet, and a check that reached them would touch them.

Status: run by hand on gle01 on 2026-10-07/08 against a guest made by `y-cluster dockerhost
provision` on `ycl1` (4 vCPU, 6 GB, 40G), all as above: the guest's qemu owns no host socket; a
container reaches 1.1.1.1:443 and 9.9.9.9:53 but not the host at 10.88.1.1 or its public address,
the `ycl0` guest, the four private ranges or 169.254.169.254; a published port answers at
10.88.1.2; both TLS ports refuse plain HTTP, a client without a certificate and one from another
CA. `TestDockerhost_Tap` itself has not run on the tap yet. The same TLS and refusal checks, and the
loopback-only host listeners, pass in the test harness (Testing).

## One-time root setup (compliance review D-a, D-g)

y-cluster never does this: it consumes the tap by name, never runs as root and never changes
the host's firewall or sysctls. A human runs it once per machine -- on a hardened host, from
outside the host's own sessions (bots `hardened-host`), since the hardening of a host is not
changed from inside it.

The guest network must keep:

- no forward from the public interface to the dockerhost guest (no DNAT, no published ports);
- the guest denied private ranges and the metadata address on egress, as for the cluster guest;
- no reach from the dockerhost guest to other guests, or from them to it;
- no new connections from the guest to the host (replies only), so the host's own services,
  its resolver included, are not reachable from the guest. The host connects to the guest.

### On a hardened host (gle01, bots `hardened-host`)

The `guest_network` role gives one guest (`ycl0`) a tap and `table inet guestnat`; the dockerhost
is a second guest, a development guest that OPS-BMH-001 #guest-network has to name. Until the
role takes a second guest, root adds it by hand (on gle01: `ycl1`, gateway `10.88.1.1/24`, guest
`10.88.1.2`):

`/etc/systemd/network/61-ycl1.netdev` (any name)

```ini
[NetDev]
Name=ycl1
Kind=tap

[Tap]
User=gle-qa
Group=gle-qa
```

`/etc/systemd/network/61-ycl1.network`

```ini
[Match]
Name=ycl1

[Network]
Address=10.88.1.1/24
ConfigureWithoutCarrier=yes
LinkLocalAddressing=no
IPv6AcceptRA=no

[Link]
RequiredForOnline=no
```

and these lines in `/etc/hardened-host/guestnat.nft`, in the existing chains of
`table inet guestnat` -- not in a table of their own: guestnat's forward chain has policy drop and
drops every forwarded packet it does not name, so a second table could accept nothing. This is
what was applied on gle01 on 2026-10-05 (public interface `bond0`):

```
  chain postrouting {   # add
    ip saddr 10.88.1.0/24 oifname "<public-if>" masquerade
  }
  chain forward {       # add, after the established/invalid rules
    iifname "ycl1" oifname "<public-if>" ip saddr 10.88.1.2 ip daddr != { 10.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16, 100.64.0.0/10, 169.254.0.0/16 } accept
  }
  chain input {         # add
    iifname "ycl1" ct state established,related accept
    iifname "ycl1" drop
  }
```

then `networkctl reload` and `systemctl reload guestnat`. No DNAT rule: nothing from the public
interface reaches the dockerhost. The forward chain's policy drop keeps `ycl0` and `ycl1` apart in
both directions. guestnat.service already turns on IPv4 forwarding. `guestnat.nft` is managed by
the role, so `hh converge` overwrites a hand edit until the role itself takes the second guest.

### On another Linux machine

`scripts/qemu-tap-host-setup.sh ycd0 10.89.0.1/24` creates a transient tap owned by the invoking
user (`sudo`), with the gateway address and IPv4 forwarding; for persistence use the
systemd-networkd files above. Without a firewall that already has a forward policy, an own table
gives the same guarantees:

```
table inet ycluster_dockerhost {
  chain postrouting {
    type nat hook postrouting priority srcnat; policy accept;
    ip saddr 10.89.0.2 oifname "<uplink>" masquerade
  }
  chain forward {
    type filter hook forward priority filter; policy accept;
    oifname "ycd0" ct state established,related accept
    oifname "ycd0" drop
    iifname "ycd0" ip saddr != 10.89.0.2 drop
    iifname "ycd0" ip daddr { 10.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16, 100.64.0.0/10, 169.254.0.0/16 } drop
  }
  chain input {
    type filter hook input priority filter; policy accept;
    iifname "ycd0" ct state established,related accept
    iifname "ycd0" drop
  }
}
```

### On a bridge

A host that keeps its guests on a bridge gives the dockerhost a tap that is a port of the bridge
(`ip tuntap add dev ycd0 mode tap user <user>; ip link set ycd0 master <bridge> up`, or
`Bridge=` in the tap's `.network`). The configuration names the tap; provision accepts the
gateway address on the bridge.

## Portability rules for consumers

Hold for host Docker, cicd-v1's `tcp://dockerd:2375`, and the dockerhost alike.

1. Address containers with `getHost()`/`Host(ctx)` and the mapped port. Never `localhost`/`127.0.0.1`.
2. No bind mounts of host paths. Copy files in (`withCopyFileToContainer`, `Files:`) or use named
   volumes and `exec` (checkit `logs/nodes-to-gcs` bans `withBindMounts` in eslint for this reason).
3. Prefer random host ports. A fixed port must be advertised with the daemon's host, not
   localhost, and collides with any concurrent run on the same daemon (and with a run less than
   ~10 s earlier, before Ryuk reaps).
4. No `host.docker.internal`; use Testcontainers host access.
5. testcontainers-go host access needs the fix in testcontainers/testcontainers-go#3877 (merged
   2026-09-04, not in v0.44.0): checkit gateway-v4 pins `v0.44.1-0.20260904110317-daa2901077f5`
   (checkit #7986).

## Consumer status (verified against a stand-in daemon, before the guest existed)

| consumer | remote daemon | notes |
|---|---|---|
| checkit gateway-v4 controlplane itest | works with #7986 | host 96-103 s, remote 111 s |
| checkit logs/nodes-to-gcs itest (turbo) | works | 17 s / 20 s |
| checkit live-v3 build-contract | works unchanged | 212 s / 228 s; images via `contain --tarball` + `docker load` |
| bots gitea review-workflow itest | works | |
| y-build (BuildKit over TLS) | works with the y-build-tls branch | keycloak-v3: 173 s TLS vs 178 s plaintext, cold |
| docker buildx: OCI to client, `--load`, `docker-image://` contexts, `docker save` | works | same timings as host |
| checkit kube/cautionary-bot itest | fails: Kafka advertised at `localhost:39092` | fix planned (rule 3) |
| checkit orgsetup-v2 client-js k3s itest | fails: kubeconfig `127.0.0.1:6443`, fixed port | fix planned (rules 1, 3) |
| checkit live-v3 k3s itest | fails on host Docker too | skipped, never worked |
| ystack yconverge itest | fails: `docker port` + 127.0.0.1 | rule 1 |
| bots agentlogs fluentbit e2e | fails: data bind mounts | rule 2; local by design today |
| Quarkus Dev Services tests (checkit, manual) | not run | fixed localhost ports (45180, 9999, 3306) |
| k3d / y-cluster docker provider based tests | out of scope | they provision clusters on localhost ports; the qemu provider covers KVM hosts |

The stand-in was dind on host Docker with TLS on a bridge address (`ghcr.io/yolean/dockerd:29.4.1-dind`)
and `moby/buildkit:v0.33.0` with TLS. The consumers have not yet run against the KVM guest.

## Open questions

1. Guest network: decided for gle01 (`ycl1`, created by hand, to move into bots `hardened-host`).
   Pending: OPS-BMH-001 #guest-network naming the development guest.
2. Timings in a real KVM guest: measured in the test harness on gle01 (Testing). Consumer suites
   against the guest: not yet run.
3. Guest memory: qemu's resident memory was 1.9 GB after the first boot with a 4 GB guest, the
   same after a container run and a build, and 1.0 GB after a restart (Testing). The proof of
   concept measured 1.2 GB idle and 2.9 GB after tests.
4. Exposure checks on a real guest: passed by hand on gle01 (Exposure checks); `TestDockerhost_Tap`
   pending its first run.
5. `env` without a guest: decided -- nothing, except `unset` lines for a shell that still carries
   this dockerhost's own variables.
6. buildkitd rootful or rootless in the guest: decided -- rootful.

## Testing

```sh
go test ./pkg/dockerhost/ ./pkg/provision/qemu/ ./cmd/y-cluster/   # unit tests; no VM
# a real guest; docker and buildctl clients on PATH or named by the variables
Y_CLUSTER_E2E_DOCKER=... Y_CLUSTER_E2E_BUILDCTL=... \
  go test -tags 'e2e kvm' -run TestDockerhost_TestForwards -timeout 60m ./e2e/
# the product path and the exposure checks, on a prepared tap
Y_CLUSTER_E2E_DOCKERHOST_IFNAME=ycl1 Y_CLUSTER_E2E_DOCKERHOST_GUEST_ADDRESS=10.88.1.2/24 ... \
  go test -tags 'e2e kvm' -run TestDockerhost_Tap -timeout 40m ./e2e/
```

`TestDockerhost_TestForwards` is a test harness, not a way to use the dockerhost: the guest is on
qemu user-mode networking with dockerd, buildkitd and ssh forwarded to 127.0.0.1, so published
container ports have no address the host reaches. It is not reachable from the CLI or the
configuration file. It covers the first boot, the TLS contract and the refusals, a `docker run`
and a `buildctl build`, the guest definition (pins held, unattended-upgrades on, no shared
directories, no CA key and no client key in the guest), idempotent provision, the idle reaper
powering the guest off, `env` after that, the restart from the disk with its image cache, and
teardown.

Measured on gle01 (2026-10-05, 4 GB / 2 vCPU guest, image already downloaded): a new guest
in 1m23s, of which the first boot (apt install of the pinned packages, the BuildKit download,
both daemons up) took 52 s; a reused guest in 0.27 s; a restart after the idle reaper in 23 s; a
cold `docker run busybox` 2 s and a cold `buildctl build` 2 s.

## Evidence

- Inventory of Docker/BuildKit use cases in ~/Yolean/* repos, and the stand-in probe results with
  logs: metalblack `~/Yolean/tmp/dockerhost/` (`INVENTORY.md`, `CONFIRMATIONS.md`, `runs/`). Not
  in git.
- The compliance recommendation this builds on, and the compliance review of this design with
  conditions D-a to D-g: the compliance coordinator's work on gle01. Not in git.
