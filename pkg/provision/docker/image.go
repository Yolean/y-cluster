package docker

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"
	"go.uber.org/zap"

	"github.com/Yolean/y-cluster/pkg/provision/config"
)

// ManifestExister checks whether a registry has a manifest for the
// given image reference. The interface lets tests swap in a fake
// without making real network calls.
type ManifestExister interface {
	Exists(ctx context.Context, ref string) (bool, error)
}

// DefaultManifestExister uses go-containerregistry's anonymous
// keychain to issue a HEAD against the manifest. Public registries
// (ghcr.io, docker.io rate-limit caveats aside) accept this without
// credentials.
type DefaultManifestExister struct{}

// Exists returns true when the registry returns 200/OK for the
// manifest, false when an anonymous probe gets back any of
// {401 Unauthorized, 403 Forbidden, 404 Not Found} or a
// MANIFEST_UNKNOWN detail, and an error for anything else
// (DNS failures, transport errors, 5xx).
//
// 401/403 are treated as "missing" because the mirror is
// supposed to be public: if an anonymous client can't see it,
// it's effectively absent from this caller's perspective and
// the right move is to fall back to upstream. GHCR in
// particular returns 403 DENIED for both missing-repo and
// private-repo cases, so we can't distinguish them anyway.
//
// Network and 5xx errors still propagate -- transient outages
// shouldn't silently flip the docker provisioner onto upstream
// (Docker Hub rate limits hurt).
func (DefaultManifestExister) Exists(ctx context.Context, ref string) (bool, error) {
	parsed, err := name.ParseReference(ref)
	if err != nil {
		return false, fmt.Errorf("parse %q: %w", ref, err)
	}
	if _, err := remote.Head(parsed, remote.WithContext(ctx)); err != nil {
		var terr *transport.Error
		if errors.As(err, &terr) {
			switch terr.StatusCode {
			case http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound:
				return false, nil
			}
			for _, d := range terr.Errors {
				if d.Code == transport.ManifestUnknownErrorCode {
					return false, nil
				}
			}
		}
		return false, fmt.Errorf("HEAD %s: %w", ref, err)
	}
	return true, nil
}

// ResolveImage decides which container image the docker provisioner
// should run for the given k3s version. The y-cluster mirror
// (config.MirrorImage) is preferred. When the mirror has no
// manifest yet — typical when testing a freshly released k3s
// version before the mirror workflow has copied it — ResolveImage
// falls back to the upstream rancher/k3s image
// (config.UpstreamImage) and logs a warning.
//
// The returned bool indicates whether the upstream fallback was
// used so callers can surface that to the user too.
func ResolveImage(ctx context.Context, version string, exister ManifestExister, logger *zap.Logger) (string, bool, error) {
	if version == "" {
		return "", false, fmt.Errorf("k3s version is empty; check pkg/provision/config/k3s.yaml")
	}
	if logger == nil {
		logger = zap.NewNop()
	}
	if exister == nil {
		exister = DefaultManifestExister{}
	}

	mirror := config.MirrorImage(version)
	upstream := config.UpstreamImage(version)
	if mirror == "" || upstream == "" {
		return "", false, fmt.Errorf("pin file missing mirror.target or mirror.upstream")
	}

	exists, err := exister.Exists(ctx, mirror)
	if err != nil {
		return "", false, fmt.Errorf("probe mirror %s: %w", mirror, err)
	}
	if exists {
		return mirror, false, nil
	}
	logger.Warn(
		"k3s mirror not accessible for this version; falling back to upstream",
		zap.String("mirror", mirror),
		zap.String("upstream", upstream),
		zap.String("version", version),
	)
	return upstream, true, nil
}
