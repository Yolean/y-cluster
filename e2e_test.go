package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

var binPath string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "kustomize-traverse-bin-")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(dir)
	binPath = filepath.Join(dir, "kustomize-traverse")
	cmd := exec.Command("go", "build", "-o", binPath, ".")
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		panic("go build failed: " + err.Error())
	}
	os.Exit(m.Run())
}

func runBin(t *testing.T, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	cmd := exec.Command(binPath, args...)
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	err := cmd.Run()
	code = 0
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else {
			t.Fatalf("exec: %v", err)
		}
	}
	return outBuf.String(), errBuf.String(), code
}

func TestE2EDirsAndNamespace(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, root, map[string]string{
		"leaf/kustomization.yaml":    "namespace: dev\n",
		"mid/kustomization.yaml":     "resources:\n- ../leaf\n- extra.yaml\n- github.com/owner/repo//p?ref=v1\n",
		"mid/extra.yaml":             "kind: ConfigMap\n",
		"overlay/kustomization.yaml": "resources:\n- ../mid\n",
	})

	stdout, stderr, code := runBin(t, "-o", "dirs", filepath.Join(root, "overlay"))
	if code != 0 {
		t.Fatalf("dirs: exit=%d stderr=%s", code, stderr)
	}
	lines := strings.Split(strings.TrimRight(stdout, "\n"), "\n")
	want := []string{"../leaf", "../mid", "."}
	if !equalStrings(lines, want) {
		t.Fatalf("dirs: got %v want %v", lines, want)
	}

	stdout, _, code = runBin(t, "-o", "namespace", filepath.Join(root, "overlay"))
	if code != 0 || strings.TrimSpace(stdout) != "dev" {
		t.Fatalf("namespace: stdout=%q code=%d", stdout, code)
	}

	stdout, _, code = runBin(t, "--output", "json", filepath.Join(root, "overlay"))
	if code != 0 {
		t.Fatalf("json: code=%d", code)
	}
	if !strings.Contains(stdout, `"namespace": "dev"`) {
		t.Fatalf("json missing namespace: %s", stdout)
	}
	if !strings.Contains(stdout, `"../leaf"`) || !strings.Contains(stdout, `"../mid"`) {
		t.Fatalf("json missing dirs: %s", stdout)
	}
}

func TestE2EExitCodes(t *testing.T) {
	// exit 1: no kustomization file at <path>
	empty := t.TempDir()
	_, stderr, code := runBin(t, empty)
	if code != 1 {
		t.Fatalf("missing kustomization: expected 1, got %d (%s)", code, stderr)
	}
	if !strings.Contains(stderr, "no kustomization") {
		t.Fatalf("expected diagnostic, got %q", stderr)
	}

	// exit 2: missing path
	_, _, code = runBin(t)
	if code != 2 {
		t.Fatalf("missing arg: expected 2, got %d", code)
	}

	// exit 2: unknown flag
	_, _, code = runBin(t, "--nope", ".")
	if code != 2 {
		t.Fatalf("unknown flag: expected 2, got %d", code)
	}

	// exit 2: unknown output format
	root := t.TempDir()
	writeFiles(t, root, map[string]string{"k/kustomization.yaml": ""})
	_, _, code = runBin(t, "-o", "xml", filepath.Join(root, "k"))
	if code != 2 {
		t.Fatalf("unknown -o: expected 2, got %d", code)
	}
}

func TestE2EWarningsStreamToStderrOnly(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, root, map[string]string{
		"k/kustomization.yaml": "resources:\n- ../missing\n",
	})
	stdout, stderr, code := runBin(t, "-o", "dirs", filepath.Join(root, "k"))
	if code != 0 {
		t.Fatalf("exit=%d", code)
	}
	if strings.TrimSpace(stdout) != "." {
		t.Fatalf("stdout should be '.', got %q", stdout)
	}
	if !strings.Contains(stderr, "warning") {
		t.Fatalf("expected warning on stderr, got %q", stderr)
	}

	// -q silences it, stdout unchanged
	stdout, stderr, code = runBin(t, "-q", "-o", "dirs", filepath.Join(root, "k"))
	if code != 0 {
		t.Fatalf("exit=%d", code)
	}
	if strings.TrimSpace(stdout) != "." {
		t.Fatalf("quiet stdout got %q", stdout)
	}
	if strings.Contains(stderr, "warning") {
		t.Fatalf("unexpected warning with -q: %q", stderr)
	}
}
