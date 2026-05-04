# Packer template that bakes a y-cluster appliance directly on
# Hetzner Cloud and saves it as a snapshot. Replaces the older
# dd-via-rescue path (qemu-img convert + zstd + dd /dev/sda from
# the rescue image) which broke at the "TCP/22 reachable, no SSH
# banner" stage we couldn't diagnose without a console.
#
# Why Packer + hcloud builder:
#   - Hetzner's supported custom-image path is snapshots, not
#     uploaded raw images. Building on Hetzner avoids the BIOS /
#     partition table / network-driver mismatch you hit when you
#     dd a qemu disk onto bare metal.
#   - Packer's hcloud builder owns the lifecycle: spin a temporary
#     server from a stock Ubuntu image, run provisioners over SSH,
#     power off, snapshot, delete the temporary server.
#   - The output (snapshot ID + name) feeds straight into
#     `hcloud server create --image=<id>` for fleet rollout.
#
# Local appliance vs Hetzner appliance:
#   - Local dev still uses `y-cluster provision` against qemu and
#     prepare-export when the operator wants a portable qcow2.
#   - Production / customer Hetzner deploys go through this Packer
#     template instead.
#   - Both share the workload manifests (pkg/echo/template.yaml and
#     the upstream Envoy Gateway install) by re-running the same
#     `y-cluster echo deploy` invocation; only the VM lifecycle
#     diverges.
#
# Required: HCLOUD_TOKEN in env, var.y_cluster_binary set to a
# linux/amd64 y-cluster build. The orchestrator script
# (e2e-appliance-hetzner.sh) supplies both.

packer {
  required_plugins {
    hcloud = {
      source  = "github.com/hetznercloud/hcloud"
      version = ">= 1.6"
    }
  }
}

variable "hcloud_token" {
  type      = string
  default   = "${env("HCLOUD_TOKEN")}"
  sensitive = true
}

variable "snapshot_name" {
  type    = string
  default = "y-cluster-appliance-{{timestamp}}"
}

# cx23 = 2 vCPU / 4 GB RAM / 40 GB disk in hel1, ~€0.006/h.
# Hetzner retired cx22 / cpx21 in EU regions during 2026; the
# x86 shared lineup is now cx*3 / cpx*2 and cax* (Ampere arm).
variable "server_type" {
  type    = string
  default = "cx23"
}

variable "location" {
  type    = string
  default = "hel1"
}

variable "base_image" {
  type    = string
  default = "ubuntu-24.04"
}

variable "k3s_version" {
  type    = string
  default = "v1.35.4+k3s1"
}

# Tracks pkg/provision/envoygateway/version.go's Version constant.
# Kept independent here so `packer build` can be run against an
# older binary if needed; the orchestrator script does NOT pin
# them together to keep that flexibility.
variable "envoy_gateway_version" {
  type    = string
  default = "v1.7.2"
}

variable "y_cluster_binary" {
  type        = string
  description = "Path to a linux/amd64 y-cluster binary to upload onto the build host"
}

variable "prepare_script" {
  type        = string
  description = "Path to pkg/provision/qemu/prepare_inguest.sh -- the shared identity-reset script that also runs against offline qcow2 disks via virt-customize"
}

# Stable k3s node-name baked into the appliance. The build host's
# hostname is whatever Packer assigns (e.g. packer-XXXXXXXX); the
# customer's cloned server will end up with a different hostname
# (Hetzner sets it from the server name on first boot). Pinning
# K3S_NODE_NAME decouples k3s identity from the OS hostname, so
# the cloned server's k3s recognises the node entry baked into
# the snapshot's sqlite datastore. Without this pin, every cloned
# server registers a NEW node under its own hostname while the
# build-host node lingers as orphan, and every workload pod stays
# bound to the dead node.
variable "k3s_node_name" {
  type    = string
  default = "appliance"
}

variable "stateful_manifest" {
  type        = string
  description = "Path to a pre-rendered single-file YAML for the appliance-stateful workload. Packer's file provisioner doesn't recursively upload directories cleanly across all builders, so the orchestrator script `kubectl kustomize`s testdata/appliance-stateful/base into a temp file and passes the path here."
}

variable "localstorage_manifest" {
  type        = string
  description = "Path to a pre-rendered local-path-provisioner manifest (output of `y-cluster localstorage render`). Same shape as stateful_manifest -- a host-rendered single yaml, applied via kubectl on the build VM."
}

source "hcloud" "appliance" {
  token         = var.hcloud_token
  image         = var.base_image
  location      = var.location
  server_type   = var.server_type
  ssh_username  = "root"
  snapshot_name = var.snapshot_name
  snapshot_labels = {
    purpose = "y-cluster-appliance"
  }
}

