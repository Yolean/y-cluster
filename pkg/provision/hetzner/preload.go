package hetzner

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"path/filepath"

	"go.uber.org/zap"

	"github.com/Yolean/y-cluster/pkg/images"
	"github.com/Yolean/y-cluster/pkg/sshexec"
)

// preloadFromS3 wires the operator-side S3 config (cluster-yaml
// fields + H_S3_* env credentials) into pkg/images.PreloadFromS3
// and runs the per-image scripts over this cluster's SSH target.
//
// The H_S3_ACCESS_KEY / H_S3_SECRET_KEY pair is required at
// provision time -- the cluster yaml deliberately doesn't carry
// secrets. A missing key surfaces a clear error here rather than
// later in a mid-pre-load 401 from S3.
func (c *Cluster) preloadFromS3(ctx context.Context) error {
	envCfg := images.S3ConfigFromEnv()
	if envCfg.AccessKey == "" || envCfg.SecretKey == "" {
		return fmt.Errorf("imageCache enabled but H_S3_ACCESS_KEY / H_S3_SECRET_KEY are unset; source ~/Yolean/.yolean-bots-device/y-cluster-hetzner.env (or wherever your S3 keys live) before running provision")
	}
	s3 := images.S3Config{
		AccessKey: envCfg.AccessKey,
		SecretKey: envCfg.SecretKey,
		Bucket:    c.cfg.ImageCache.Bucket,
		Region:    c.cfg.ImageCache.Region,
		IndexKey:  c.cfg.ImageCache.IndexKey,
	}
	target := sshexec.Target{
		Host:    c.state.IPv4,
		Port:    "22",
		User:    c.cfg.SSHUser,
		KeyPath: filepath.Join(c.cacheDir, c.cfg.Context+"-ssh"),
	}
	run := func(ctx context.Context, cmd string, stdin []byte) ([]byte, error) {
		var r io.Reader
		if len(stdin) > 0 {
			r = bytes.NewReader(stdin)
		}
		return sshexec.Exec(ctx, target, cmd, r)
	}
	c.logger.Info("pre-loading images from S3",
		zap.String("bucket", s3.Bucket),
		zap.String("region", s3.Region))
	return images.PreloadFromS3(ctx, s3, run, c.logger)
}
