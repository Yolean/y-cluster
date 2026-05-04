package qemu

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// runSeedCheck executes the embedded data_seed_check.sh against a
// fake $MOUNT and $SEED setup the caller wires up under tmp.
//
// The script is hardcoded to /data/yolean / /var/lib/y-cluster paths.
// We override them by writing a tiny wrapper that exports the same
// names as shell environment variables and then sources the original
// script with sed-substituted constants. Cheaper than refactoring the
// boot-time script to take env-var paths -- the boot-time script is
// what runs on a real customer machine and shouldn't grow knobs we
// don't need there.
func runSeedCheck(t *testing.T, mount, seed, meta string) (stdout, stderr string, exit int) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("seed-check is /bin/sh-only")
	}
	if _, err := exec.LookPath("zstd"); err != nil {
		t.Skip("zstd not on PATH")
	}

	// Materialise the script with path substitutions.
	src := dataSeedCheckScript
	src = strings.Replace(src, "MOUNT=/data/yolean", "MOUNT="+mount, 1)
	src = strings.Replace(src, "SEED=/var/lib/y-cluster/data-seed.tar.zst", "SEED="+seed, 1)
	src = strings.Replace(src, "META=/var/lib/y-cluster/data-seed.meta.json", "META="+meta, 1)
	// The script's mountpoint check uses `mountpoint -q` against
	// MOUNT. tmp dirs are not mountpoints, so the test would
	// always exit early at "not a separate mount". Replace the
	// guard with a TEST_FORCE_MOUNT env-var override so we can
	// exercise the real branches.
	src = strings.Replace(src,
		`if ! mountpoint -q "$MOUNT" 2>/dev/null; then`,
		`if [ -z "${TEST_FORCE_MOUNT:-}" ] && ! mountpoint -q "$MOUNT" 2>/dev/null; then`,
		1)

	scriptPath := filepath.Join(t.TempDir(), "seed-check.sh")
	if err := os.WriteFile(scriptPath, []byte(src), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/bin/sh", scriptPath)
	cmd.Env = append(os.Environ(), "TEST_FORCE_MOUNT=1")
	var sob, seb strings.Builder
	cmd.Stdout = &sob
	cmd.Stderr = &seb
	err := cmd.Run()
	if exitErr, ok := err.(*exec.ExitError); ok {
		exit = exitErr.ExitCode()
	} else if err != nil {
		t.Fatalf("script run: %v", err)
	}
	return sob.String(), seb.String(), exit
}

