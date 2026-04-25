package serve

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeConfig(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ConfigFilename), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadConfigDir_YKustomizeLocal(t *testing.T) {
	dir := t.TempDir()
	writeConfig(t, dir, `
port: 12345
type: y-kustomize-local
sources:
- dir: ./a
- dir: /abs/b
`)
	c, err := LoadConfigDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if c.Port != 12345 || c.Type != TypeYKustomizeLocal {
		t.Fatalf("got %+v", c)
	}
	if len(c.Sources) != 2 {
		t.Fatalf("sources: %v", c.Sources)
	}
	got := c.ResolvedSources()
	if got[0] != filepath.Join(c.Dir, "a") {
		t.Fatalf("relative not resolved against Dir: %s", got[0])
	}
	if got[1] != "/abs/b" {
		t.Fatalf("absolute source mangled: %s", got[1])
	}
}

func TestLoadConfigDir_Static(t *testing.T) {
	dir := t.TempDir()
	writeConfig(t, dir, `
port: 8080
type: static
static:
  dir: ./files
  root: /assets
  yamlToJson: true
  dirTrailingSlash: redirect
`)
	c, err := LoadConfigDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if c.Static == nil || !c.Static.YAMLToJSON || c.Static.DirTrailingSlash != "redirect" {
		t.Fatalf("static: %+v", c.Static)
	}
}

func TestLoadConfigDir_Errors(t *testing.T) {
	cases := []struct {
		name, body, wantSub string
	}{
		{"missing-port", "type: y-kustomize-local\nsources: [{dir: ./a}]\n", "port 0"},
		{"port-out-of-range", "port: 70000\ntype: y-kustomize-local\nsources: [{dir: ./a}]\n", "out of range"},
		{"missing-type", "port: 1\n", "type is required"},
		{"unknown-type", "port: 1\ntype: bogus\n", "unknown type"},
		{"unknown-field", "port: 1\ntype: y-kustomize-local\nsources: [{dir: ./a}]\nextra: x\n", "unknown field"},
		{"ykl-no-sources", "port: 1\ntype: y-kustomize-local\n", "at least one source"},
		{"ykl-empty-source-dir", "port: 1\ntype: y-kustomize-local\nsources: [{dir: ''}]\n", "dir is empty"},
		{"ykl-with-static", "port: 1\ntype: y-kustomize-local\nsources: [{dir: ./a}]\nstatic: {dir: ./x}\n", "static config not allowed"},
		{"static-no-block", "port: 1\ntype: static\n", "requires static block"},
		{"static-empty-dir", "port: 1\ntype: static\nstatic: {dir: ''}\n", "static.dir is empty"},
		{"static-with-sources", "port: 1\ntype: static\nstatic: {dir: ./a}\nsources: [{dir: ./b}]\n", "sources not allowed"},
		{"static-bad-trailing-slash", "port: 1\ntype: static\nstatic: {dir: ./a, dirTrailingSlash: strip}\n", "dirTrailingSlash"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeConfig(t, dir, tc.body)
			_, err := LoadConfigDir(dir)
			if err == nil || !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("want error containing %q, got %v", tc.wantSub, err)
			}
		})
	}
}

func TestLoadConfigDir_MissingDir(t *testing.T) {
	if _, err := LoadConfigDir(filepath.Join(t.TempDir(), "nope")); err == nil {
		t.Fatal("want error")
	}
}

func TestLoadConfigDir_NotADirectory(t *testing.T) {
	f := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfigDir(f); err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("want not-a-directory error, got %v", err)
	}
}

func TestLoadConfigDir_MissingFile(t *testing.T) {
	dir := t.TempDir()
	if _, err := LoadConfigDir(dir); err == nil {
		t.Fatal("want error for missing file")
	}
}

func TestLoadConfigDirs_DuplicatePort(t *testing.T) {
	a := t.TempDir()
	b := t.TempDir()
	writeConfig(t, a, "port: 9000\ntype: y-kustomize-local\nsources: [{dir: ./a}]\n")
	writeConfig(t, b, "port: 9000\ntype: y-kustomize-local\nsources: [{dir: ./a}]\n")
	if _, err := LoadConfigDirs([]string{a, b}); err == nil || !strings.Contains(err.Error(), "port 9000") {
		t.Fatalf("want duplicate-port error, got %v", err)
	}
}

func TestLoadConfigDirs_DedupSameDir(t *testing.T) {
	a := t.TempDir()
	writeConfig(t, a, "port: 9001\ntype: y-kustomize-local\nsources: [{dir: ./a}]\n")
	cfgs, err := LoadConfigDirs([]string{a, a})
	if err != nil {
		t.Fatal(err)
	}
	if len(cfgs) != 1 {
		t.Fatalf("dedup: %d", len(cfgs))
	}
}

func TestLoadConfigDirs_Empty(t *testing.T) {
	if _, err := LoadConfigDirs(nil); err == nil {
		t.Fatal("want error for empty dirs")
	}
}

func TestLoadConfigDirs_SortsByPort(t *testing.T) {
	a := t.TempDir()
	b := t.TempDir()
	writeConfig(t, a, "port: 9003\ntype: y-kustomize-local\nsources: [{dir: ./a}]\n")
	writeConfig(t, b, "port: 9002\ntype: y-kustomize-local\nsources: [{dir: ./a}]\n")
	cfgs, err := LoadConfigDirs([]string{a, b})
	if err != nil {
		t.Fatal(err)
	}
	if cfgs[0].Port != 9002 || cfgs[1].Port != 9003 {
		t.Fatalf("not sorted: %d %d", cfgs[0].Port, cfgs[1].Port)
	}
}

func TestDigest_Stable(t *testing.T) {
	a := t.TempDir()
	b := t.TempDir()
	writeConfig(t, a, "port: 8000\ntype: y-kustomize-local\nsources: [{dir: ./a}]\n")
	writeConfig(t, b, "port: 8001\ntype: y-kustomize-local\nsources: [{dir: ./a}]\n")
	cfgs1, _ := LoadConfigDirs([]string{a, b})
	cfgs2, _ := LoadConfigDirs([]string{b, a}) // reversed
	if Digest(cfgs1) != Digest(cfgs2) {
		t.Fatal("digest depends on order")
	}
}

func TestDigest_ChangesWhenSourceChanges(t *testing.T) {
	a := t.TempDir()
	writeConfig(t, a, "port: 8000\ntype: y-kustomize-local\nsources: [{dir: ./a}]\n")
	cfgs1, _ := LoadConfigDirs([]string{a})
	first := Digest(cfgs1)

	writeConfig(t, a, "port: 8000\ntype: y-kustomize-local\nsources: [{dir: ./b}]\n")
	cfgs2, _ := LoadConfigDirs([]string{a})
	if Digest(cfgs2) == first {
		t.Fatal("digest did not change when sources changed")
	}
}
