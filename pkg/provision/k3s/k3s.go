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
// restart.
var DisableFlags = []string{"--disable=traefik", "--disable=local-storage"}

// ServerFlags is the INSTALL_K3S_EXEC value of a VM provisioner:
// DisableFlags, a kubeconfig the unprivileged login user can read,
// then whatever the provisioner adds (--tls-san for every host-side
// apiserver address k3s would not put in its serving cert by itself).
func ServerFlags(extra ...string) string {
	flags := append([]string{"--write-kubeconfig-mode=644"}, DisableFlags...)
	return strings.Join(append(flags, extra...), " ")
}

// installCommand is the get.k3s.io invocation. skipDownload is the
// airgap form: the installer sets up the systemd unit around a binary
// that is already in place.
func installCommand(version, serverFlags string, skipDownload bool) string {
	env := "INSTALL_K3S_VERSION=" + shquote.Quote(version)
	if skipDownload {
		env += " INSTALL_K3S_SKIP_DOWNLOAD=true"
	}
	env += " INSTALL_K3S_EXEC=" + shquote.Quote(serverFlags)
	return "curl -sfL https://get.k3s.io | " + env + " sudo -E sh -"
}

// InstallScript runs the upstream installer, which downloads k3s on
// the node. The node needs outbound HTTPS to get.k3s.io and GitHub.
func InstallScript(ctx context.Context, exec NodeExec, version, serverFlags string) error {
	if version == "" {
		return errNoVersion
	}
	out, err := exec(ctx, installCommand(version, serverFlags, false), nil)
	if err != nil {
		return fmt.Errorf("k3s install script: %s: %w", out, err)
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
