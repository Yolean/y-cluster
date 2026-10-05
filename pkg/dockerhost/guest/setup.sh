#!/bin/sh
# y-cluster dockerhost guest setup. cloud-init runs it once, as root, at
# the end of the first boot (runcmd), after it wrote the TLS material and
# the configuration files next to it. See DOCKERHOST.md in y-cluster.
#
# Installs the pinned dockerd and containerd from Docker's apt repository,
# whose signature apt checks with the key cloud-init wrote, and the pinned
# BuildKit release, checked against its digest. Both daemons then listen
# with TLS and client verification only. A non-zero exit fails the first
# boot, which `cloud-init status` reports and y-cluster's provision reads.
set -eu

# shellcheck source=/dev/null
. /etc/y-cluster-dockerhost/pins.env

export DEBIAN_FRONTEND=noninteractive
log() { echo "y-cluster-dockerhost-setup: $*"; }

log "docker-ce $DOCKER_DEB_VERSION, containerd.io $CONTAINERD_DEB_VERSION, buildkit $BUILDKIT_VERSION"

# The idle reaper counts the guest's age from here, and a fresh guest
# starts with a lease.
install -d -m 0755 /var/lib/y-cluster-dockerhost
[ -s /var/lib/y-cluster-dockerhost/created ] || date +%s > /var/lib/y-cluster-dockerhost/created
date +%s > /var/lib/y-cluster-dockerhost/lease

echo "deb [arch=amd64 signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/ubuntu noble stable" \
  > /etc/apt/sources.list.d/docker.list
# unattended-upgrades may hold the dpkg lock on a first boot.
apt-get -o DPkg::Lock::Timeout=600 update -q
apt-get -o DPkg::Lock::Timeout=600 install -y -q --no-install-recommends \
  "docker-ce=$DOCKER_DEB_VERSION" \
  "docker-ce-cli=$DOCKER_DEB_VERSION" \
  "containerd.io=$CONTAINERD_DEB_VERSION"
# Only a y-cluster release moves these, never unattended-upgrades.
apt-mark hold docker-ce docker-ce-cli containerd.io

tmp=$(mktemp -d)
curl -fsSL --retry 5 --retry-all-errors -o "$tmp/buildkit.tar.gz" "$BUILDKIT_URL"
echo "$BUILDKIT_SHA256  $tmp/buildkit.tar.gz" | sha256sum -c -
tar -xzf "$tmp/buildkit.tar.gz" -C /usr/local bin/buildkitd bin/buildctl bin/buildkit-runc
rm -rf "$tmp"

systemctl daemon-reload
# The package started dockerd already; the drop-in adds the TLS listener.
systemctl restart docker.service
systemctl enable --now buildkit.service y-cluster-dockerhost-idle.timer

i=0
while [ "$i" -lt 90 ]; do
  if docker info >/dev/null 2>&1 &&
    buildctl --addr unix:///run/buildkit/buildkitd.sock debug workers >/dev/null 2>&1; then
    log "dockerd $(docker version --format '{{.Server.Version}}') and buildkitd $(buildkitd --version | awk '{print $3}') are up"
    exit 0
  fi
  i=$((i + 1))
  sleep 2
done
log "dockerd or buildkitd did not come up" >&2
systemctl --no-pager status docker.service buildkit.service >&2 || true
exit 1
