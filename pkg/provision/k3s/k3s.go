// Package k3s is what the provisioners share about putting k3s on a
// node and reaching it afterwards: the server flags, the install
// commands, the readiness wait and the kubeconfig rewrite.
//
// The node is reached through functions the provisioner injects, so
// this package knows nothing about ssh, multipass or docker and every
// step can be tested against a fake node.
package k3s

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/Yolean/y-cluster/pkg/shquote"
)

// NodeExec runs a shell command on the node and returns its combined
// output. It has the signature of provision.Cluster's NodeExec, so a
// provisioner passes its own method value.
type NodeExec func(ctx context.Context, command string, stdin io.Reader) ([]byte, error)

// NodeCopy places a host file on the node.
type NodeCopy func(ctx context.Context, hostPath, nodePath string) error

// KubeconfigPath is where k3s writes its admin kubeconfig.
const KubeconfigPath = "/etc/rancher/k3s/k3s.yaml"

// DisableFlags turn off the k3s addons y-cluster replaces. traefik:
// Envoy Gateway is the cluster ingress, and two controllers would
// fight over :80/:443. local-storage: pkg/provision/localstorage
// ships its own local-path-provisioner, and k3s's deploy controller
// would reconcile that config back to upstream defaults on every
// restart. gateway-api-crd: pkg/provision/envoygateway installs the
// Gateway API CRDs its Envoy Gateway release needs; k3s 1.37's bundled
// ones carry a ValidatingAdmissionPolicy (safe-upgrades) that refuses
// that apply. Older k3s releases have no such addon and ignore the flag.
var DisableFlags = []string{"--disable=traefik", "--disable=local-storage", "--disable=gateway-api-crd"}

// CiliumFlags start k3s without a CNI for pkg/provision/cilium to
// install: no flannel, and no k3s network policy controller, which
// would enforce NetworkPolicy alongside Cilium. Only provisioners that
// install Cilium pass them (qemu); the rest keep flannel.
var CiliumFlags = []string{"--flannel-backend=none", "--disable-network-policy"}

// ServerFlags is the INSTALL_K3S_EXEC value of a VM provisioner:
// DisableFlags, a kubeconfig the unprivileged login user can read,
// then whatever the provisioner adds (--tls-san for every host-side
// apiserver address k3s would not put in its serving cert by itself).
func ServerFlags(extra ...string) string {
	flags := append([]string{"--write-kubeconfig-mode=644"}, DisableFlags...)
	return strings.Join(append(flags, extra...), " ")
}

// installScriptBaseURL serves the installer as committed at each k3s
// release tag, a variable so tests can serve it. get.k3s.io serves the
// installer of the default branch, which can differ from the pinned
// release and has been down (HTTP 500 on 2026-09-29) while GitHub was not.
var installScriptBaseURL = "https://raw.githubusercontent.com/k3s-io/k3s"

// installScriptURL is the installer of release version.
func installScriptURL(version string) string {
	return installScriptBaseURL + "/" + strings.ReplaceAll(version, "+", "%2B") + "/install.sh"
}

// installScriptNodePath is where the installer is put on the node.
const installScriptNodePath = "/tmp/k3s-install.sh"

// installCommand runs the installer at installScriptNodePath with its
// settings in the environment. skipDownload is the airgap form: the
// installer sets up the systemd unit around a binary that is already
// in place.
func installCommand(version, serverFlags string, skipDownload bool) string {
	env := "INSTALL_K3S_VERSION=" + shquote.Quote(version)
	if skipDownload {
		env += " INSTALL_K3S_SKIP_DOWNLOAD=true"
	}
	env += " INSTALL_K3S_EXEC=" + shquote.Quote(serverFlags)
	return env + " sudo -E sh " + installScriptNodePath
}

// fetchInstallScriptCommand downloads the installer on the node. A
// separate step and not `curl | sh`: in a pipe a failed download hands
// sh empty input, which exits 0 with nothing installed.
func fetchInstallScriptCommand(version string) string {
	return "curl -sSfL -o " + installScriptNodePath + " " + shquote.Quote(installScriptURL(version))
}

// InstallScript runs the upstream installer of the release, which
// downloads k3s on the node. The node needs outbound HTTPS to GitHub.
func InstallScript(ctx context.Context, exec NodeExec, version, serverFlags string) error {
	if version == "" {
		return errNoVersion
	}
	for _, step := range []string{fetchInstallScriptCommand(version), installCommand(version, serverFlags, false)} {
		if out, err := exec(ctx, step, nil); err != nil {
			return fmt.Errorf("k3s install script: %s: %w", out, err)
		}
	}
	return nil
}

var errNoVersion = fmt.Errorf("k3s.version is empty; pkg/provision/config sets a pin-driven default")

// readyProbe runs on the node and prints "ok" once the apiserver
// serves /readyz. The kubeconfig file alone says nothing on a disk
// that has booted before (start, or provision of an imported disk):
// it is already there while k3s is still coming up, and the first
// kubectl call after "ready" got ServiceUnavailable.
const readyProbe = "sudo test -s " + KubeconfigPath + " && sudo k3s kubectl get --raw=/readyz"

// readyPollInterval is a variable so tests need not wait for it.
var readyPollInterval = 2 * time.Second

// WaitReady polls the node until the apiserver is ready or timeout
// passes. The installer returns when systemd has started k3s, which
// is well before the apiserver answers.
func WaitReady(ctx context.Context, exec NodeExec, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		out, err := exec(ctx, readyProbe, nil)
		if err == nil && strings.TrimSpace(string(out)) == "ok" {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("k3s apiserver not ready within %s (last answer: %s)", timeout, strings.TrimSpace(string(out)))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(readyPollInterval):
		}
	}
}

// nodeAPIServer is the server address k3s writes into its own
// kubeconfig: the apiserver as seen from inside the node.
const nodeAPIServer = "127.0.0.1:6443"

// ReadKubeconfig reads the k3s kubeconfig off the node and points it
// at hostPort, the apiserver address as seen from the host. TLS only
// validates if that address is among the serving cert's SANs: k3s
// lists 127.0.0.1 and the node's own addresses, anything else needs a
// --tls-san server flag.
func ReadKubeconfig(ctx context.Context, exec NodeExec, hostPort string) ([]byte, error) {
	raw, err := exec(ctx, "sudo cat "+KubeconfigPath, nil)
	if err != nil {
		return nil, fmt.Errorf("read %s: %s: %w", KubeconfigPath, raw, err)
	}
	return RewriteKubeconfigServer(raw, hostPort)
}

// RewriteKubeconfigServer replaces the node-local apiserver address
// in a k3s kubeconfig with hostPort. Input that does not name the
// node-local address is not the file k3s writes (an error message on
// stdout, a k3s that changed its format) and is refused rather than
// merged into the operator's kubeconfig as it is.
func RewriteKubeconfigServer(raw []byte, hostPort string) ([]byte, error) {
	if !bytes.Contains(raw, []byte(nodeAPIServer)) {
		return nil, fmt.Errorf("k3s kubeconfig does not name %s as its server; cannot point it at %s", nodeAPIServer, hostPort)
	}
	return bytes.ReplaceAll(raw, []byte(nodeAPIServer), []byte(hostPort)), nil
}
