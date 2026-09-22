package qemu

import (
	"errors"
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

// Only a guest without /data/yolean may ship without a seed. The
// stderr text is what libguestfs 1.5x prints for that case.
func TestTarOutError(t *testing.T) {
	exit1 := errors.New("exit status 1")

	missing := tarOutError("*stdin*:0: libguestfs: error: tar_out: stat: /data/yolean: No such file or directory\n", exit1)
	if !errors.Is(missing, ErrNoDataDir) {
		t.Errorf("missing source dir must classify as ErrNoDataDir: %v", missing)
	}

	for name, stderr := range map[string]string{
		"supermin":      "libguestfs: error: /usr/bin/supermin exited with error status 1.\n",
		"other path":    "libguestfs: error: tar_out: stat: /data: No such file or directory\n",
		"no diagnostic": "",
	} {
		got := tarOutError(stderr, exit1)
		if errors.Is(got, ErrNoDataDir) {
			t.Errorf("%s: must not be treated as \"nothing to seed\": %v", name, got)
		}
		if !errors.Is(got, exit1) {
			t.Errorf("%s: cause lost: %v", name, got)
		}
	}
}

// printedRecipe returns the shell commands the conflict message
// prints under one resolution label, minus what only makes sense on
// the appliance: sudo, and the unit restart (the tests rerun the
// script instead). What the customer is told to type is what runs.
func printedRecipe(t *testing.T, stderr, label string) string {
	t.Helper()
	var cmds []string
	in := false
	for _, line := range strings.Split(stderr, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "(") {
			in = strings.HasPrefix(trimmed, label)
			continue
		}
		// Commands are indented by ten, prose by less.
		if !in || !strings.HasPrefix(line, "          ") {
			continue
		}
		if strings.HasPrefix(trimmed, "#") || strings.Contains(trimmed, "systemctl restart") {
			continue
		}
		cmds = append(cmds, strings.ReplaceAll(line, "sudo ", ""))
	}
	if len(cmds) == 0 {
		t.Fatalf("no commands under %s in:\n%s", label, stderr)
	}
	return strings.Join(cmds, "\n")
}

