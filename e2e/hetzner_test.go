//go:build e2e && hetzner

package e2e

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/Yolean/y-cluster/pkg/provision/config"
	"github.com/Yolean/y-cluster/pkg/provision/hetzner"
)

// TestHetzner_ProvisionTeardown smokes the bare lifecycle a
// Hetzner server goes through under y-cluster's hetzner provider
// (phase 1 of the provisioner branch):
//
//   - Provision creates an Ubuntu cloud server, uploads our SSH
//     key, waits for SSH.
//   - SSH `hostname` succeeds and matches the configured context.
//   - Teardown reverses: server delete + SSH-key delete + state
//     sidecar removed + local key files unlinked.
//
// Skips quietly when HCLOUD_TOKEN is unset (no project for an
// unprivileged CI to bill against; the test is opt-in by env).
func TestHetzner_ProvisionTeardown(t *testing.T) {
	if os.Getenv("HCLOUD_TOKEN") == "" {
		t.Skip("HCLOUD_TOKEN unset; opt in to the hetzner e2e by exporting it")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	logger, _ := zap.NewDevelopment()

	// Use the test name as a context suffix so concurrent runs in
	// the same project don't collide. The context-shape rules
	// (>= 4 chars, DNS-label) constrain the value; "y-c-e2e-..."
	// keeps it tidy.
	ctxName := "y-c-e2e-" + strings.ToLower(strings.ReplaceAll(t.Name(), "_", "-"))
	if len(ctxName) > 50 {
		ctxName = ctxName[:50]
	}

	// Isolate the cache dir so the test never touches the
	// operator's real one.
	t.Setenv(hetzner.CacheDirEnv, t.TempDir())

	cfg := config.HetznerConfig{
		CommonConfig: config.CommonConfig{
			Provider: config.ProviderHetzner,
			Context:  ctxName,
		},
	}
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("config invalid: %v", err)
	}

	cluster, err := hetzner.Provision(ctx, cfg, logger)
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}

	// Cleanup as t.Cleanup so the test always tears down even on
	// downstream assertion failures. Re-running the test on a
	// project with a stranded server requires a manual
	// `hcloud server delete <ctxName>`.
	t.Cleanup(func() {
		if err := hetzner.Teardown(context.Background(), ctxName, logger); err != nil {
			t.Logf("teardown: %v", err)
		}
	})

	// SSH works via the public IPv4. `hostname` should match the
	// context; cloud-init pinned it via preserve_hostname:false +
	// hostname:<ctx>.
	out, err := cluster.SSH(ctx, "hostname")
	if err != nil {
		t.Fatalf("SSH: %v", err)
	}
	if !strings.Contains(string(out), ctxName) {
		t.Errorf("hostname output does not match context: got %q, want substring %q", out, ctxName)
	}

	// State sidecar contains a non-zero server ID + IPv4.
	st := cluster.State()
	if st.ServerID == 0 {
		t.Errorf("state.ServerID is zero; expected a real Hetzner server id")
	}
	if cluster.PublicIPv4() == "" {
		t.Errorf("PublicIPv4 is empty; cluster.Lookup would have nothing to dial")
	}
}

// TestHetzner_PreloadFromS3 covers phase 6.c: with
// HetznerConfig.ImageCache.Bucket set, Provision pulls every
// entry from s3://<bucket>/index.json into the node's containerd
// before envoy-gateway runs.
//
// Pre-requisites:
//   - HCLOUD_TOKEN (server create + teardown)
//   - H_S3_ACCESS_KEY / H_S3_SECRET_KEY / H_S3_REGION /
//     H_S3_BUCKET (presigned URL generation; same env file as
//     above ships them all together)
//   - the bucket must already contain at least one pushed image
//     (run `y-cluster images push hello-world:latest` once).
//
// The test asserts the hello-world ref shows up in
// `k3s ctr -n k8s.io image list` after Provision returns. Skips
// if the env isn't set up, so opt-in by sourcing the env file.
func TestHetzner_PreloadFromS3(t *testing.T) {
	if os.Getenv("HCLOUD_TOKEN") == "" {
		t.Skip("HCLOUD_TOKEN unset; opt in to the hetzner e2e by exporting it")
	}
	if os.Getenv("H_S3_ACCESS_KEY") == "" || os.Getenv("H_S3_SECRET_KEY") == "" ||
		os.Getenv("H_S3_BUCKET") == "" || os.Getenv("H_S3_REGION") == "" {
		t.Skip("H_S3_* env vars unset; opt in by sourcing y-cluster-hetzner.env")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	logger, _ := zap.NewDevelopment()

	ctxName := "y-c-e2e-" + strings.ToLower(strings.ReplaceAll(t.Name(), "_", "-"))
	if len(ctxName) > 50 {
		ctxName = ctxName[:50]
	}
	t.Setenv(hetzner.CacheDirEnv, t.TempDir())

	cfg := config.HetznerConfig{
		CommonConfig: config.CommonConfig{
			Provider: config.ProviderHetzner,
			Context:  ctxName,
		},
		ImageCache: config.HetznerImageCache{
			Bucket: os.Getenv("H_S3_BUCKET"),
			Region: os.Getenv("H_S3_REGION"),
		},
	}
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("config invalid: %v", err)
	}

	cluster, err := hetzner.Provision(ctx, cfg, logger)
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	t.Cleanup(func() {
		if err := hetzner.Teardown(context.Background(), ctxName, logger); err != nil {
			t.Logf("teardown: %v", err)
		}
	})

	// The pre-load step lands every index entry into the k8s.io
	// namespace via `ctr image import`. We expect at least one
	// image (the bucket has hello-world:latest from a prior push)
	// to show up in the list.
	out, err := cluster.SSH(ctx, "sudo k3s ctr -n k8s.io image list -q")
	if err != nil {
		t.Fatalf("ctr image list: %v", err)
	}
	listed := string(out)
	if !strings.Contains(listed, "hello-world") {
		t.Errorf("expected hello-world in containerd's k8s.io namespace after preload; got:\n%s", listed)
	}
}
