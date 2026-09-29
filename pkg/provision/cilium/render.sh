#!/usr/bin/env bash
# render.sh -- regenerate the Cilium install manifests the provisioners
# apply to a cluster started without a CNI: cilium-talos.yaml (glesys,
# Talos) and cilium-k3s.yaml (qemu, k3s with --flannel-backend=none).
#
# Rendered ahead of time and embedded so the y-cluster binary needs no
# helm at provision time and every cluster gets the same manifest. Bump
# VERSION, run this with ystack's bin on PATH (y-helm pins helm), commit
# the result. Cilium publishes no static install manifest, only the
# chart, which is why this is rendered here instead of downloaded per
# version like Envoy Gateway's and cert-manager's release manifests.
#
# Shared values: WireGuard encryption of pod traffic between nodes and
# of the nodes' own traffic (encryption.nodeEncryption), kube-proxy
# kept, IPAM from the node's podCIDR. Hubble's TLS comes from the
# chart's cronJob generator, so every cluster creates its own CA at
# install time and the manifests carry no key material (helm's default
# generates one at render time, which would be committed and shared).
set -euo pipefail
cd "$(dirname "$0")"
VERSION=1.20.2
HELM="${HELM:-y-helm}"
command -v "$HELM" >/dev/null || { echo "render.sh: $HELM not on PATH (put ystack's bin on PATH, or set HELM)" >&2; exit 1; }
"$HELM" repo add cilium https://helm.cilium.io/ >/dev/null 2>&1 || true
"$HELM" repo update cilium >/dev/null

render() {
  local out=$1; shift
  "$HELM" template cilium cilium/cilium --version "$VERSION" --namespace kube-system \
    --set ipam.mode=kubernetes \
    --set kubeProxyReplacement=false \
    --set encryption.enabled=true \
    --set encryption.type=wireguard \
    --set encryption.nodeEncryption=true \
    --set hubble.tls.auto.method=cronJob \
    "$@" > "$out"
  if grep -q '^kind: Secret' "$out"; then
    echo "render.sh: $out contains a Secret; key material must not be committed" >&2
    exit 1
  fi
  echo "wrote $out for chart $VERSION"
}

# Talos's documented Cilium install with kube-proxy kept
# (docs.siderolabs.com/kubernetes-guides/cni/deploying-cilium): no
# SYS_MODULE (Talos loads no modules), cgroup v2 already mounted.
render cilium-talos.yaml \
  --set 'securityContext.capabilities.ciliumAgent={CHOWN,KILL,NET_ADMIN,NET_RAW,IPC_LOCK,SYS_ADMIN,SYS_RESOURCE,DAC_OVERRIDE,FOWNER,SETGID,SETUID}' \
  --set 'securityContext.capabilities.cleanCiliumState={NET_ADMIN,SYS_ADMIN,SYS_RESOURCE}' \
  --set cgroup.autoMount.enabled=false \
  --set cgroup.hostRoot=/sys/fs/cgroup

# k3s with --flannel-backend=none writes no CNI section into its
# containerd config, so containerd uses its defaults, /etc/cni/net.d and
# /opt/cni/bin, which are also the chart's (checked on k3s 1.37).
# portmap chaining keeps hostPort working without kube-proxy
# replacement, which is how k3s ServiceLB publishes Envoy Gateway's
# 80/443 on the node; the provisioner links k3s's own portmap plugin
# into /opt/cni/bin. Default capabilities keep SYS_MODULE so the agent
# can load wireguard on Ubuntu. defaultLBServiceIPAM=none leaves
# LoadBalancer Services to k3s ServiceLB; Cilium's LB-IPAM would
# otherwise claim them too. One operator replica: the qemu provisioner
# is a single node, and the second replica of the default two stays
# Pending on its anti-affinity.
render cilium-k3s.yaml \
  --set operator.replicas=1 \
  --set defaultLBServiceIPAM=none \
  --set cni.chainingMode=portmap
