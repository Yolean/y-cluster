package images

import (
	"bytes"
	"context"
	"fmt"
	"io"

	"go.uber.org/zap"

	"github.com/Yolean/y-cluster/pkg/cluster"
)

// Load streams an OCI archive (or any tar containerd's image
// importer accepts) into the cluster's containerd via
// `ctr -n k8s.io image import -`. The archive's manifest carries
// the ref + tag — the same way `ctr image import` would behave
// if the operator ran it on the node directly. No cache is
// touched: callers driving from local build artefacts (e.g. a
// `contain` tarball under /tmp) can purge those independently.
//
// Routing per backend matches the rest of pkg/cluster:
//   - docker: dockerexec.Exec into the k3s container
//   - qemu:   sshexec.ExecStream over SSH
//
// The k8s.io namespace is the one kubelet / containerd reads,
// not the default `default` namespace `ctr` uses without -n.
// Without the explicit namespace the loaded image is invisible
// to kubernetes.
func Load(ctx context.Context, lr *cluster.LookupResult, archive io.Reader, logger *zap.Logger) error {
	if logger == nil {
		logger = zap.NewNop()
	}
	if archive == nil {
		return fmt.Errorf("nil archive reader")
	}
	args := []string{"-n", "k8s.io", "image", "import", "-"}
	logger.Info("loading image archive",
		zap.String("backend", string(lr.Backend)),
		zap.String("cluster", lr.ClusterName),
	)
	var stdout, stderr bytes.Buffer
	if err := cluster.RunCtr(ctx, lr, args, archive, &stdout, &stderr); err != nil {
		// Surface whatever the remote `ctr import` printed —
		// "ctr: image-related error: ..." is the most common
		// case and worth showing verbatim.
		return fmt.Errorf("ctr image import: %s%s: %w",
			stdout.String(), stderr.String(), err)
	}
	if stdout.Len() > 0 {
		logger.Info("ctr image import",
			zap.String("output", stdout.String()),
		)
	}
	return nil
}