build {
  sources = ["source.hcloud.appliance"]

  # Stage the y-cluster binary on the build host. Used here for
  # `y-cluster echo deploy`; left on the appliance as a no-cost
  # operator-inspection convenience.
  provisioner "file" {
    source      = var.y_cluster_binary
    destination = "/usr/local/bin/y-cluster"
  }

  # Stage the shared identity-reset script. Same script runs on
  # the qemu prepare-export path via virt-customize. Single
  # source of truth for what the appliance disk looks like at
  # snapshot time.
  provisioner "file" {
    source      = var.prepare_script
    destination = "/usr/local/bin/y-cluster-prepare"
  }

  # Stage the stateful-workload manifest (VersityGW
  # StatefulSet + Service + HTTPRoute + 1Gi local-path PVC).
  # The file is a single rendered YAML produced by the
  # orchestrator's `kubectl kustomize`, so this is a plain
  # one-file scp -- no recursive directory upload, no Packer
  # SSH-communicator quirks.
  provisioner "file" {
    source      = var.stateful_manifest
    destination = "/root/appliance-stateful.yaml"
  }

  # Stage the bundled local-path-provisioner manifest
  # (rendered by `y-cluster localstorage render` on the host).
  # Replaces k3s's disabled local-storage addon with the
  # appliance-shape defaults: path /data/yolean, predictable
  # PVC namespace_name pattern, Retain reclaim.
  provisioner "file" {
    source      = var.localstorage_manifest
    destination = "/root/y-cluster-localstorage.yaml"
  }

  # k3s install + workload + smoketest, all running normally.
  # We run k3s during the build (no INSTALL_K3S_SKIP_START) so
  # the snapshot includes a fully-converged cluster: kubeconfig,
  # sqlite-resident workload state, pulled container images,
  # everything.  The cloned server's k3s recognises the node
  # entry by K3S_NODE_NAME (baked in via /etc/systemd/system/
  # k3s.service.env) and resumes -- no orphan node, no first-boot
  # manifests-dir reconcile loop, faster startup.
  provisioner "shell" {
    inline_shebang = "/bin/bash -eux"
    environment_vars = [
      "K3S_VERSION=${var.k3s_version}",
      "K3S_NODE_NAME=${var.k3s_node_name}",
      "ENVOY_GATEWAY_VERSION=${var.envoy_gateway_version}",
      "KUBECONFIG=/etc/rancher/k3s/k3s.yaml",
    ]
    inline = [
      "cloud-init status --wait",
      "chmod +x /usr/local/bin/y-cluster /usr/local/bin/y-cluster-prepare",
      # Install + start. K3S_NODE_NAME comes from the
      # environment_vars block above; the install script writes
      # it into /etc/systemd/system/k3s.service.env so the
      # cloned server's systemd-managed k3s reads it back on
      # cold boot.
      # --disable=local-storage: y-cluster ships its own
      # local-path-provisioner via the y-cluster-localstorage.yaml
      # applied below; k3s's bundled local-storage would otherwise
      # reconcile our ConfigMap back to the upstream defaults.
      "curl -sfL https://get.k3s.io | INSTALL_K3S_VERSION=\"$K3S_VERSION\" INSTALL_K3S_EXEC='--disable=traefik --disable=local-storage' sh -",
      "until kubectl get nodes 2>/dev/null | grep -qE ' Ready '; do sleep 2; done",
      # Bundled local-path-provisioner with appliance-shape
      # defaults (path /data/yolean, predictable PVC
      # namespace_name pattern, Retain reclaim).
      "kubectl apply --server-side --field-manager=y-cluster -f /root/y-cluster-localstorage.yaml",
      "kubectl --namespace=local-path-storage rollout status deployment/local-path-provisioner --timeout=120s",
      # Envoy Gateway upstream install + the y-cluster GatewayClass.
      "kubectl apply --server-side -f https://github.com/envoyproxy/gateway/releases/download/$ENVOY_GATEWAY_VERSION/install.yaml",
      "kubectl wait --namespace=envoy-gateway-system --for=condition=Available deployments --all --timeout=180s",
      "kubectl apply --server-side -f - <<'EOF'\napiVersion: gateway.networking.k8s.io/v1\nkind: GatewayClass\nmetadata:\n  name: y-cluster\nspec:\n  controllerName: gateway.envoyproxy.io/gatewayclass-controller\nEOF",
      # Echo workload via the standard kubectl path -- y-cluster
      # has no special case for the customer's app.
      "/usr/local/bin/y-cluster echo deploy --context default",
      "kubectl --namespace=y-cluster wait --for=condition=Available deployment/echo --timeout=120s",
      # Stateful workload: VersityGW (S3-over-posix gateway)
      # backed by a local-path PVC. Brings up the persistent-
      # volume code path so the snapshot includes a
      # provisioned PV directory under /var/lib/rancher/k3s/
      # storage, with the StatefulSet bound to it. Cloned
      # servers' k3s recognises the same node-name (appliance)
      # and rebinds the same PV directory -- no orphan, no
      # re-provision.
      "kubectl apply --server-side --field-manager=appliance-build -f /root/appliance-stateful.yaml",
      "kubectl --namespace=appliance-stateful rollout status statefulset/versitygw --timeout=180s",
      # In-VM smoketest: klipper-lb (k3s's bundled LoadBalancer
      # controller) binds host port 80 on the node. Probe both
      # the echo path and the s3 path so a build with a broken
      # PVC, missing storage class, or mis-routed HTTPRoute
      # fails at build time.
      "for i in $(seq 1 60); do curl -fsS http://localhost/q/envoy/echo && break; sleep 2; done",
      "for i in $(seq 1 60); do curl -fsS http://localhost/s3/health && break; sleep 2; done",
    ]
  }

  # Identity reset via the shared script. Runs in the live VM
  # against /etc/cloud/cloud.cfg.d/, /etc/netplan/, log files,
  # bash history, etc.  Same script the qemu prepare-export
  # runs offline; one source of truth.
  #
  # After the script, stop k3s gracefully so the snapshot
  # captures a quiesced sqlite datastore. Packer's hcloud
  # builder powers the VM off and snapshots after this
  # provisioner returns.
  provisioner "shell" {
    inline_shebang = "/bin/bash -eux"
    inline = [
      "/usr/local/bin/y-cluster-prepare",
      "systemctl stop k3s",
      "sync",
    ]
  }
}
