package k3s

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"go.uber.org/zap"

	"github.com/Yolean/y-cluster/pkg/cache"
)

// releaseBaseURL is a variable so tests can serve the artifacts.
var releaseBaseURL = "https://github.com/k3s-io/k3s/releases/download"

// airgapImagesDir is where k3s imports image tarballs from at start.
const airgapImagesDir = "/var/lib/rancher/k3s/agent/images"

// artifacts names the two release files an airgap install needs, as
// k3s publishes them for one CPU architecture.
type artifacts struct {
	binary string
	images string
}

// artifactsFor maps `uname -m` output to release file names. The
// amd64 binary is the one without a suffix.
func artifactsFor(unameMachine string) (artifacts, error) {
	switch unameMachine {
	case "x86_64":
		return artifacts{binary: "k3s", images: "k3s-airgap-images-amd64.tar.zst"}, nil
	case "aarch64", "arm64":
		return artifacts{binary: "k3s-arm64", images: "k3s-airgap-images-arm64.tar.zst"}, nil
	default:
		return artifacts{}, fmt.Errorf("no k3s airgap artifacts for node architecture %q", unameMachine)
	}
}

// InstallAirgap downloads the k3s binary and image tarball on the
// host (once per version, into the shared cache), copies them to the
// node and runs the installer around them. The node pulls nothing,
// which is the point when its outbound is slow, rate-limited or
// absent.
//
// The architecture is asked of the node rather than assumed from the
// host: a multipass VM on Apple silicon is arm64 whatever y-cluster
// was built for.
func InstallAirgap(ctx context.Context, exec NodeExec, copy NodeCopy, version, serverFlags string, logger *zap.Logger) error {
	if version == "" {
		return errNoVersion
	}
	out, err := exec(ctx, "uname -m", nil)
	if err != nil {
		return fmt.Errorf("node architecture: %s: %w", out, err)
	}
	art, err := artifactsFor(strings.TrimSpace(string(out)))
	if err != nil {
		return err
	}
	binPath, imagesPath, err := cacheAirgap(ctx, version, art, logger)
	if err != nil {
		return err
	}

	if err := copy(ctx, binPath, "/tmp/k3s"); err != nil {
		return fmt.Errorf("copy k3s binary to node: %w", err)
	}
	if err := copy(ctx, imagesPath, "/tmp/"+art.images); err != nil {
		return fmt.Errorf("copy airgap images to node: %w", err)
	}
	for _, step := range []string{
		"sudo install -m 755 /tmp/k3s /usr/local/bin/k3s",
		"sudo mkdir -p " + airgapImagesDir,
		"sudo mv /tmp/" + art.images + " " + airgapImagesDir + "/",
		installCommand(version, serverFlags, true),
	} {
		if out, err := exec(ctx, step, nil); err != nil {
			return fmt.Errorf("airgap step %q: %s: %w", step, out, err)
		}
	}
	return nil
}

// cacheAirgap returns the host paths of the two artifacts under
// cache.K3s/<version>/, downloading what is missing. The cache is
// shared by every provisioner and cluster on the host, and
// `y-cluster cache purge --k3s` clears it.
func cacheAirgap(ctx context.Context, version string, art artifacts, logger *zap.Logger) (binPath, imagesPath string, err error) {
	root, err := cache.K3s("")
	if err != nil {
		return "", "", fmt.Errorf("resolve k3s cache: %w", err)
	}
	dir := filepath.Join(root, version)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", "", fmt.Errorf("create k3s cache: %w", err)
	}
	// GitHub release URLs need the `+` of v1.35.3+k3s1 encoded.
	base := releaseBaseURL + "/" + strings.ReplaceAll(version, "+", "%2B") + "/"

	paths := make([]string, 0, 2)
	for _, name := range []string{art.binary, art.images} {
		path := filepath.Join(dir, name)
		if _, statErr := os.Stat(path); statErr != nil {
			logger.Info("downloading k3s release artifact", zap.String("version", version), zap.String("name", name))
			if err := cache.Download(ctx, base+name, path); err != nil {
				return "", "", fmt.Errorf("download %s: %w", name, err)
			}
		}
		paths = append(paths, path)
	}
	return paths[0], paths[1], nil
}
