package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestManifestNameRE_Valid pins the names that the validation
// regex accepts. Anything in this list MUST round-trip from
// `y-cluster manifests add <name> ...` to a file at
// /var/lib/y-cluster/manifests-staging/<name>.yaml on the cluster
// node. The shape of the regex deliberately matches what kubectl
// accepts as a resource name so the manifest's filename and
// metadata.name typically line up.
func TestManifestNameRE_Valid(t *testing.T) {
	cases := []string{
		"a",
		"abc",
		"migrate-v0.5.0-userdb",
		"migrate-v0.5.0-userdb-add-tenants",
		"01-bootstrap",
		"x.y.z",
		"a_b",
	}
	for _, in := range cases {
		if !manifestNameRE.MatchString(in) {
			t.Errorf("manifestNameRE rejected valid name %q", in)
		}
	}
}

// TestManifestNameRE_Invalid pins the rejection set. The regex is
// the only safety belt before we write to the cluster node's
// filesystem -- a slip here turns into a path-traversal that
// writes outside the staging dir.
func TestManifestNameRE_Invalid(t *testing.T) {
	cases := []string{
		"",                // empty
		".hidden",         // leading dot
		"-leading-dash",   // leading dash
		"path/with/slash", // path separator
		"..",              // path traversal
		"../escape",       // path traversal
		"name with space", // whitespace
		"name\twith\ttab", // whitespace
		"name;rm -rf /",   // shell metacharacter
		"name`whoami`",    // shell metacharacter
		"name$HOME",       // shell metacharacter
		"name*",           // glob
		"name?",           // glob
	}
	for _, in := range cases {
		if manifestNameRE.MatchString(in) {
			t.Errorf("manifestNameRE accepted invalid name %q", in)
		}
	}
}

// TestManifestsAddCmd_Wired smokes that the cobra subcommand
// graph rejects the obvious bad-input paths before any cluster
// I/O happens. Doesn't hit a real cluster -- the important shape
// here is that bad <name> trips the regex check, so a typo
// can't reach RunShell.
func TestManifestsAddCmd_RejectsInvalidName(t *testing.T) {
	cmd := manifestsAddCmd()
	cmd.SetArgs([]string{"../escape", "/dev/null"})
	cmd.SetOut(&strings.Builder{})
	cmd.SetErr(&strings.Builder{})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for path-traversal name, got nil")
	}
	if !strings.Contains(err.Error(), "invalid manifest name") {
		t.Errorf("expected 'invalid manifest name' in error, got: %v", err)
	}
}

// TestManifestsReplaceCmd_RejectsInvalidName mirrors the add
// guard for the replace verb -- same regex, same fail-fast.
func TestManifestsReplaceCmd_RejectsInvalidName(t *testing.T) {
	cmd := manifestsReplaceCmd()
	cmd.SetArgs([]string{"../escape", "/dev/null"})
	cmd.SetOut(&strings.Builder{})
	cmd.SetErr(&strings.Builder{})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for path-traversal name, got nil")
	}
	if !strings.Contains(err.Error(), "invalid manifest name") {
		t.Errorf("expected 'invalid manifest name' in error, got: %v", err)
	}
}

// TestManifestsRmCmd_RejectsInvalidName guards the rm path too --
// rm takes <name> only (no file input) and we want the same
// regex check to fire before any RunShell.
func TestManifestsRmCmd_RejectsInvalidName(t *testing.T) {
	cmd := manifestsRmCmd()
	cmd.SetArgs([]string{"../escape"})
	cmd.SetOut(&strings.Builder{})
	cmd.SetErr(&strings.Builder{})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for path-traversal name, got nil")
	}
	if !strings.Contains(err.Error(), "invalid manifest name") {
		t.Errorf("expected 'invalid manifest name' in error, got: %v", err)
	}
}

// TestStagedManifestPath pins the on-cluster path the three
// verbs compute. The path is the single touchpoint between the
// build-side commands and the prepare-export step that moves
// these files onto the appliance disk; changing it accidentally
// would silently break the appliance flow.
func TestStagedManifestPath(t *testing.T) {
	got := stagedManifestPath("migrate-v0.5.0-userdb")
	want := "/var/lib/y-cluster/manifests-staging/migrate-v0.5.0-userdb.yaml"
	if got != want {
		t.Errorf("stagedManifestPath: got %q, want %q", got, want)
	}
}

// TestManifestsCmd_Subcommands pins the three-verb surface so a
// future refactor can't accidentally drop one. The strict-in-
// both-directions contract (add: must-not-exist, replace: must-
// exist, rm: must-exist) only works if all three exist; deleting
// any of them turns either a "create" or an "overwrite" into a
// silent --force.
func TestManifestsCmd_Subcommands(t *testing.T) {
	cmd := manifestsCmd()
	have := map[string]bool{}
	for _, sub := range cmd.Commands() {
		have[sub.Name()] = true
	}
	for _, want := range []string{"add", "replace", "rm"} {
		if !have[want] {
			t.Errorf("manifests subcommand missing: %q (have %v)", want, have)
		}
	}
}

