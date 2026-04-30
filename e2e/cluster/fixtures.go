//go:build e2e

package cluster

import (
	"fmt"
	"runtime"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
)

// PushFixtureImage builds a tiny synthetic image (one annotation
// layer, no executable content) and pushes it to the local
// registry under reg.Endpoint/repo:tag. Returns the resolved
// digest reference so callers can compare against what `images
// cache` later resolves.
//
// Synthetic images are sub-KB and don't depend on the network
// being available — useful when cluster tests need to push
// something but shouldn't pull busybox / nginx every run.
func PushFixtureImage(t *testing.T, reg *Registry, repo, tag string) (digestRef string, manifestDigest v1.Hash) {
	t.Helper()
	img := mutate.ConfigMediaType(empty.Image, "application/vnd.docker.container.image.v1+json")
	img = mutate.Annotations(img, map[string]string{
		"y-cluster.fixture": fmt.Sprintf("%s:%s", repo, tag),
	}).(v1.Image)

	ref, err := name.NewTag(fmt.Sprintf("%s/%s:%s", reg.Endpoint, repo, tag))
	if err != nil {
		t.Fatalf("parse tag: %v", err)
	}
	if err := remote.Write(ref, img); err != nil {
		t.Fatalf("push %s: %v", ref, err)
	}
	digest, err := img.Digest()
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	return fmt.Sprintf("%s/%s@%s", reg.Endpoint, repo, digest), digest
}

// SaveFixtureArchive writes a tiny synthetic image as an OCI
// archive (the format `ctr image import` accepts) at archivePath.
// Used by the `images load` arbitrary-OCI tests so they don't
// require a running registry.
//
// The image is stamped linux/<runtime.GOARCH> in its config;
// without a platform, containerd's `ctr image import` filters
// the image out as platform-less and the load is silently a no-op.
// GOARCH (rather than a hardcoded amd64) is correct because each
// provisioner runs the guest at the host's architecture: docker
// uses host kernel, qemu uses -cpu host on KVM, multipass uses the
// host hypervisor's native arch. So whatever GOARCH the test
// process compiles for is also what kubelet inside the VM expects.
func SaveFixtureArchive(t *testing.T, archivePath, repo, tag string) {
	t.Helper()
	img := mutate.ConfigMediaType(empty.Image, "application/vnd.docker.container.image.v1+json")
	img, err := mutate.ConfigFile(img, &v1.ConfigFile{
		Architecture: runtime.GOARCH,
		OS:           "linux",
	})
	if err != nil {
		t.Fatalf("set fixture platform: %v", err)
	}
	ref, err := name.NewTag(fmt.Sprintf("%s:%s", repo, tag))
	if err != nil {
		t.Fatalf("parse tag: %v", err)
	}
	if err := tarball.WriteToFile(archivePath, ref, img); err != nil {
		t.Fatalf("save tarball: %v", err)
	}
}
