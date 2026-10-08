package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"slices"
	"testing"
)

func TestBuildctlGlobals(t *testing.T) {
	noGuest := func() (string, error) { return filepath.Join(t.TempDir(), "none"), nil }
	env := func(kv map[string]string) func(string) string {
		return func(k string) string { return kv[k] }
	}
	contract := map[string]string{"BUILDKIT_HOST": "tcp://10.88.1.2:8547", "BUILDKIT_TLS_DIR": "/c"}
	for _, tc := range []struct {
		name string
		args []string
		env  map[string]string
		want []string
	}{
		{"no guest, no env: buildctl's default", []string{"build"}, nil, nil},
		{"the env contract adds its tlsdir", []string{"build"}, contract, []string{"--tlsdir", "/c"}},
		{"an explicit tlsdir wins", []string{"--tlsdir=/x", "build"}, contract, nil},
		{"an explicit addr wins over all", []string{"--addr", "unix:///run/buildkit.sock", "build"}, contract, nil},
		{"single dash counts", []string{"-addr=tcp://b:1234", "build"}, contract, nil},
		{"after -- is not a flag", []string{"build", "--", "--addr"}, contract, []string{"--tlsdir", "/c"}},
		{"host without tlsdir", []string{"build"}, map[string]string{"BUILDKIT_HOST": "tcp://b:1234"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := buildctlGlobals(tc.args, env(tc.env), noGuest)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// Upstream's init functions set the process umask to 0; in y-cluster
// they must only run inside `y-cluster buildctl`.
func TestBuildctlCopyHasNoInit(t *testing.T) {
	files, err := filepath.Glob("../../pkg/buildctl/*.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("no pkg/buildctl sources: %v", err)
	}
	for _, f := range files {
		file, err := parser.ParseFile(token.NewFileSet(), f, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range file.Decls {
			if fn, ok := d.(*ast.FuncDecl); ok && fn.Recv == nil && fn.Name.Name == "init" {
				t.Errorf("%s has func init: rerun scripts/buildctl-refresh.sh, or extend it", f)
			}
		}
	}
}
