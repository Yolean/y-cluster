package certmanager

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"go.uber.org/zap"

	"github.com/Yolean/y-cluster/pkg/cache"
)

// installURL is overridable by tests. The format string takes the
// version (e.g. "v1.21.2").
var installURL = "https://github.com/cert-manager/cert-manager/releases/download/%s/cert-manager.yaml"

// EnsureOptions controls Ensure's resolution of the cache path and
// version. Empty fields fall back to Version and the default cache root.
type EnsureOptions struct {
	Version       string
	CacheOverride string
	Logger        *zap.Logger
}

// Ensure returns the path of the release's cert-manager.yaml in the
// per-version cache, downloading it when missing. A present, non-empty
// file is a cache hit; cache.Download writes through a temporary file,
// so a failed download leaves nothing that looks like one.
func Ensure(ctx context.Context, opts EnsureOptions) (string, error) {
	logger := opts.Logger
	if logger == nil {
		logger = zap.NewNop()
	}
	version := opts.Version
	if version == "" {
		version = Version
	}
	dir, err := cache.CertManagerVersion(opts.CacheOverride, version)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create %s: %w", dir, err)
	}
	path := filepath.Join(dir, "cert-manager.yaml")
	if info, err := os.Stat(path); err == nil && info.Size() > 0 {
		logger.Info("cert-manager manifest cached", zap.String("path", path), zap.String("version", version))
		return path, nil
	}
	url := fmt.Sprintf(installURL, version)
	logger.Info("downloading cert-manager manifest", zap.String("url", url), zap.String("path", path))
	if err := cache.Download(ctx, url, path); err != nil {
		return "", fmt.Errorf("download cert-manager.yaml for %s: %w", version, err)
	}
	return path, nil
}
