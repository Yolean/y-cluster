package k3s

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
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

// artifacts names the release files an airgap install needs, as k3s
// publishes them for one CPU architecture.
type artifacts struct {
	binary string
	images string
	// checksums lists the sha256 of every file of this architecture.
	checksums string
}

// artifactsFor maps `uname -m` output to release file names. The
// amd64 binary is the one without a suffix.
func artifactsFor(unameMachine string) (artifacts, error) {
	switch unameMachine {
	case "x86_64":
		return artifacts{binary: "k3s", images: "k3s-airgap-images-amd64.tar.zst", checksums: "sha256sum-amd64.txt"}, nil
	case "aarch64", "arm64":
		return artifacts{binary: "k3s-arm64", images: "k3s-airgap-images-arm64.tar.zst", checksums: "sha256sum-arm64.txt"}, nil
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

	// What is fetched here runs as root on the node. The release's
	// checksum file is fetched only when something has to be
	// downloaded; a file already in the cache was checked when it
	// got there.
	var sums map[string]string
	paths := make([]string, 0, 2)
	for _, name := range []string{art.binary, art.images} {
		path := filepath.Join(dir, name)
		if _, statErr := os.Stat(path); statErr != nil {
			if sums == nil {
				if sums, err = releaseChecksums(ctx, base+art.checksums); err != nil {
					return "", "", err
				}
			}
			logger.Info("downloading k3s release artifact", zap.String("version", version), zap.String("name", name))
			if err := downloadVerified(ctx, base+name, path, sums[name]); err != nil {
				return "", "", fmt.Errorf("download %s: %w", name, err)
			}
		}
		paths = append(paths, path)
	}
	return paths[0], paths[1], nil
}

// releaseChecksums fetches a sha256sum-<arch>.txt and returns its
// entries by file name.
func releaseChecksums(ctx context.Context, url string) (map[string]string, error) {
	tmp, err := os.CreateTemp("", "k3s-sha256sum-*.txt")
	if err != nil {
		return nil, err
	}
	_ = tmp.Close()
	defer func() { _ = os.Remove(tmp.Name()) }()
	if err := cache.Download(ctx, url, tmp.Name()); err != nil {
		return nil, fmt.Errorf("download k3s checksums: %w", err)
	}
	body, err := os.ReadFile(tmp.Name())
	if err != nil {
		return nil, err
	}
	sums := map[string]string{}
	for _, line := range strings.Split(string(body), "\n") {
		// "<sha256>  <name>", as sha256sum writes it.
		if fields := strings.Fields(line); len(fields) == 2 {
			sums[strings.TrimPrefix(fields[1], "*")] = fields[0]
		}
	}
	return sums, nil
}

// downloadVerified downloads url and moves it to dest only if its
// sha256 is want. dest is where the next run looks for a cache hit,
// so nothing unverified is ever there, not even for a moment.
func downloadVerified(ctx context.Context, url, dest, want string) error {
	if want == "" {
		return fmt.Errorf("the release's checksum file has no entry for %s", filepath.Base(dest))
	}
	unverified := dest + ".unverified"
	defer func() { _ = os.Remove(unverified) }()
	if err := cache.Download(ctx, url, unverified); err != nil {
		return err
	}
	f, err := os.Open(unverified)
	if err != nil {
		return err
	}
	h := sha256.New()
	_, err = io.Copy(h, f)
	_ = f.Close()
	if err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		return fmt.Errorf("sha256 is %s, the release says %s", got, want)
	}
	return os.Rename(unverified, dest)
}
