package hetzner

import (
	"fmt"
	"strings"
)

// renderCloudInitUserData returns the #cloud-config payload sent
// to Hetzner Cloud as user_data on server-create. Hetzner delivers
// it via a NoCloud-compatible cidata volume the guest's cloud-init
// reads at first boot, which our pinned datasource_list ([NoCloud,
// None]) accepts -- the same shape the qemu provisioner uses.
//
// Three things land in the guest at first boot:
//
//  1. The unprivileged user (default ystack) with the operator's
//     freshly-generated SSH public key in authorized_keys.
//  2. A datasource_list pin under /etc/cloud/cloud.cfg.d/ so a
//     re-imaged or snapshot-restored host doesn't stall probing
//     EC2 IMDS / GCE metadata. Cosmetic on Hetzner-stays-Hetzner
//     but matches the qemu provisioner's convention so future
//     image-export paths inherit the pin.
//  3. NO k3s install -- phase 1 keeps cloud-init small and
//     installs k3s via SSH after first boot. That's the curl|sh
//     "script" install in pkg/provision/qemu/k3s.go's
//     installK3sScript shape; Hetzner servers have outbound HTTPS
//     so the script-mode is sufficient. (Airgap-mode mirroring
//     qemu lands in a later phase if the dev-cluster experience
//     needs it.)
func renderCloudInitUserData(hostname, sshUser, sshPubKey string) string {
	return fmt.Sprintf(`#cloud-config
hostname: %s
preserve_hostname: false
users:
  - name: %s
    sudo: ALL=(ALL) NOPASSWD:ALL
    shell: /bin/bash
    ssh_authorized_keys:
      - %s
package_update: false
write_files:
  - path: /etc/cloud/cloud.cfg.d/99-y-cluster-pin.cfg
    permissions: '0644'
    content: |
      # y-cluster: bind cloud-init datasource discovery so a
      # re-imaged host does not stall probing EC2 IMDS / GCE
      # metadata. NoCloud covers Hetzner's cidata user_data
      # delivery; None lets cloud-init proceed when no NoCloud
      # source is present.
      datasource_list: [NoCloud, None]
`, hostname, sshUser, strings.TrimSpace(sshPubKey))
}
