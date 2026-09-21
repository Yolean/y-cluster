package images

import (
	"context"
	"encoding/json"
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

	// The pull is staged next to its final place and renamed in when
	// complete. layout.Write creates oci-layout and index.json before
	// any blob exists, so a pull written straight into dir and then
	// killed would look like a cache hit from then on.
	if err := os.MkdirAll(imagesDir, 0o755); err != nil {
		return "", fmt.Errorf("create %s: %w", imagesDir, err)
	}
	staging, err := os.MkdirTemp(imagesDir, digest.String()+".partial-")
	if err != nil {
		return "", fmt.Errorf("create staging dir in %s: %w", imagesDir, err)
	}
	defer func() { _ = os.RemoveAll(staging) }() // gone already after the rename
	logger.Info("pulling image",
		zap.String("ref", digestRef),
		zap.String("path", dir),
	)

	// remote.Get returns either a v1.Image or v1.ImageIndex
	// depending on whether the manifest is single- or multi-arch;
	// the layout package writes either kind via the right method.
	got, err := remote.Get(parsed.Context().Digest(digest.String()), remote.WithContext(ctx))
	if err != nil {
		return "", fmt.Errorf("fetch %s: %w", digestRef, err)
	}
	lp, err := layout.Write(staging, empty.Index)
	if err != nil {
		return "", fmt.Errorf("init layout %s: %w", staging, err)
	}
	refAnnotation := layout.WithAnnotations(map[string]string{
		"org.opencontainers.image.ref.name": ref,
	})
	if got.MediaType.IsIndex() {
		idx, err := got.ImageIndex()
		if err != nil {
			return "", fmt.Errorf("decode index %s: %w", digestRef, err)
		}
		if err := lp.AppendIndex(idx, refAnnotation); err != nil {
			return "", fmt.Errorf("write index %s: %w", digestRef, err)
		}
	} else {
		img, err := got.Image()
		if err != nil {
			return "", fmt.Errorf("decode image %s: %w", digestRef, err)
		}
		if err := lp.AppendImage(img, refAnnotation); err != nil {
			return "", fmt.Errorf("write image %s: %w", digestRef, err)
		}
	}
	// dir can only exist here as something layoutExists rejected.
	if err := os.RemoveAll(dir); err != nil {
		return "", fmt.Errorf("remove unusable %s: %w", dir, err)
	}
	if err := os.Rename(staging, dir); err != nil {
		// A concurrent pull of the same digest got there first.
		if exists, existsErr := layoutExists(dir); existsErr != nil || !exists {
			return "", fmt.Errorf("move %s into place: %w", staging, err)
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

// layoutExists reports whether dir holds a usable OCI layout: the
// oci-layout marker plus an index.json that lists at least one
// manifest. An index without manifests is what a pull leaves behind
// when it dies before its first blob is complete.
func layoutExists(dir string) (bool, error) {
	if _, err := os.Stat(filepath.Join(dir, "oci-layout")); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	data, err := os.ReadFile(filepath.Join(dir, "index.json"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	var index struct {
		Manifests []json.RawMessage `json:"manifests"`
	}
	if err := json.Unmarshal(data, &index); err != nil {
		// Truncated by a crash: unusable, and safe to pull over.
		return false, nil
	}
	return len(index.Manifests) > 0, nil
}
