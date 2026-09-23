#!/usr/bin/env bash
# render.sh -- regenerate cilium.yaml, the Cilium install manifest the
# glesys provisioner applies to a Talos cluster generated without a CNI.
#
# Rendered ahead of time and embedded so the y-cluster binary needs no
# helm at provision time and every cluster gets the same manifest. Bump
# VERSION, run this, commit the result.
#
# The values are Talos's documented Cilium install with kube-proxy kept
# (docs.siderolabs.com/kubernetes-guides/cni/deploying-cilium), plus
# WireGuard encryption of pod traffic between nodes and of the nodes'
# own traffic (encryption.nodeEncryption), which is what the glesys
# provider is for: a compliance row that reads "encrypted traffic
# between components" over a shared provider network.
set -euo pipefail
cd "$(dirname "$0")"
VERSION=1.20.2
helm repo add cilium https://helm.cilium.io/ >/dev/null 2>&1 || true
helm repo update cilium >/dev/null
helm template cilium cilium/cilium --version "$VERSION" --namespace kube-system \
  --set ipam.mode=kubernetes \
  --set kubeProxyReplacement=false \
  --set 'securityContext.capabilities.ciliumAgent={CHOWN,KILL,NET_ADMIN,NET_RAW,IPC_LOCK,SYS_ADMIN,SYS_RESOURCE,DAC_OVERRIDE,FOWNER,SETGID,SETUID}' \
  --set 'securityContext.capabilities.cleanCiliumState={NET_ADMIN,SYS_ADMIN,SYS_RESOURCE}' \
  --set cgroup.autoMount.enabled=false \
  --set cgroup.hostRoot=/sys/fs/cgroup \
  --set encryption.enabled=true \
  --set encryption.type=wireguard \
  --set encryption.nodeEncryption=true \
  > cilium.yaml
echo "wrote cilium.yaml for chart $VERSION"
