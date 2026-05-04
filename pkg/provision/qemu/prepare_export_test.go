package qemu

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestPrepareInguestScript_NoMACPinning is the regression guard
// for the original Hetzner failure: nothing in the embedded
// netplan body anchors a specific MAC. The script lands the
// netplan into /etc/netplan/50-cloud-init.yaml on the imported
// VM; if it pins a build-host MAC, DHCP fails for any new MAC.
func TestPrepareInguestScript_NoMACPinning(t *testing.T) {
	body := PrepareInguestScript()
	if strings.Contains(body, "macaddress") {
		t.Errorf("prepare-inguest script must not pin macaddress:\n%s", body)
	}
	if strings.Contains(body, "52:54:") {
		t.Errorf("prepare-inguest script must not contain qemu SLIRP MAC:\n%s", body)
	}
}

// TestPrepareInguestScript_GenericNetplan pins the netplan match
// shape so a future edit can't accidentally narrow it (e.g.
// matching only "en*", which would miss Hetzner's eth0).
func TestPrepareInguestScript_GenericNetplan(t *testing.T) {
	body := PrepareInguestScript()
	for _, want := range []string{
		`/etc/netplan/50-cloud-init.yaml`,
		`name: "e*"`,
		`dhcp4: true`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("prepare-inguest script missing %q:\n%s", want, body)
		}
	}
}

// TestPrepareInguestScript_CloudInitClean pins the cloud-init
// reset. We do NOT pass --machine-id (would defeat the
// keep-machine-id stance and break Ubuntu 24.04 DHCP).
func TestPrepareInguestScript_CloudInitClean(t *testing.T) {
	body := PrepareInguestScript()
	if !strings.Contains(body, "cloud-init clean --logs --seed") {
		t.Errorf("prepare-inguest script must clean cloud-init state (--logs --seed):\n%s", body)
	}
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		// Skip comment lines so the header's "Do NOT pass --machine-id"
		// note doesn't trip the assertion.
		if strings.HasPrefix(trimmed, "#") {
			continue
		}
		if strings.Contains(trimmed, "cloud-init clean") && strings.Contains(trimmed, "--machine-id") {
			t.Errorf("prepare-inguest script must NOT pass --machine-id to cloud-init clean: %q", trimmed)
		}
	}
}

// TestPrepareInguestScript_DisablesCloudInitNetworkConfig pins
// the cfg drop that prevents cloud-init from regenerating
// /etc/netplan/50-cloud-init.yaml on the imported VM's first
// boot pinned to whatever NIC's MAC.
func TestPrepareInguestScript_DisablesCloudInitNetworkConfig(t *testing.T) {
	body := PrepareInguestScript()
	for _, want := range []string{
		"/etc/cloud/cloud.cfg.d/99-y-cluster-no-network-config.cfg",
		"network: {config: disabled}",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("prepare-inguest script missing %q:\n%s", want, body)
		}
	}
}

// TestPrepareInguestScript_KeepsHostIdentity documents the
// "keep these" stance: machine-id and ssh host keys + user dir
// must NOT be wiped (would break Ubuntu DHCP and customer ssh).
// Ensures the script can't be edited to add e.g.
// `> /etc/machine-id` or `rm -rf ~/.ssh` without a test failure.
func TestPrepareInguestScript_KeepsHostIdentity(t *testing.T) {
	body := PrepareInguestScript()
	mustNotMatch := []string{
		"> /etc/machine-id",
		"rm -f /etc/machine-id",
		"rm /etc/machine-id",
		"truncate -s 0 /etc/machine-id",
		"rm -f /etc/ssh/ssh_host_",
		"rm /etc/ssh/ssh_host_",
		"rm -rf /home/*/.ssh",
		"rm -rf /root/.ssh",
	}
	for _, bad := range mustNotMatch {
		if strings.Contains(body, bad) {
			t.Errorf("prepare-inguest script must not contain %q (host identity must survive prepare):\n%s", bad, body)
		}
	}
}

// TestPrepareExportArgs pins the virt-customize argv shape: the
// disk under -a, the script under --run. With no seed assets the
// argv is the minimal shape; with seed assets the seed --upload /
// --mkdir / --chmod / --run-command flags are inserted BEFORE the
// --run script (virt-customize processes flags in order). Drift
// here means PrepareExport silently runs different libguestfs
// operations.
func TestPrepareExportArgs(t *testing.T) {
	args := prepareExportArgs("/tmp/x.qcow2", "/tmp/script.sh", nil)
	want := []string{"-a", "/tmp/x.qcow2", "--run", "/tmp/script.sh"}
	if len(args) != len(want) {
		t.Fatalf("got %d args, want %d: %v", len(args), len(want), args)
	}
	for i, a := range args {
		if a != want[i] {
			t.Errorf("arg[%d]: got %q, want %q", i, a, want[i])
		}
	}
}

