package qemu

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

type seedCheckOpts struct {
	mount      string // -> $MOUNT
	seed       string // -> $SEED
	meta       string // -> $META
	bypass     string // -> $BYPASS_FLAG (defaults to a tmpdir non-existent path; create the file before running to exercise the bypass branch)
	forceMount bool   // when true, override mountpoint -q to always succeed (simulate "/data/yolean is a mountpoint")
}

// runSeedCheck executes the embedded data_seed_check.sh against
// caller-supplied paths. The boot-time script hardcodes
// /data/yolean / /var/lib/y-cluster / /run for production; tests
// override each path via sed substitution so we can exercise the
// real branches without root or a real mount.
func runSeedCheck(t *testing.T, opts seedCheckOpts) (stdout, stderr string, exit int) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("seed-check is /bin/sh-only")
	}
	if _, err := exec.LookPath("zstd"); err != nil {
		t.Skip("zstd not on PATH")
	}

	// Default the bypass path to something that won't exist unless
	// the test explicitly creates it. Tests that want to exercise
	// the bypass branch set opts.bypass to a path AND `touch` it.
	bypass := opts.bypass
	if bypass == "" {
		bypass = filepath.Join(t.TempDir(), "no-bypass-flag")
	}

	// Path substitutions on the production script. Each replacement
	// is anchored to the constant assignment line so a future
	// renaming of the literal doesn't silently break the test.
	src := dataSeedCheckScript
	src = strings.Replace(src, "MOUNT=/data/yolean", "MOUNT="+opts.mount, 1)
	src = strings.Replace(src, "SEED=/var/lib/y-cluster/data-seed.tar.zst", "SEED="+opts.seed, 1)
	src = strings.Replace(src, "META=/var/lib/y-cluster/data-seed.meta.json", "META="+opts.meta, 1)
	src = strings.Replace(src, "BYPASS_FLAG=/run/y-cluster-seed-bypass", "BYPASS_FLAG="+bypass, 1)

	// The mountpoint check uses `mountpoint -q` against MOUNT. tmp
	// dirs aren't mountpoints, so any test exercising "the mount IS
	// present" (states 1, 2, 5) needs to short-circuit the check.
	// We slip an env-var override in front of the original guard.
	if opts.forceMount {
		src = strings.Replace(src,
			`if ! mountpoint -q "$MOUNT" 2>/dev/null; then`,
			`if [ -z "${TEST_FORCE_MOUNT:-}" ] && ! mountpoint -q "$MOUNT" 2>/dev/null; then`,
			1)
	}

	scriptPath := filepath.Join(t.TempDir(), "seed-check.sh")
	if err := os.WriteFile(scriptPath, []byte(src), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/bin/sh", scriptPath)
	if opts.forceMount {
		cmd.Env = append(os.Environ(), "TEST_FORCE_MOUNT=1")
	}
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
// the entries (path -> body) given.
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

// fixture sets up a (mount, seed, meta) triple under t.TempDir()
// so individual tests stay focused on the assertion shape, not the
// boilerplate.
type fixture struct {
	dir   string
	mount string
	seed  string
	meta  string
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	dir := t.TempDir()
	f := fixture{
		dir:   dir,
		mount: filepath.Join(dir, "mount"),
		seed:  filepath.Join(dir, "seed.tar.zst"),
		meta:  filepath.Join(dir, "seed.meta.json"),
	}
	if err := os.MkdirAll(f.mount, 0o755); err != nil {
		t.Fatal(err)
	}
	return f
}

// State 1: volume attached, empty mount -> seed extracts, marker written.
func TestSeedCheck_EmptyMount_Seeds(t *testing.T) {
	f := newFixture(t)
	if err := os.MkdirAll(filepath.Join(f.mount, "lost+found"), 0o755); err != nil {
		t.Fatal(err)
	}
	makeSeedTar(t, f.seed, map[string]string{
		"workload-data/db.txt": "schema=v0.4.0",
	})
	if err := os.WriteFile(f.meta,
		[]byte(`{"schemaVersion":1,"seed_sha256":"sha256:fake"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	stdout, stderr, exit := runSeedCheck(t, seedCheckOpts{
		mount: f.mount, seed: f.seed, meta: f.meta, forceMount: true,
	})
	if exit != 0 {
		t.Fatalf("exit: got %d, want 0; stdout=%q stderr=%q", exit, stdout, stderr)
	}
	body, err := os.ReadFile(filepath.Join(f.mount, "workload-data/db.txt"))
	if err != nil {
		t.Fatalf("seed file should be extracted: %v", err)
	}
	if string(body) != "schema=v0.4.0" {
		t.Errorf("extracted body: got %q, want schema=v0.4.0", body)
	}
	markerBody, err := os.ReadFile(filepath.Join(f.mount, ".y-cluster-seeded"))
	if err != nil {
		t.Fatalf("marker should be written: %v", err)
	}
	if !strings.Contains(string(markerBody), "seed_sha256") {
		t.Errorf("marker should contain seed metadata: %s", markerBody)
	}
	// Bypass-sentinel must NOT exist in the production-mount path.
	if _, err := os.Stat(filepath.Join(f.mount, ".y-cluster-seeded-via-bypass")); err == nil {
		t.Errorf("bypass sentinel should not exist on a mounted-volume seed")
	}
}

// State 2: volume attached, has unmarked data -> conflict, no seed.
func TestSeedCheck_NonEmptyNoMarker_Conflict(t *testing.T) {
	f := newFixture(t)
	if err := os.WriteFile(filepath.Join(f.mount, "customer-stuff.txt"),
		[]byte("not ours"), 0o644); err != nil {
		t.Fatal(err)
	}
	makeSeedTar(t, f.seed, map[string]string{"x": "y"})
	if err := os.WriteFile(f.meta, []byte(`{"schemaVersion":1}`), 0o644); err != nil {
		t.Fatal(err)
	}

	stdout, stderr, exit := runSeedCheck(t, seedCheckOpts{
		mount: f.mount, seed: f.seed, meta: f.meta, forceMount: true,
	})
	if exit == 0 {
		t.Errorf("exit: got 0, want non-zero (conflict); stdout=%q", stdout)
	}
	if !strings.Contains(stderr, "refusing to seed") {
		t.Errorf("stderr should mention refusal: %s", stderr)
	}
	if !strings.Contains(stderr, "Resolution") {
		t.Errorf("stderr should include recovery recipes: %s", stderr)
	}
	body, _ := os.ReadFile(filepath.Join(f.mount, "customer-stuff.txt"))
	if string(body) != "not ours" {
		t.Errorf("customer file mutated: %q", body)
	}
	if _, err := os.Stat(filepath.Join(f.mount, ".y-cluster-seeded")); err == nil {
		t.Errorf("marker should not exist after conflict")
	}
}

// State 3: no volume, no bypass -> production gate fails closed.
// This is the regression posture for the customer-mounts-after-k3s
// race we hit on the GCP appliance.
func TestSeedCheck_NotMounted_NoBypass_Fails(t *testing.T) {
	f := newFixture(t)
	makeSeedTar(t, f.seed, map[string]string{"x": "y"})
	if err := os.WriteFile(f.meta, []byte(`{"schemaVersion":1}`), 0o644); err != nil {
		t.Fatal(err)
	}

	stdout, stderr, exit := runSeedCheck(t, seedCheckOpts{
		mount: f.mount, seed: f.seed, meta: f.meta,
		// forceMount: false -- the tmp dir really isn't a mountpoint.
	})
	if exit == 0 {
		t.Fatalf("exit: got 0, want non-zero (mount required); stdout=%q stderr=%q", stdout, stderr)
	}
	if !strings.Contains(stderr, "not a mountpoint") {
		t.Errorf("stderr should mention missing mountpoint: %s", stderr)
	}
	if !strings.Contains(stderr, "LABEL=y-cluster-data") {
		t.Errorf("stderr should reference the LABEL fstab convention: %s", stderr)
	}
	if _, err := os.Stat(filepath.Join(f.mount, ".y-cluster-seeded")); err == nil {
		t.Errorf("marker should not exist when mount-required gate fires")
	}
}

// State 4: no volume + bypass flag -> extract regardless of mount,
// drop sibling sentinel marking the bypass.
func TestSeedCheck_BypassFlag_Extracts(t *testing.T) {
	f := newFixture(t)
	bypass := filepath.Join(f.dir, "bypass-flag")
	if err := os.WriteFile(bypass, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	makeSeedTar(t, f.seed, map[string]string{"hello.txt": "world"})
	if err := os.WriteFile(f.meta, []byte(`{"schemaVersion":1}`), 0o644); err != nil {
		t.Fatal(err)
	}

	stdout, stderr, exit := runSeedCheck(t, seedCheckOpts{
		mount: f.mount, seed: f.seed, meta: f.meta, bypass: bypass,
		// forceMount: false on purpose -- the bypass branch must
		// short-circuit the mount-required gate.
	})
	if exit != 0 {
		t.Fatalf("exit: got %d, want 0; stdout=%q stderr=%q", exit, stdout, stderr)
	}
	if !strings.Contains(stdout, "bypass flag") {
		t.Errorf("stdout should announce bypass: %s", stdout)
	}
	body, err := os.ReadFile(filepath.Join(f.mount, "hello.txt"))
	if err != nil {
		t.Fatalf("seed should have been extracted in bypass mode: %v", err)
	}
	if string(body) != "world" {
		t.Errorf("extracted body: got %q, want world", body)
	}
	if _, err := os.Stat(filepath.Join(f.mount, ".y-cluster-seeded")); err != nil {
		t.Errorf("marker should be written even in bypass mode: %v", err)
	}
	if _, err := os.Stat(filepath.Join(f.mount, ".y-cluster-seeded-via-bypass")); err != nil {
		t.Errorf("bypass sentinel should be present: %v", err)
	}
}

// State 5: marker present -> upgrade fast path, no-op.
func TestSeedCheck_MarkerPresent_NoOp(t *testing.T) {
	f := newFixture(t)
	if err := os.WriteFile(filepath.Join(f.mount, ".y-cluster-seeded"),
		[]byte(`{"schemaVersion":1,"existing":"marker"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.mount, "existing-data.txt"),
		[]byte("customer's data"), 0o644); err != nil {
		t.Fatal(err)
	}

	stdout, _, exit := runSeedCheck(t, seedCheckOpts{
		mount: f.mount, seed: "/nonexistent/seed", meta: "/nonexistent/meta",
		forceMount: true,
	})
	if exit != 0 {
		t.Errorf("exit: got %d, want 0; stdout=%q", exit, stdout)
	}
	if !strings.Contains(stdout, "marker present") {
		t.Errorf("expected 'marker present' in stdout, got: %s", stdout)
	}
	body, _ := os.ReadFile(filepath.Join(f.mount, "existing-data.txt"))
	if string(body) != "customer's data" {
		t.Errorf("existing data mutated: %q", body)
	}
}

// State 6: lost+found ignored on freshly-formatted ext4. The kernel
// creates lost+found on every mkfs.ext4, so a "fresh empty" volume
// is actually never empty; the script must treat lost+found as
// non-content.
func TestSeedCheck_LostFoundIgnored(t *testing.T) {
	f := newFixture(t)
	if err := os.MkdirAll(filepath.Join(f.mount, "lost+found"), 0o755); err != nil {
		t.Fatal(err)
	}
	makeSeedTar(t, f.seed, map[string]string{"hello.txt": "world"})
	if err := os.WriteFile(f.meta, []byte(`{"schemaVersion":1}`), 0o644); err != nil {
		t.Fatal(err)
	}

	_, _, exit := runSeedCheck(t, seedCheckOpts{
		mount: f.mount, seed: f.seed, meta: f.meta, forceMount: true,
	})
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
