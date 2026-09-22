package envoygateway

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"go.uber.org/zap"

	"github.com/Yolean/y-cluster/pkg/cache"
)

// installURL is overridable by tests to point at an httptest
// server. Production builds always use the real GitHub Releases
// URL. The format string takes the version (e.g. "v1.7.2").
var installURL = "https://github.com/envoyproxy/gateway/releases/download/%s/install.yaml"

// EnsureOptions controls Ensure's resolution of the cache path
// and version. Empty fields fall back to package defaults --
// Version=Version (the constant), CacheOverride="" (XDG default).
type EnsureOptions struct {
	Version       string
	CacheOverride string
	Logger        *zap.Logger
}

// Ensure resolves the per-version cache directory for an Envoy
// Gateway release, downloads the upstream install.yaml into it
// if missing, and returns the path to the local file.
//
// Idempotent: a present, non-empty install.yaml is a no-op (no
// network access). The download is atomic via a .tmp file +
// rename so a partial download from a previous attempt isn't
// reused.
//
// The version subdirectory is also where pkg/images.Cache will
// drop its OCI layouts when image pre-cache is wired (passing
// the dir as cacheRoot). Purging a version is therefore one
// recursive delete of the returned dir's parent.
func Ensure(ctx context.Context, opts EnsureOptions) (installYAMLPath string, err error) {
	logger := opts.Logger
	if logger == nil {
		logger = zap.NewNop()
	}
	version := opts.Version
	if version == "" {
		version = Version
	}
	dir, err := cache.EnvoyGatewayVersion(opts.CacheOverride, version)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create %s: %w", dir, err)
	}
	path := filepath.Join(dir, "install.yaml")

	if info, err := os.Stat(path); err == nil && info.Size() > 0 {
		logger.Info("envoy gateway install.yaml cached",
			zap.String("path", path),
			zap.String("version", version),
		)
		return path, nil
	}

	url := fmt.Sprintf(installURL, version)
	logger.Info("downloading envoy gateway install.yaml",
		zap.String("url", url),
		zap.String("path", path),
	)
	if err := cache.Download(ctx, url, path); err != nil {
		return "", fmt.Errorf("download install.yaml for %s: %w", version, err)
	}
	return path, nil
}
