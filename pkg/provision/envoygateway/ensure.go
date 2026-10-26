package envoygateway

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
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
	Version string
	// InstallSHA256 is the sha256 of the release's install.yaml. Empty
	// means VersionSHA256 when Version is the constant, and is an
	// error for any other release: nothing is applied unverified.
	InstallSHA256 string
	CacheOverride string
	Logger        *zap.Logger
}

// installDigest is the sha256 that version's install.yaml must have.
func installDigest(version, configured string) (string, error) {
	switch {
	case configured != "":
		return configured, nil
	case version == Version:
		return VersionSHA256, nil
	default:
		return "", fmt.Errorf("envoy gateway %s: the sha256 of its install.yaml is required; y-cluster has it only for %s", version, Version)
	}
}

// Ensure resolves the per-version cache directory for an Envoy
// Gateway release, downloads the upstream install.yaml into it
// if missing, and returns the path to the local file.
//
// The file is verified against its sha256 both ways: a download is
// hashed as it is written and only lands in the cache when it matches
// (cache.DownloadSHA256), and a file already in the cache is hashed
// again before each use. A cached file that does not match is an
// error, not a reason to download again: it was corrupted or replaced,
// or the configured digest is wrong, and the operator should know
// which.
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
	want, err := installDigest(version, opts.InstallSHA256)
	if err != nil {
		return "", err
	}
	dir, err := cache.EnvoyGatewayVersion(opts.CacheOverride, version)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create %s: %w", dir, err)
	}
	path := filepath.Join(dir, "install.yaml")

	if _, err := os.Stat(path); err == nil {
		if err := cache.VerifySHA256(path, want); err != nil {
			return "", fmt.Errorf("envoy gateway %s: cached %w: the cache entry was corrupted or replaced, or the configured sha256 is wrong; remove the file to download it again", version, err)
		}
		logger.Info("envoy gateway install.yaml cached",
			zap.String("path", path),
			zap.String("version", version),
			zap.String("sha256", want),
		)
		return path, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}

	url := fmt.Sprintf(installURL, version)
	logger.Info("downloading envoy gateway install.yaml",
		zap.String("url", url),
		zap.String("path", path),
		zap.String("sha256", want),
	)
	if err := cache.DownloadSHA256(ctx, url, path, want); err != nil {
		return "", fmt.Errorf("download install.yaml for %s: %w", version, err)
	}
	return path, nil
}
