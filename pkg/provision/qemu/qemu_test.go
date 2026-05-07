package qemu

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Yolean/y-cluster/pkg/provision/config"
)

// defaultedRuntimeConfig builds a runtime Config from a freshly
// defaulted config.QEMUConfig. Tests use this where they need a
// "typical" config without spelling out every field, exercising both
// the defaults applier (in the config package) and FromConfig (here).
func defaultedRuntimeConfig(t *testing.T) Config {
	t.Helper()
	c := &config.QEMUConfig{CommonConfig: config.CommonConfig{Provider: config.ProviderQEMU}}
	c.ApplyDefaults()
	return FromConfig(c)
}

func TestFromConfig_AppliesDefaults(t *testing.T) {
	cfg := defaultedRuntimeConfig(t)
	if cfg.Name != "y-cluster" {
		t.Fatalf("Name: %q", cfg.Name)
	}
	if cfg.DiskSize != "20G" {
		t.Fatalf("DiskSize: %q", cfg.DiskSize)
	}
	if cfg.Memory != "8192" {
		t.Fatalf("Memory: %q", cfg.Memory)
	}
	if cfg.SSHPort != "2222" {
		t.Fatalf("SSHPort: %q", cfg.SSHPort)
	}
	if cfg.Context != "local" {
		t.Fatalf("Context: %q", cfg.Context)
	}
	if cfg.CacheDir == "" {
		t.Fatal("CacheDir defaulted to empty (should fall back to ~/.cache/y-cluster-qemu)")
	}
	// Default port forwards land here when the on-disk config omits them.
	if len(cfg.PortForwards) != 3 {
		t.Fatalf("PortForwards: %v", cfg.PortForwards)
	}
}

func TestFromConfig_PreservesExplicitPortForwards(t *testing.T) {
	c := &config.QEMUConfig{
		CommonConfig: config.CommonConfig{
			Provider:     config.ProviderQEMU,
			PortForwards: []config.PortForward{{Host: "26443", Guest: "6443"}, {Host: "9090", Guest: "9090"}},
		},
	}
	c.ApplyDefaults()
	rt := FromConfig(c)
	if len(rt.PortForwards) != 2 || rt.PortForwards[1].Guest != "9090" {
		t.Fatalf("port forwards not preserved: %v", rt.PortForwards)
	}
}

func TestIsRunning_NoPidFile(t *testing.T) {
	cfg := defaultedRuntimeConfig(t)
	cfg.CacheDir = t.TempDir()
	running, _ := cfg.IsRunning()
	if running {
		t.Fatal("expected not running when no pid file")
	}
}