// makeSeedTar writes a small tar.zst at seedPath whose contents are
// the entries (path -> body) given. The sha256 of the resulting
// tarball is returned for verification.
func makeSeedTar(t *testing.T, seedPath string, entries map[string]string) {
	t.Helper()
	if _, err := exec.LookPath("zstd"); err != nil {
		t.Skip("zstd not on PATH")
	}
	contentDir := filepath.Join(filepath.Dir(seedPath), "seed-src")
	if err := os.MkdirAll(contentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range entries {
		full := filepath.Join(contentDir, name)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// tar -C contentDir -cf - . | zstd > seedPath
	tarCmd := exec.Command("tar", "-C", contentDir, "-cf", "-", ".")
	tarOut, err := tarCmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	zstdCmd := exec.Command("zstd", "-q", "-")
	zstdCmd.Stdin = tarOut
	out, err := os.Create(seedPath)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	zstdCmd.Stdout = out
	if err := zstdCmd.Start(); err != nil {
		t.Fatal(err)
	}
	if err := tarCmd.Run(); err != nil {
		t.Fatal(err)
	}
	if err := zstdCmd.Wait(); err != nil {
		t.Fatal(err)
	}
}

// TestSeedCheck_MarkerPresent_NoOp pins the upgrade fast path: the
// customer's drive already has a marker, we respect it and exit 0.
func TestSeedCheck_MarkerPresent_NoOp(t *testing.T) {
	dir := t.TempDir()
	mount := filepath.Join(dir, "mount")
	if err := os.MkdirAll(mount, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mount, ".y-cluster-seeded"),
		[]byte(`{"schemaVersion":1,"existing":"marker"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mount, "existing-data.txt"),
		[]byte("customer's data"), 0o644); err != nil {
		t.Fatal(err)
	}

	stdout, _, exit := runSeedCheck(t, mount, "/nonexistent/seed", "/nonexistent/meta")
	if exit != 0 {
		t.Errorf("exit: got %d, want 0; stdout=%q", exit, stdout)
	}
	if !strings.Contains(stdout, "marker present") {
		t.Errorf("expected 'marker present' in stdout, got: %s", stdout)
	}
	// Existing data must be untouched.
	body, _ := os.ReadFile(filepath.Join(mount, "existing-data.txt"))
	if string(body) != "customer's data" {
		t.Errorf("existing data mutated: %q", body)
	}
}

// TestSeedCheck_EmptyMount_Seeds covers the green path: customer
// attached an empty drive, we extract the seed and write the marker.
func TestSeedCheck_EmptyMount_Seeds(t *testing.T) {
	dir := t.TempDir()
	mount := filepath.Join(dir, "mount")
	if err := os.MkdirAll(mount, 0o755); err != nil {
		t.Fatal(err)
	}
	// lost+found is allowed and must be ignored.
	if err := os.MkdirAll(filepath.Join(mount, "lost+found"), 0o755); err != nil {
		t.Fatal(err)
	}

	seed := filepath.Join(dir, "seed.tar.zst")
	meta := filepath.Join(dir, "seed.meta.json")
	makeSeedTar(t, seed, map[string]string{
		"workload-data/db.txt": "schema=v0.4.0",
	})
	if err := os.WriteFile(meta,
		[]byte(`{"schemaVersion":1,"seed_sha256":"sha256:fake"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	stdout, stderr, exit := runSeedCheck(t, mount, seed, meta)
	if exit != 0 {
		t.Fatalf("exit: got %d, want 0; stdout=%q stderr=%q", exit, stdout, stderr)
	}
	body, err := os.ReadFile(filepath.Join(mount, "workload-data/db.txt"))
	if err != nil {
		t.Fatalf("seed file should be extracted: %v", err)
	}
	if string(body) != "schema=v0.4.0" {
		t.Errorf("extracted body: got %q, want schema=v0.4.0", body)
	}
	markerBody, err := os.ReadFile(filepath.Join(mount, ".y-cluster-seeded"))
	if err != nil {
		t.Fatalf("marker should be written: %v", err)
	}
	if !strings.Contains(string(markerBody), "seed_sha256") {
		t.Errorf("marker should contain seed metadata: %s", markerBody)
	}
}

// TestSeedCheck_NonEmptyNoMarker_Conflict pins the safety belt:
// customer drive has unrelated data, no marker -> we refuse and
// exit non-zero so the k3s drop-in blocks startup.
func TestSeedCheck_NonEmptyNoMarker_Conflict(t *testing.T) {
	dir := t.TempDir()
	mount := filepath.Join(dir, "mount")
	if err := os.MkdirAll(mount, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mount, "customer-stuff.txt"),
		[]byte("not ours"), 0o644); err != nil {
		t.Fatal(err)
	}

	seed := filepath.Join(dir, "seed.tar.zst")
	meta := filepath.Join(dir, "seed.meta.json")
	makeSeedTar(t, seed, map[string]string{"x": "y"})
	if err := os.WriteFile(meta, []byte(`{"schemaVersion":1}`), 0o644); err != nil {
		t.Fatal(err)
	}

	stdout, stderr, exit := runSeedCheck(t, mount, seed, meta)
	if exit == 0 {
		t.Errorf("exit: got 0, want non-zero (conflict); stdout=%q", stdout)
	}
	if !strings.Contains(stderr, "refusing to seed") {
		t.Errorf("stderr should mention refusal: %s", stderr)
	}
	if !strings.Contains(stderr, "Resolution") {
		t.Errorf("stderr should include recovery recipes: %s", stderr)
	}
	// Customer file must be untouched.
	body, _ := os.ReadFile(filepath.Join(mount, "customer-stuff.txt"))
	if string(body) != "not ours" {
		t.Errorf("customer file mutated: %q", body)
	}
	// Marker must NOT have been written.
	if _, err := os.Stat(filepath.Join(mount, ".y-cluster-seeded")); err == nil {
		t.Errorf("marker should not exist after conflict")
	}
}

// TestSeedCheck_LostFoundIgnored covers the freshly-formatted ext4
// case: lost+found exists but isn't customer data.
func TestSeedCheck_LostFoundIgnored(t *testing.T) {
	dir := t.TempDir()
	mount := filepath.Join(dir, "mount")
	if err := os.MkdirAll(filepath.Join(mount, "lost+found"), 0o755); err != nil {
		t.Fatal(err)
	}
	seed := filepath.Join(dir, "seed.tar.zst")
	meta := filepath.Join(dir, "seed.meta.json")
	makeSeedTar(t, seed, map[string]string{"hello.txt": "world"})
	if err := os.WriteFile(meta, []byte(`{"schemaVersion":1}`), 0o644); err != nil {
		t.Fatal(err)
	}

	_, _, exit := runSeedCheck(t, mount, seed, meta)
	if exit != 0 {
		t.Errorf("lost+found should be ignored; exit=%d", exit)
	}
}

// TestWriteSeedMeta_RoundTrip pins the JSON shape since it's the
// on-disk schema the customer's marker carries forward.
func TestWriteSeedMeta_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "meta.json")
	if err := writeSeedMeta(path, dataSeedMeta{
		SchemaVersion: SeedMetaSchemaVersion,
		SeededAt:      "2026-05-04T12:30:00Z",
		SeededBy:      "y-cluster v0.4.0 (abc1234)",
		ApplianceName: "appliance-test",
		SeedSHA256:    "sha256:c7e3",
	}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`"schemaVersion": 1`,
		`"seed_sha256": "sha256:c7e3"`,
		`"appliance_name": "appliance-test"`,
		`"seeded_by": "y-cluster v0.4.0 (abc1234)"`,
	} {
		if !strings.Contains(string(data), want) {
			t.Errorf("meta missing %q:\n%s", want, data)
		}
	}
}
