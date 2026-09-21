package registries

import (
	"bytes"
	"context"
	"fmt"
	"io"

	"go.uber.org/zap"

	"github.com/Yolean/y-cluster/pkg/provision/config"
)

// WriteToNode stages r as registries.yaml on a VM node. It has to run
// before k3s starts: containerd reads the file once, at start. An
// empty r writes nothing, which leaves containerd on its defaults.
//
// exec has the signature of provision.Cluster's NodeExec. The file
// may carry registry credentials, hence root-only 0600.
func WriteToNode(ctx context.Context, exec func(ctx context.Context, command string, stdin io.Reader) ([]byte, error), r config.Registries, logger *zap.Logger) error {
	body, err := Marshal(r)
	if err != nil {
		return err
	}
	if body == nil {
		return nil
	}
	logger.Info("writing registries.yaml",
		zap.String("path", Path),
		zap.Int("mirrors", len(r.Mirrors)),
		zap.Int("configs", len(r.Configs)),
	)
	cmd := "sudo install -d -m 0755 /etc/rancher/k3s && sudo install -m 0600 /dev/stdin " + Path
	if out, err := exec(ctx, cmd, bytes.NewReader(body)); err != nil {
		return fmt.Errorf("write %s: %s: %w", Path, out, err)
	}
	return nil
}
