package hetzner

import (
	"bytes"
	"context"
	"fmt"
	"io"

	"go.uber.org/zap"

	"github.com/Yolean/y-cluster/pkg/images"
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
	run := func(ctx context.Context, cmd string, stdin []byte) ([]byte, error) {
		var r io.Reader
		if len(stdin) > 0 {
			r = bytes.NewReader(stdin)
		}
		return c.NodeExec(ctx, cmd, r)
	}
	c.logger.Info("pre-loading images from S3",
		zap.String("bucket", s3.Bucket),
		zap.String("region", s3.Region))
	return images.PreloadFromS3(ctx, s3, run, c.logger)
}
