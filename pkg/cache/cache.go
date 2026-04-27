// Package cache resolves the on-disk root y-cluster uses for
// downloaded artefacts (k3s airgap bundles, OCI image layouts).
// Runtime state — qcow2 disks, pidfiles, ssh keys — stays in
// each provisioner's own cache (e.g. ~/.cache/y-cluster-qemu)
// because that's not a "download" and shouldn't be cleared by
// `y-cluster cache purge`.
//
// Resolution order on every command that needs the cache root:
//
//	1. --cache-dir=<path>      (per-command flag override)
//	2. $Y_CLUSTER_CACHE_DIR    (env override)
//	3. $XDG_CACHE_HOME/y-cluster
//	4. $HOME/.cache/y-cluster  (POSIX fallback when XDG is unset)
//
// All four candidates collapse to the same root; the subtrees
// (Images, K3s, EnvoyGateway) are conventional names beneath it
// so a user can `ls $(y-cluster cache info -p)` and see what's
// there.
package cache

import (
	"fmt"
	"os"
	"path/filepath"
)

// Root returns the resolved cache directory. The directory is
// not created here — callers that write into Images() / K3s()
// own MkdirAll. flagOverride is the value of a `--cache-dir`
// flag (empty means "no flag was given").
func Root(flagOverride string) (string, error) {
	if flagOverride != "" {
		return filepath.Abs(flagOverride)
	}
	if env := os.Getenv("Y_CLUSTER_CACHE_DIR"); env != "" {
		return filepath.Abs(env)
	}
	if xdg := os.Getenv("XDG_CACHE_HOME"); xdg != "" {
		return filepath.Join(xdg, "y-cluster"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve cache dir: %w", err)
	}
	return filepath.Join(home, ".cache", "y-cluster"), nil
}

// Images returns the per-image OCI layout root. Each image lives
// under <root>/images/<sha256>/ so a digest-pinned ref is a stable
// directory name; tag-pinned refs resolve to a digest at pull time
// and write into the matching directory.
func Images(flagOverride string) (string, error) {
	root, err := Root(flagOverride)
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "images"), nil
}

// K3s returns the k3s download root: airgap tarballs, k3s binary,
// per-version. Replaces the qemu provisioner's old
// ~/.cache/y-cluster-qemu/airgap/<version> location.
func K3s(flagOverride string) (string, error) {
	root, err := Root(flagOverride)
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "k3s"), nil
}

// EnvoyGateway returns the Envoy Gateway download root. Each EG
// release lives under <root>/envoygateway/<version>/ and contains
//
//	install.yaml         -- upstream release manifest
//	images/<digest>/     -- OCI layouts of the EG container
//	                        images, written by pkg/images.Cache
//	                        with this dir as its --cache-dir
//	                        override (per-version isolation, so
//	                        purging a version is one recursive
//	                        delete).
//
// The version subdirectory is the operator-friendly purge unit;
// the shared <root>/images/ tree is intentionally bypassed for
// EG so dedup-across-versions doesn't fight purge clarity. The
// EG image surface is small enough (3 images) that the duplicate
// cost is negligible.
func EnvoyGateway(flagOverride string) (string, error) {
	root, err := Root(flagOverride)
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "envoygateway"), nil
}

// EnvoyGatewayVersion is the per-release subdir of EnvoyGateway.
// Pass version directly -- the caller owns mapping its config to
// a release tag (typically envoygateway.Version).
func EnvoyGatewayVersion(flagOverride, version string) (string, error) {
	if version == "" {
		return "", fmt.Errorf("EnvoyGatewayVersion: version is empty")
	}
	root, err := EnvoyGateway(flagOverride)
	if err != nil {
		return "", err
	}
	return filepath.Join(root, version), nil
}
