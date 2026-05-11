package images

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/layout"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"go.uber.org/zap"

	"github.com/Yolean/y-cluster/pkg/cache"
)

// Cache pulls a single registry reference into the y-cluster
// shared image cache (cache.Images()). Idempotent on the
// per-image OCI layout: a digest-pinned ref whose layout already
// exists is a no-op; tag-only refs HEAD the registry to
// re-resolve the digest, then no-op when the resolved digest
// already has a layout on disk.
//
// The cache layout is one OCI v1 layout per image, keyed by
// digest, under <cacheRoot>/images/<sha256>/. cacheRoot empty
// means use cache.Root("") (XDG default; honors
// $Y_CLUSTER_CACHE_DIR).
//
// Returns the resolved digest reference (always digest-pinned)
// so callers can record exactly what was cached and reuse it
// for a subsequent Load.
func Cache(ctx context.Context, ref, cacheRoot string, logger *zap.Logger) (string, error) {
	if logger == nil {
		logger = zap.NewNop()
	}
	parsed, err := name.ParseReference(ref)
	if err != nil {
		return "", fmt.Errorf("parse %q: %w", ref, err)
	}

	imagesDir, err := cache.Images(cacheRoot)
	if err != nil {
		return "", err
	}

	// Resolve the input to a digest. For digest-pinned input the
	// digest comes from the ref itself -- no network call needed,
	// which is what makes a digest-pinned warm-cache hit fully
	// offline-safe. For tag-only input we HEAD the registry to
	// translate tag -> digest.
	var digest v1.Hash
	if dr, ok := parsed.(name.Digest); ok {
		digest, err = v1.NewHash(dr.DigestStr())
		if err != nil {
			return "", fmt.Errorf("parse digest %s: %w", dr.DigestStr(), err)
		}
	} else {
		desc, err := remote.Head(parsed, remote.WithContext(ctx))
		if err != nil {
			return "", fmt.Errorf("HEAD %s: %w", ref, err)
		}
		digest = desc.Digest
	}
	digestRef, err := digestReference(parsed, digest)
	if err != nil {
		return "", err
	}

	dir := filepath.Join(imagesDir, digest.String())
	if exists, err := layoutExists(dir); err != nil {
		return "", err
	} else if exists {
		logger.Info("image already cached",
			zap.String("ref", digestRef),
			zap.String("path", dir),
		)
		return digestRef, nil
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create %s: %w", dir, err)
	}
	logger.Info("pulling image",
		zap.String("ref", digestRef),
		zap.String("path", dir),
	)

	// remote.Get returns either a v1.Image or v1.ImageIndex
	// depending on whether the manifest is single- or multi-arch;
	// the layout package writes either kind via the right method.
	got, err := remote.Get(parsed.Context().Digest(digest.String()), remote.WithContext(ctx))
	if err != nil {
		_ = os.RemoveAll(dir)
		return "", fmt.Errorf("fetch %s: %w", digestRef, err)
	}
	lp, err := layout.Write(dir, empty.Index)
	if err != nil {
		_ = os.RemoveAll(dir)
		return "", fmt.Errorf("init layout %s: %w", dir, err)
	}
	if got.MediaType.IsIndex() {
		idx, err := got.ImageIndex()
		if err != nil {
			_ = os.RemoveAll(dir)
			return "", fmt.Errorf("decode index %s: %w", digestRef, err)
		}
		if err := lp.AppendIndex(idx, layout.WithAnnotations(map[string]string{
			"org.opencontainers.image.ref.name": ref,
		})); err != nil {
			_ = os.RemoveAll(dir)
			return "", fmt.Errorf("write index %s: %w", digestRef, err)
		}
	} else {
		img, err := got.Image()
		if err != nil {
			_ = os.RemoveAll(dir)
			return "", fmt.Errorf("decode image %s: %w", digestRef, err)
		}
		if err := lp.AppendImage(img, layout.WithAnnotations(map[string]string{
			"org.opencontainers.image.ref.name": ref,
		})); err != nil {
			_ = os.RemoveAll(dir)
			return "", fmt.Errorf("write image %s: %w", digestRef, err)
		}
	}
	// Symmetric with the "pulling image" / "image already
	// cached" lines: one info entry per network pull, one per
	// cache hit, one per import. Grep-friendly for operators
	// watching a long sideload script.
	logger.Info("image cached",
		zap.String("ref", digestRef),
		zap.String("path", dir),
	)
	return digestRef, nil
}

// ResolveDigest resolves ref to its digest-pinned form without
// downloading any blobs. Used by the load-by-ref path to ask
// "is this already in the cluster?" before deciding whether to
// pull. Digest-pinned input passes through with no network call;
// tag input HEADs the registry.
func ResolveDigest(ctx context.Context, ref string) (string, error) {
	parsed, err := name.ParseReference(ref)
	if err != nil {
		return "", fmt.Errorf("resolve-digest: parse %q: %w", ref, err)
	}
	var digest v1.Hash
	if dr, ok := parsed.(name.Digest); ok {
		digest, err = v1.NewHash(dr.DigestStr())
		if err != nil {
			return "", fmt.Errorf("resolve-digest: parse digest %s: %w", dr.DigestStr(), err)
		}
	} else {
		desc, err := remote.Head(parsed, remote.WithContext(ctx))
		if err != nil {
			return "", fmt.Errorf("resolve-digest: HEAD %s: %w", ref, err)
		}
		digest = desc.Digest
	}
	return digestReference(parsed, digest)
}

// digestReference rebuilds the input reference with its digest
// resolved, e.g. "nginx:1.27" → "nginx@sha256:abc…", preserving
// repository / registry. Used for log lines and for the return
// value of Cache so callers always know exactly what landed.
func digestReference(parsed name.Reference, d v1.Hash) (string, error) {
	dr, err := name.NewDigest(parsed.Context().Name() + "@" + d.String())
	if err != nil {
		return "", fmt.Errorf("build digest ref: %w", err)
	}
	return dr.String(), nil
}

// layoutExists reports whether dir holds a usable OCI layout.
// The OCI v1 layout spec mandates oci-layout + index.json at the
// root; either's absence means the directory is unusable as a
// layout (whether brand-new or leftover from a partial pull).
func layoutExists(dir string) (bool, error) {
	for _, f := range []string{"oci-layout", "index.json"} {
		_, err := os.Stat(filepath.Join(dir, f))
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return false, nil
			}
			return false, err
		}
	}
	return true, nil
}