// TestPrepareExportArgs_WithSeed pins that seed assets land BEFORE
// the --run script. Customer-side correctness depends on the
// systemd unit being present on the disk before prepare_inguest's
// `systemctl enable y-cluster-data-seed.service` runs.
func TestPrepareExportArgs_WithSeed(t *testing.T) {
	seed := &SeedAssets{
		SeedTarPath:    "/tmp/seed.tar.zst",
		SeedMetaPath:   "/tmp/seed.meta.json",
		SeedCheckPath:  "/tmp/seed-check",
		SeedStatusPath: "/tmp/seed-status",
		UnitPath:       "/tmp/seed.service",
		K3sDropinPath:  "/tmp/k3s-dropin.conf",
	}
	args := prepareExportArgs("/tmp/x.qcow2", "/tmp/script.sh", seed)

	if args[0] != "-a" || args[1] != "/tmp/x.qcow2" {
		t.Fatalf("first two args should be -a <disk>, got %v", args[:2])
	}
	// --run must appear once and at the end.
	runIdx := -1
	for i, a := range args {
		if a == "--run" {
			if runIdx >= 0 {
				t.Fatalf("--run appears more than once: %v", args)
			}
			runIdx = i
		}
	}
	if runIdx == -1 || runIdx != len(args)-2 || args[runIdx+1] != "/tmp/script.sh" {
		t.Fatalf("--run must be the last flag: %v", args)
	}
	// The script unit upload MUST be before --run.
	wantUnitArg := "/tmp/seed.service:/etc/systemd/system/y-cluster-data-seed.service"
	found := false
	for i, a := range args {
		if a == wantUnitArg {
			if i > runIdx {
				t.Errorf("seed unit upload appears AFTER --run: index %d > %d", i, runIdx)
			}
			found = true
		}
	}
	if !found {
		t.Errorf("seed unit upload not found in args: %v", args)
	}
}

// TestWritePrepareInguestScript checks that the embedded script
// round-trips through the temp-file helper unchanged and is
// executable.
func TestWritePrepareInguestScript(t *testing.T) {
	path, err := WritePrepareInguestScript(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(path)

	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o755 {
		t.Errorf("script mode: got %v, want 0755", st.Mode().Perm())
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != PrepareInguestScript() {
		t.Errorf("written content does not match embedded script")
	}
}

// TestPrepareExport_NoSavedState exercises the "no saved state"
// branch: the error must point the user at `y-cluster provision`,
// not bubble up an opaque os.IsNotExist.
func TestPrepareExport_NoSavedState(t *testing.T) {
	err := PrepareExport(context.Background(), t.TempDir(), "missing", nil)
	if err == nil {
		t.Fatal("expected error when no saved state exists")
	}
	if !strings.Contains(err.Error(), "y-cluster provision") {
		t.Errorf("error should hint at provision: %v", err)
	}
}

// TestPrepareExport_VMRunning exercises the IsRunning guard: a
// stale-but-live pidfile (we write our own pid into it) must
// trigger the "run y-cluster stop first" error rather than
// blindly invoking virt-customize on a busy disk.
func TestPrepareExport_VMRunning(t *testing.T) {
	cacheDir := t.TempDir()
	cfg := defaultedRuntimeConfig(t)
	cfg.CacheDir = cacheDir
	if err := saveState(cfg); err != nil {
		t.Fatal(err)
	}
	pidFile := filepath.Join(cacheDir, cfg.Name+".pid")
	if err := os.WriteFile(pidFile, []byte("1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	err := PrepareExport(context.Background(), cacheDir, cfg.Name, nil)
	if err == nil {
		t.Fatal("expected error when VM still running")
	}
	if !strings.Contains(err.Error(), "y-cluster stop") {
		t.Errorf("error should hint at stop: %v", err)
	}
}

// TestPrepareExport_MissingVirtCustomize exercises the LookPath
// guard. We empty $PATH so virt-customize can't be found, then
// confirm the apt-install hint fires before we ever try to
// invoke it.
func TestPrepareExport_MissingVirtCustomize(t *testing.T) {
	cacheDir := t.TempDir()
	cfg := defaultedRuntimeConfig(t)
	cfg.CacheDir = cacheDir
	if err := saveState(cfg); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", "")

	err := PrepareExport(context.Background(), cacheDir, cfg.Name, nil)
	if err == nil {
		t.Fatal("expected error when virt-customize is missing from PATH")
	}
	if !strings.Contains(err.Error(), "libguestfs-tools") {
		t.Errorf("error should hint at apt install libguestfs-tools: %v", err)
	}
}
