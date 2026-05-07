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

// TestHetzner_RejectUpstream covers phase 6.d: with
// HetznerConfig.ImageCache.RejectUpstream true, after Provision
// returns:
//
//  1. /etc/rancher/k3s/registries.yaml on the node contains the
//     wildcard reject mirror; and
//  2. an upstream `crictl pull` for an uncached image fails (DNS
//     never resolves the .invalid mirror endpoint).
//
// Inherits TestHetzner_PreloadFromS3's env requirements.
func TestHetzner_RejectUpstream(t *testing.T) {
	if os.Getenv("HCLOUD_TOKEN") == "" {
		t.Skip("HCLOUD_TOKEN unset; opt in to the hetzner e2e by exporting it")
	}
	if os.Getenv("H_S3_ACCESS_KEY") == "" || os.Getenv("H_S3_SECRET_KEY") == "" ||
		os.Getenv("H_S3_BUCKET") == "" || os.Getenv("H_S3_REGION") == "" {
		t.Skip("H_S3_* env vars unset; opt in by sourcing y-cluster-hetzner.env")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
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
			Bucket:         os.Getenv("H_S3_BUCKET"),
			Region:         os.Getenv("H_S3_REGION"),
			RejectUpstream: true,
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

	// (1) registries.yaml is present (k3s source-of-truth, used
	// to regenerate certs.d on a future k3s restart).
	out, err := cluster.SSH(ctx, "sudo cat /etc/rancher/k3s/registries.yaml")
	if err != nil {
		t.Fatalf("read registries.yaml: %v", err)
	}
	got := string(out)
	for _, want := range []string{`"*":`, "reject-upstream-by-y-cluster.invalid"} {
		if !strings.Contains(got, want) {
			t.Errorf("registries.yaml missing %q:\n%s", want, got)
		}
	}

	// (2) hosts.toml is present for _default and the major
	// registries; this is the file containerd actually consults
	// at pull time.
	hostsToml, err := cluster.SSH(ctx, "sudo cat /var/lib/rancher/k3s/agent/etc/containerd/certs.d/_default/hosts.toml")
	if err != nil {
		t.Fatalf("read _default/hosts.toml: %v", err)
	}
	for _, want := range []string{
		`server = "http://reject-upstream-by-y-cluster.invalid:9999"`,
		`capabilities = ["pull", "resolve"]`,
	} {
		if !strings.Contains(string(hostsToml), want) {
			t.Errorf("_default/hosts.toml missing %q:\n%s", want, string(hostsToml))
		}
	}

	// (3) An uncached image pull fails. We use a registry path
	// that's not in the cache (the bucket only has hello-world)
	// and is unlikely to be in containerd's store from any
	// bootstrap step. busybox:1.36-musl is small, on docker.io,
	// and not pulled by k3s/envoy-gateway/reaper.
	//
	// `crictl pull` exits non-zero on failure; we OR with `echo
	// PULL_FAILED` so the SSH command itself doesn't error and
	// we can grep the output deterministically.
	pull, err := cluster.SSH(ctx, "sudo k3s crictl pull busybox:1.36-musl 2>&1 || echo PULL_FAILED")
	if err != nil {
		t.Fatalf("crictl pull (cmd-level): %v", err)
	}
	pullOut := string(pull)
	if !strings.Contains(pullOut, "PULL_FAILED") {
		t.Errorf("expected pull to fail with rejectUpstream on, got:\n%s", pullOut)
	}

	// (4) The reaper Pod must be Running (or already Succeeded)
	// despite the lockdown -- the script's wait-for-reaper gate
	// is the only thing that prevents the lockdown from racing
	// the reaper's first pull of hetznercloud/cli. If the gate
	// regresses, this assertion catches it.
	phase, err := cluster.SSH(ctx, "sudo k3s kubectl -n y-cluster-reaper get pods -l job-name=reaper -o jsonpath='{.items[0].status.phase}'")
	if err != nil {
		t.Fatalf("get reaper Pod phase: %v", err)
	}
	switch strings.TrimSpace(string(phase)) {
	case "Running", "Succeeded":
		// expected
	default:
		t.Errorf("reaper Pod phase = %q after rejectUpstream; want Running/Succeeded (lockdown raced the image pull?)", string(phase))
	}
}