func TestIsRunning_StalePidFile(t *testing.T) {
	cfg := defaultedRuntimeConfig(t)
	cfg.CacheDir = t.TempDir()
	pidFile := filepath.Join(cfg.CacheDir, cfg.Name+".pid")
	if err := os.WriteFile(pidFile, []byte("999999999\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	running, _ := cfg.IsRunning()
	if running {
		t.Fatal("expected not running for stale pid")
	}
}

func TestImportVMDK_MissingVMDK(t *testing.T) {
	if err := ImportVMDK("/nonexistent/disk.vmdk", "/tmp/out.qcow2"); err == nil {
		t.Fatal("expected error for missing VMDK")
	}
}

func TestTeardownConfig_NoPidFile(t *testing.T) {
	cfg := defaultedRuntimeConfig(t)
	cfg.CacheDir = t.TempDir()
	cfg.Kubeconfig = ""
	if err := TeardownConfig(cfg, false, nil); err != nil {
		t.Fatal(err)
	}
}

func TestTeardownConfig_KeepDisk(t *testing.T) {
	cfg := defaultedRuntimeConfig(t)
	cfg.CacheDir = t.TempDir()
	cfg.Kubeconfig = ""
	diskPath := filepath.Join(cfg.CacheDir, cfg.Name+".qcow2")
	if err := os.WriteFile(diskPath, []byte("fake"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := TeardownConfig(cfg, true, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(diskPath); err != nil {
		t.Fatal("disk should be preserved with keepDisk=true")
	}
}

// TestRenderCloudInitUserData_DatasourceListPin guards the
// portability fix: the seed must drop a cloud-init config
// snippet that pins datasource_list to NoCloud + None so a
// re-imported disk doesn't stall on EC2 IMDS probing.
func TestRenderCloudInitUserData_DatasourceListPin(t *testing.T) {
	body := renderCloudInitUserData("foo", "ssh-ed25519 AAAA test@host\n")
	if !strings.Contains(body, "/etc/cloud/cloud.cfg.d/99-y-cluster-pin.cfg") {
		t.Errorf("user-data must drop pin file under /etc/cloud/cloud.cfg.d/:\n%s", body)
	}
	if !strings.Contains(body, "datasource_list: [NoCloud, None]") {
		t.Errorf("user-data must pin datasource_list to [NoCloud, None]:\n%s", body)
	}
}

// TestRenderCloudInitUserData_KeepsCoreShape pins the rest of the
// user-data so the pin addition didn't accidentally drop the
// hostname / user / sshkey wiring the qemu provisioner relies on.
func TestRenderCloudInitUserData_KeepsCoreShape(t *testing.T) {
	body := renderCloudInitUserData("my-cluster", "ssh-ed25519 KEY user@h\n")
	for _, want := range []string{
		"hostname: my-cluster",
		"name: ystack",
		"sudo: ALL=(ALL) NOPASSWD:ALL",
		"ssh-ed25519 KEY user@h",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("user-data missing %q:\n%s", want, body)
		}
	}
	// Pubkey must be trimmed -- a trailing newline inside the
	// YAML list item produces a malformed block.
	if strings.Contains(body, "user@h\n      - ") {
		t.Errorf("trailing newline on ssh key not trimmed:\n%s", body)
	}
}

func TestTeardownConfig_DeleteDisk(t *testing.T) {
	cfg := defaultedRuntimeConfig(t)
	cfg.CacheDir = t.TempDir()
	cfg.Kubeconfig = ""
	diskPath := filepath.Join(cfg.CacheDir, cfg.Name+".qcow2")
	if err := os.WriteFile(diskPath, []byte("fake"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := TeardownConfig(cfg, false, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(diskPath); err == nil {
		t.Fatal("disk should be deleted with keepDisk=false")
	}
}

// TestTeardownConfig_DeletesKeypair pins the no-key-reuse contract:
// teardown must remove the SSH keypair (and the other per-VM
// artefacts) so the next provision generates a fresh one. Reusing
// keys across customer builds would compromise the per-customer
// appliance handoff.
func TestTeardownConfig_DeletesKeypair(t *testing.T) {
	cfg := defaultedRuntimeConfig(t)
	cfg.CacheDir = t.TempDir()
	cfg.Kubeconfig = ""
	artefacts := []string{
		cfg.Name + ".qcow2",
		cfg.Name + "-ssh",
		cfg.Name + "-ssh.pub",
		cfg.Name + "-seed.img",
		cfg.Name + "-cloud-init.yaml",
		cfg.Name + "-console.log",
		cfg.Name + "-gateway-state.json",
	}
	for _, name := range artefacts {
		if err := os.WriteFile(filepath.Join(cfg.CacheDir, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := TeardownConfig(cfg, false, nil); err != nil {
		t.Fatal(err)
	}
	for _, name := range artefacts {
		if _, err := os.Stat(filepath.Join(cfg.CacheDir, name)); err == nil {
			t.Errorf("teardown should remove %s", name)
		}
	}
}

// TestTeardownConfig_KeepDiskKeepsKeypair documents that keepDisk
// also preserves the keypair. Export workflows want both: the
// qcow2 to ship and the keypair that authenticates against it.
func TestTeardownConfig_KeepDiskKeepsKeypair(t *testing.T) {
	cfg := defaultedRuntimeConfig(t)
	cfg.CacheDir = t.TempDir()
	cfg.Kubeconfig = ""
	keyPath := filepath.Join(cfg.CacheDir, cfg.Name+"-ssh")
	if err := os.WriteFile(keyPath, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := TeardownConfig(cfg, true, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(keyPath); err != nil {
		t.Errorf("keepDisk=true must preserve the keypair: %v", err)
	}
}

// TestPerVMArtefacts pins the path layout. Provision and
// PrepareExport create these files; teardown removes them. A
// drift between the two leaves stale state that breaks the
// no-key-reuse contract OR ships a stale gateway-state dump
// in the next prepare-export bundle.
func TestPerVMArtefacts(t *testing.T) {
	got := perVMArtefacts("/c", "n")
	want := []string{
		"/c/n.qcow2",
		"/c/n-ssh",
		"/c/n-ssh.pub",
		"/c/n-seed.img",
		"/c/n-cloud-init.yaml",
		"/c/n-console.log",
		"/c/n-gateway-state.json",
	}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("artefact[%d]: got %q, want %q", i, got[i], want[i])
		}
	}
}
