// Package provision exposes the cross-provisioner Cluster interface.
// Concrete provisioners live in subpackages (qemu, docker,
// future multipass/lima); each returns something that satisfies
// Cluster so generic e2e tests can drive any backend.
package provision

import (
	"context"
	"io"
)

// Cluster is the runtime handle a provisioner returns. Methods are
// the minimum every backend must support to enable generic e2e
// scenarios: kubectl through the merged kubeconfig, command
// execution on the cluster node, and clean teardown.
type Cluster interface {
	// Context returns the kubectl context name the provisioner
	// merged into the host's kubeconfig. Tests pass
	// `--context=<this>` to kubectl, or set KUBECONFIG and rely on
	// the current-context.
	Context() string

	// NodeExec runs a shell command on the cluster node. SSH for
	// VM-based providers; `docker exec` for docker. stdin
	// may be nil; when non-nil it is piped to the remote process,
	// which lets callers stream OCI tarballs into `ctr image
	// import` without a temporary file on the node.
	//
	// Returns combined stdout+stderr. Errors include the remote
	// exit status and any captured output for diagnostics.
	NodeExec(ctx context.Context, cmd string, stdin io.Reader) ([]byte, error)

	// Teardown stops the cluster and (subject to keepDisk) cleans
	// up persistent state. keepDisk is meaningful for VM
	// provisioners that cache a qcow2 disk; container-based
	// provisioners may ignore it.
	Teardown(keepDisk bool) error
}
