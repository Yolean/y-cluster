package envoygateway

import (
	"context"
	"fmt"
	"io"
	"os"
	"slices"

	"go.uber.org/zap"

	"github.com/Yolean/y-cluster/pkg/cache"
	"github.com/Yolean/y-cluster/pkg/images"
)

// Images extracts the container image references declared by an
// EG install.yaml stream, deduplicated and in YAML stream order,
// followed by proxyImage (gateway.proxyImage) when it is set. Pass an
// open reader -- typically os.Open of the path Ensure returned, or an
// embedded test fixture.
//
// The proxy image the release runs by default is not among them:
// Envoy Gateway has it compiled in, not in install.yaml.
func Images(r io.Reader, proxyImage string) ([]string, error) {
	refs, err := images.ListYAML(r)
	if err != nil {
		return nil, fmt.Errorf("list images from install.yaml: %w", err)
	}
	if proxyImage != "" && !slices.Contains(refs, proxyImage) {
		refs = append(refs, proxyImage)
	}
	return refs, nil
}

// CacheImages pre-pulls every image referenced by install.yaml, and
// proxyImage when it is set, into the per-version cache directory.
// Each image lands under <version>/images/<digest>/ as a standard OCI
// v1 layout, so a future ctr-import-from-layout step can stream the
// bytes onto the node without re-fetching from upstream.
//
// Idempotent: pkg/images.Cache no-ops on a digest already on
// disk. The first pre-cache after an EG version bump pays the
// pull cost; subsequent provisions hit the local layouts.
//
// Returns the list of digest-pinned references actually cached
// (post-resolution from the tag refs in install.yaml), so the
// caller can record what the per-version dir contains.
func CacheImages(ctx context.Context, opts EnsureOptions, proxyImage string) ([]string, error) {
	logger := opts.Logger
	if logger == nil {
		logger = zap.NewNop()
	}
	version := opts.Version
	if version == "" {
		version = Version
	}
	installPath, err := Ensure(ctx, EnsureOptions{
		Version:       version,
		InstallSHA256: opts.InstallSHA256,
		CacheOverride: opts.CacheOverride,
		Logger:        logger,
	})
	if err != nil {
		return nil, err
	}
	f, err := os.Open(installPath)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", installPath, err)
	}
	defer f.Close()
	refs, err := Images(f, proxyImage)
	if err != nil {
		return nil, err
	}

	versionDir, err := cache.EnvoyGatewayVersion(opts.CacheOverride, version)
	if err != nil {
		return nil, err
	}
	logger.Info("pre-caching envoy gateway images",
		zap.Int("count", len(refs)),
		zap.String("dir", versionDir),
	)

	digests := make([]string, 0, len(refs))
	for _, ref := range refs {
		// Pass the per-version dir as the cache root override:
		// pkg/images.Cache appends "images/<digest>" so layouts
		// land at <version>/images/<digest>/.
		digestRef, err := images.Cache(ctx, ref, versionDir, logger)
		if err != nil {
			return nil, fmt.Errorf("cache %s: %w", ref, err)
		}
		digests = append(digests, digestRef)
	}
	return digests, nil
}