// localShell is a nodeShell that runs on this machine, so the verbs'
// rules meet a real shell and a real file system.
func localShell(ctx context.Context, cmd string, stdin io.Reader, stdout, stderr io.Writer) error {
	c := exec.CommandContext(ctx, "sh", "-c", cmd)
	c.Stdin, c.Stdout, c.Stderr = stdin, stdout, stderr
	return c.Run()
}

func TestManifests_AddReplaceRemoveRules(t *testing.T) {
	ctx := context.Background()
	// A path that needs quoting, staging dir not created yet.
	target := filepath.Join(t.TempDir(), "staging dir", "migrate-v1.yaml")
	v1, v2 := []byte("kind: Job\nmetadata:\n  name: v1\n"), []byte("kind: Job\nmetadata:\n  name: v2\n")
	var out bytes.Buffer

	if err := stageReplace(ctx, localShell, &out, "m", target, v1); err == nil || !strings.Contains(err.Error(), "manifests add") {
		t.Fatalf("replace of a name that is not staged must point at add, got %v", err)
	}
	if err := stageRemove(ctx, localShell, &out, "m", target); err == nil || !strings.Contains(err.Error(), "nothing to remove") {
		t.Fatalf("rm of a name that is not staged must fail, got %v", err)
	}

	if err := stageAdd(ctx, localShell, &out, "m", target, v1); err != nil {
		t.Fatalf("add: %v", err)
	}
	if got, _ := os.ReadFile(target); !bytes.Equal(got, v1) {
		t.Fatalf("staged content: %q", got)
	}
	if info, _ := os.Stat(target); info.Mode().Perm() != 0o644 {
		t.Errorf("mode %v, want 0644", info.Mode().Perm())
	}

	out.Reset()
	if err := stageAdd(ctx, localShell, &out, "m", target, v1); err != nil || !strings.Contains(out.String(), "identical content; no change") {
		t.Fatalf("re-adding identical content is a no-op: err=%v out=%q", err, out.String())
	}
	if err := stageAdd(ctx, localShell, &out, "m", target, v2); err == nil || !strings.Contains(err.Error(), "manifests replace") {
		t.Fatalf("add over different content must point at replace, got %v", err)
	}
	if got, _ := os.ReadFile(target); !bytes.Equal(got, v1) {
		t.Fatalf("a refused add changed the staged file: %q", got)
	}

	if err := stageReplace(ctx, localShell, &out, "m", target, v2); err != nil {
		t.Fatalf("replace: %v", err)
	}
	if got, _ := os.ReadFile(target); !bytes.Equal(got, v2) {
		t.Fatalf("content after replace: %q", got)
	}
	if err := stageRemove(ctx, localShell, &out, "m", target); err != nil {
		t.Fatalf("rm: %v", err)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("file still there after rm: %v", err)
	}
}

// An empty staged file is present, not absent.
func TestReadStagedManifest_EmptyFileIsPresent(t *testing.T) {
	target := filepath.Join(t.TempDir(), "empty.yaml")
	if err := os.WriteFile(target, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	content, present, err := readStagedManifest(context.Background(), localShell, target)
	if err != nil || !present || len(content) != 0 {
		t.Fatalf("got content=%q present=%v err=%v", content, present, err)
	}
}

// When the node cannot be asked, nothing may be concluded. "Absent"
// used to be inferred from any failure of `test -e`, so a dropped ssh
// connection let add overwrite a manifest it could not see.
func TestManifests_TransportFailureIsNotAbsence(t *testing.T) {
	wrote := false
	broken := func(_ context.Context, cmd string, _ io.Reader, _, _ io.Writer) error {
		if strings.Contains(cmd, "install ") {
			wrote = true
		}
		return errors.New("ssh: connect to host 127.0.0.1 port 2222: connection refused")
	}
	var out bytes.Buffer
	err := stageAdd(context.Background(), broken, &out, "m", "/var/lib/y-cluster/manifests-staging/m.yaml", []byte("x"))
	if err == nil || !strings.Contains(err.Error(), "connection refused") {
		t.Fatalf("want the transport error, got %v", err)
	}
	if wrote {
		t.Fatal("add went on to write although it could not read the node")
	}
	if err := stageRemove(context.Background(), broken, &out, "m", "/x/m.yaml"); err == nil || strings.Contains(err.Error(), "nothing to remove") {
		t.Fatalf("rm must report the transport error, not \"not staged\": %v", err)
	}
}
