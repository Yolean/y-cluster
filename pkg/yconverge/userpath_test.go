package yconverge

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestUserPath_RelativeToCWD: a path under cwd renders as a
// short relative string -- the shape `-k <path>` accepts and
// the user can `cd` to.
func TestUserPath_RelativeToCWD(t *testing.T) {
	tmp := t.TempDir()
	t.Chdir(tmp)
	got := userPath(filepath.Join(tmp, "base"))
	if got != "base" {
		t.Fatalf("got %q, want %q", got, "base")
	}
}

// TestUserPath_TraversesUp: a path outside cwd produces the
// `../...` form. Long but still actionable in a shell.
func TestUserPath_TraversesUp(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "a/b"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "x"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(filepath.Join(root, "a/b"))
	got := userPath(filepath.Join(root, "x"))
	if !strings.HasPrefix(got, "..") {
		t.Fatalf("got %q, expected `..`-prefixed path", got)
	}
	if !strings.HasSuffix(got, "/x") {
		t.Fatalf("got %q, expected trailing /x", got)
	}
}