// conflictFixture is a mounted volume with a file the seed did not
// put there, which is what makes the unit refuse.
func conflictFixture(t *testing.T) (f fixture, stderr string) {
	t.Helper()
	f = newFixture(t)
	if err := os.WriteFile(filepath.Join(f.mount, "restored.txt"), []byte("from backup"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.mount, ".hidden"), []byte("dotfile"), 0o644); err != nil {
		t.Fatal(err)
	}
	makeSeedTar(t, f.seed, map[string]string{"from-seed.txt": "seeded"})
	if err := os.WriteFile(f.meta, []byte(`{"schemaVersion":1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, stderr, exit := runSeedCheck(t, seedCheckOpts{mount: f.mount, seed: f.seed, meta: f.meta, forceMount: true})
	if exit == 0 {
		t.Fatal("setup: unmarked contents should be a conflict")
	}
	return f, stderr
}

// Recovery recipe (a): the customer restored their own data and
// marks it as seeded. The unit then starts k3s on that data and
// extracts nothing over it.
func TestSeedCheck_RecipeMarkExistingData(t *testing.T) {
	f, stderr := conflictFixture(t)
	recipe := printedRecipe(t, stderr, "(a)")
	if out, err := exec.Command("/bin/sh", "-c", recipe).CombinedOutput(); err != nil {
		t.Fatalf("recipe failed: %s: %v\n%s", out, err, recipe)
	}

	stdout, stderr, exit := runSeedCheck(t, seedCheckOpts{mount: f.mount, seed: f.seed, meta: f.meta, forceMount: true})
	if exit != 0 {
		t.Fatalf("after recipe (a) the unit still fails (%d): %s%s", exit, stdout, stderr)
	}
	if body, _ := os.ReadFile(filepath.Join(f.mount, "restored.txt")); string(body) != "from backup" {
		t.Errorf("restored data changed: %q", body)
	}
	if _, err := os.Stat(filepath.Join(f.mount, "from-seed.txt")); err == nil {
		t.Error("the seed was extracted over data the customer marked as correct")
	}
}

// Recovery recipe (b): the contents are junk, wipe and seed afresh.
// The printed glob has to take dotfiles with it, or the rerun is a
// conflict again.
func TestSeedCheck_RecipeWipeAndReseed(t *testing.T) {
	f, stderr := conflictFixture(t)
	recipe := printedRecipe(t, stderr, "(b)")
	// This test runs an rm -rf it did not write. Everything it may
	// touch is under the fixture's mount.
	for _, word := range strings.Fields(recipe) {
		if strings.HasPrefix(word, "/") && !strings.HasPrefix(word, f.mount+"/") {
			t.Fatalf("recipe reaches outside %s: %s", f.mount, recipe)
		}
	}
	if out, err := exec.Command("/bin/sh", "-c", recipe).CombinedOutput(); err != nil {
		t.Fatalf("recipe failed: %s: %v\n%s", out, err, recipe)
	}

	stdout, stderr, exit := runSeedCheck(t, seedCheckOpts{mount: f.mount, seed: f.seed, meta: f.meta, forceMount: true})
	if exit != 0 {
		t.Fatalf("after recipe (b) the unit still fails (%d): %s%s", exit, stdout, stderr)
	}
	if body, _ := os.ReadFile(filepath.Join(f.mount, "from-seed.txt")); string(body) != "seeded" {
		t.Errorf("seed content missing after re-seed: %q", body)
	}
	for _, gone := range []string{"restored.txt", ".hidden"} {
		if _, err := os.Stat(filepath.Join(f.mount, gone)); err == nil {
			t.Errorf("%s survived the wipe", gone)
		}
	}
	if _, err := os.Stat(filepath.Join(f.mount, ".y-cluster-seeded")); err != nil {
		t.Errorf("no marker after re-seed: %v", err)
	}
}

// A damaged seed must not become a partial extract that gets marked
// as seeded: the pipe hides zstdcat's exit status, and tar accepts a
// stream that ends on a member boundary. Nothing is extracted, so
// every later boot fails the same way until the disk is replaced.
func TestSeedCheck_DamagedSeedExtractsNothing(t *testing.T) {
	f := newFixture(t)
	entries := map[string]string{}
	for i := 0; i < 200; i++ {
		entries[filepath.Join("d", "file-"+strings.Repeat("n", i))] = strings.Repeat("payload ", 400)
	}
	makeSeedTar(t, f.seed, entries)
	if err := os.WriteFile(f.meta, []byte(`{"schemaVersion":1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	whole, err := os.ReadFile(f.seed)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.seed, whole[:len(whole)/2], 0o644); err != nil {
		t.Fatal(err)
	}

	opts := seedCheckOpts{mount: f.mount, seed: f.seed, meta: f.meta, forceMount: true}
	for boot := 1; boot <= 2; boot++ {
		stdout, stderr, exit := runSeedCheck(t, opts)
		if exit == 0 {
			t.Fatalf("boot %d: half a seed extracted as if it were whole: %s", boot, stdout)
		}
		if !strings.Contains(stderr, "damaged") {
			t.Errorf("boot %d: stderr should say the seed is damaged: %s", boot, stderr)
		}
		left, err := os.ReadDir(f.mount)
		if err != nil {
			t.Fatal(err)
		}
		if len(left) != 0 {
			t.Fatalf("boot %d: %d entries extracted from a damaged seed", boot, len(left))
		}
	}
}

// The marker is written last: an extract that died halfway (power
// loss) leaves contents without a marker, and the next boot refuses
// them as a conflict rather than taking the volume for seeded.
func TestSeedCheck_InterruptedExtractIsAConflict(t *testing.T) {
	f := newFixture(t)
	makeSeedTar(t, f.seed, map[string]string{"a.txt": "a", "b.txt": "b"})
	if err := os.WriteFile(f.meta, []byte(`{"schemaVersion":1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	// What the volume looks like after tar got as far as a.txt.
	if err := os.WriteFile(filepath.Join(f.mount, "a.txt"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, stderr, exit := runSeedCheck(t, seedCheckOpts{mount: f.mount, seed: f.seed, meta: f.meta, forceMount: true})
	if exit == 0 {
		t.Fatal("a half-extracted volume passed for seeded")
	}
	if !strings.Contains(stderr, "refusing to seed") {
		t.Errorf("want the conflict message, got: %s", stderr)
	}
	if _, err := os.Stat(filepath.Join(f.mount, "b.txt")); err == nil {
		t.Error("the unit extracted over a half-extracted volume")
	}

	// The ordering that makes the above true.
	extract := strings.Index(dataSeedCheckScript, `tar -C "$MOUNT" -xpf -`)
	marker := strings.Index(dataSeedCheckScript, `cp "$META" "$MARKER"`)
	if extract < 0 || marker < 0 || marker < extract {
		t.Errorf("the marker must be written after the extract (extract at %d, marker at %d)", extract, marker)
	}
}

// runSeedStatus runs the customer-facing status helper against a
// fixture, with the same path substitution runSeedCheck uses.
func runSeedStatus(t *testing.T, f fixture) (output string, exit int) {
	t.Helper()
	src := seedStatusScript
	for from, to := range map[string]string{
		"MOUNT=/data/yolean":                          "MOUNT=" + f.mount,
		"SEED=/var/lib/y-cluster/data-seed.tar.zst":   "SEED=" + f.seed,
		"META=/var/lib/y-cluster/data-seed.meta.json": "META=" + f.meta,
	} {
		if !strings.Contains(src, from) {
			t.Fatalf("seed_status.sh no longer assigns %q", from)
		}
		src = strings.Replace(src, from, to, 1)
	}
	path := filepath.Join(t.TempDir(), "seed-status.sh")
	if err := os.WriteFile(path, []byte(src), 0o755); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("/bin/sh", path).CombinedOutput()
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		exit = exitErr.ExitCode()
	} else if err != nil {
		t.Fatalf("run seed-status: %v", err)
	}
	return string(out), exit
}

// y-cluster-seed-status is what a customer is told to run when the
// appliance does not come up. It has to tell the states apart, and it
// has to finish: it runs on a machine where k3s is down and the seed
// unit has failed.
func TestSeedStatus_ReportsConflictThenSeeded(t *testing.T) {
	f, _ := conflictFixture(t)

	out, exit := runSeedStatus(t, f)
	if exit != 0 {
		t.Errorf("exit %d on a machine in conflict mode:\n%s", exit, out)
	}
	for _, want := range []string{"ABSENT.", filepath.Join(f.mount, "restored.txt"), "Recovery recipes", `{"schemaVersion":1}`} {
		if !strings.Contains(out, want) {
			t.Errorf("conflict-mode status lacks %q:\n%s", want, out)
		}
	}

	// The recipe it prints is the one that works (recipe (a) of the
	// seed unit, run by TestSeedCheck_RecipeMarkExistingData).
	wantRecipe := `echo '{"schemaVersion":1,"manuallyMarked":true}' | sudo tee ` + filepath.Join(f.mount, ".y-cluster-seeded")
	if !strings.Contains(out, wantRecipe) {
		t.Errorf("status prints a different mark-as-seeded recipe than the seed unit:\n%s", out)
	}

	if err := os.WriteFile(filepath.Join(f.mount, ".y-cluster-seeded"), []byte(`{"schemaVersion":1,"manuallyMarked":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	out, exit = runSeedStatus(t, f)
	if exit != 0 {
		t.Errorf("exit %d on a seeded machine:\n%s", exit, out)
	}
	if !strings.Contains(out, "PRESENT:") || !strings.Contains(out, `"manuallyMarked":true`) {
		t.Errorf("seeded status should show the marker:\n%s", out)
	}
}
