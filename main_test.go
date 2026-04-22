package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFiles(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, content := range files {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func runCLI(t *testing.T, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	var outBuf, errBuf bytes.Buffer
	code = run(args, &outBuf, &errBuf)
	return outBuf.String(), errBuf.String(), code
}

func TestDirsHappyPath(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, root, map[string]string{
		"base/kustomization.yaml": "namespace: dev\nresources:\n- deployment.yaml\n",
		"base/deployment.yaml":    "kind: Deployment\n",
		"overlay/kustomization.yaml": "resources:\n- ../base\n",
	})

	stdout, stderr, code := runCLI(t, "-o", "dirs", filepath.Join(root, "overlay"))
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, stderr)
	}
	got := strings.Split(strings.TrimRight(stdout, "\n"), "\n")
	want := []string{"../base", "."}
	if !equalStrings(got, want) {
		t.Fatalf("dirs: got %v want %v", got, want)
	}
}

func TestNamespaceOutermostWins(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, root, map[string]string{
		"base/kustomization.yaml":    "namespace: base-ns\n",
		"overlay/kustomization.yaml": "namespace: overlay-ns\nresources:\n- ../base\n",
	})
	stdout, _, code := runCLI(t, "-o", "namespace", filepath.Join(root, "overlay"))
	if code != 0 || strings.TrimSpace(stdout) != "overlay-ns" {
		t.Fatalf("got %q code=%d", stdout, code)
	}
}

func TestNamespaceFallsBackThroughSingleBase(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, root, map[string]string{
		"site-apply/kustomization.yaml":            "namespace: dev\n",
		"site-apply-namespaced/kustomization.yaml": "resources:\n- ../site-apply\n",
	})
	stdout, _, code := runCLI(t, "-o", "namespace", filepath.Join(root, "site-apply-namespaced"))
	if code != 0 || strings.TrimSpace(stdout) != "dev" {
		t.Fatalf("got %q code=%d", stdout, code)
	}
}

func TestNamespaceEmptyWhenUnresolvable(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, root, map[string]string{
		"a/kustomization.yaml": "",
		"b/kustomization.yaml": "",
		"root/kustomization.yaml": "resources:\n- ../a\n- ../b\n",
	})
	stdout, _, code := runCLI(t, "-o", "namespace", filepath.Join(root, "root"))
	if code != 0 {
		t.Fatalf("exit=%d", code)
	}
	if strings.TrimSpace(stdout) != "" {
		t.Fatalf("expected empty output, got %q", stdout)
	}
}

func TestComponentsAreTraversed(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, root, map[string]string{
		"comp/kustomization.yaml": "apiVersion: kustomize.config.k8s.io/v1alpha1\nkind: Component\n",
		"overlay/kustomization.yaml": "components:\n- ../comp\n",
	})
	stdout, _, code := runCLI(t, "-o", "dirs", filepath.Join(root, "overlay"))
	if code != 0 {
		t.Fatal(code)
	}
	got := strings.Split(strings.TrimRight(stdout, "\n"), "\n")
	want := []string{"../comp", "."}
	if !equalStrings(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestLegacyBasesField(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, root, map[string]string{
		"base/kustomization.yaml":    "namespace: legacy\n",
		"overlay/kustomization.yaml": "bases:\n- ../base\n",
	})
	stdout, _, code := runCLI(t, "-o", "namespace", filepath.Join(root, "overlay"))
	if code != 0 || strings.TrimSpace(stdout) != "legacy" {
		t.Fatalf("got %q code=%d", stdout, code)
	}
}

func TestDedupeAcrossOverlayAndBase(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, root, map[string]string{
		"shared/kustomization.yaml": "",
		"a/kustomization.yaml":      "resources:\n- ../shared\n",
		"b/kustomization.yaml":      "resources:\n- ../shared\n",
		"top/kustomization.yaml":    "resources:\n- ../a\n- ../b\n",
	})
	stdout, _, code := runCLI(t, "-o", "dirs", filepath.Join(root, "top"))
	if code != 0 {
		t.Fatal(code)
	}
	got := strings.Split(strings.TrimRight(stdout, "\n"), "\n")
	want := []string{"../shared", "../a", "../b", "."}
	if !equalStrings(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestDepthFirstBasesBeforeOverlay(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, root, map[string]string{
		"leaf/kustomization.yaml":   "",
		"mid/kustomization.yaml":    "resources:\n- ../leaf\n",
		"top/kustomization.yaml":    "resources:\n- ../mid\n",
	})
	stdout, _, code := runCLI(t, "-o", "dirs", filepath.Join(root, "top"))
	if code != 0 {
		t.Fatal(code)
	}
	got := strings.Split(strings.TrimRight(stdout, "\n"), "\n")
	want := []string{"../leaf", "../mid", "."}
	if !equalStrings(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestRemoteRefsSkippedSilently(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, root, map[string]string{
		"overlay/kustomization.yaml": strings.Join([]string{
			"resources:",
			"- github.com/owner/repo//path?ref=v1",
			"- https://example.com/thing.yaml",
			"- git@github.com:owner/repo.git",
		}, "\n") + "\n",
	})
	stdout, stderr, code := runCLI(t, "-o", "dirs", filepath.Join(root, "overlay"))
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, stderr)
	}
	if strings.TrimSpace(stdout) != "." {
		t.Fatalf("stdout=%q", stdout)
	}
	if strings.Contains(stderr, "warning") {
		t.Fatalf("remote refs should not warn: %s", stderr)
	}
}

func TestResourceFilesAreSkipped(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, root, map[string]string{
		"overlay/kustomization.yaml": "resources:\n- deployment.yaml\n- service.yaml\n",
		"overlay/deployment.yaml":    "kind: Deployment\n",
		"overlay/service.yaml":       "kind: Service\n",
	})
	stdout, stderr, code := runCLI(t, "-o", "dirs", filepath.Join(root, "overlay"))
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, stderr)
	}
	if strings.TrimSpace(stdout) != "." {
		t.Fatalf("stdout=%q", stdout)
	}
}

func TestMissingDirWarnsUnlessQuiet(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, root, map[string]string{
		"overlay/kustomization.yaml": "resources:\n- ../gone\n",
	})
	_, stderr, code := runCLI(t, "-o", "dirs", filepath.Join(root, "overlay"))
	if code != 0 {
		t.Fatalf("exit=%d", code)
	}
	if !strings.Contains(stderr, "warning") {
		t.Fatalf("expected warning, got %q", stderr)
	}
	_, stderr, code = runCLI(t, "-q", "-o", "dirs", filepath.Join(root, "overlay"))
	if code != 0 {
		t.Fatalf("exit=%d", code)
	}
	if strings.Contains(stderr, "warning") {
		t.Fatalf("expected no warning with -q, got %q", stderr)
	}
}

func TestJSONOutput(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, root, map[string]string{
		"base/kustomization.yaml":    "namespace: dev\n",
		"overlay/kustomization.yaml": "resources:\n- ../base\n",
	})
	stdout, _, code := runCLI(t, "-o", "json", filepath.Join(root, "overlay"))
	if code != 0 {
		t.Fatal(code)
	}
	var got struct {
		Namespace string   `json:"namespace"`
		Dirs      []string `json:"dirs"`
	}
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("not valid JSON: %v\n%s", err, stdout)
	}
	if got.Namespace != "dev" {
		t.Fatalf("namespace=%q", got.Namespace)
	}
	if !equalStrings(got.Dirs, []string{"../base", "."}) {
		t.Fatalf("dirs=%v", got.Dirs)
	}
}

func TestLongFlagsWork(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, root, map[string]string{
		"overlay/kustomization.yaml": "namespace: x\n",
	})
	stdout, _, code := runCLI(t, "--output", "namespace", "--quiet", filepath.Join(root, "overlay"))
	if code != 0 || strings.TrimSpace(stdout) != "x" {
		t.Fatalf("stdout=%q code=%d", stdout, code)
	}
}

func TestKustomizationYmlExtension(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, root, map[string]string{
		"overlay/kustomization.yml": "namespace: via-yml\n",
	})
	stdout, _, code := runCLI(t, "-o", "namespace", filepath.Join(root, "overlay"))
	if code != 0 || strings.TrimSpace(stdout) != "via-yml" {
		t.Fatalf("stdout=%q code=%d", stdout, code)
	}
}

func TestNoKustomizationFileExitsOne(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, stderr, code := runCLI(t, "-o", "dirs", filepath.Join(root, "empty"))
	if code != 1 {
		t.Fatalf("expected exit 1, got %d, stderr=%s", code, stderr)
	}
	if !strings.Contains(stderr, "no kustomization") {
		t.Fatalf("unexpected stderr: %q", stderr)
	}
}

func TestMissingPathArg(t *testing.T) {
	_, _, code := runCLI(t)
	if code != 2 {
		t.Fatalf("expected exit 2, got %d", code)
	}
}

func TestUnknownFlagExitsTwo(t *testing.T) {
	_, _, code := runCLI(t, "--nope", ".")
	if code != 2 {
		t.Fatalf("expected exit 2, got %d", code)
	}
}

func TestUnknownOutputFormatExitsTwo(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, root, map[string]string{
		"overlay/kustomization.yaml": "",
	})
	_, _, code := runCLI(t, "-o", "xml", filepath.Join(root, "overlay"))
	if code != 2 {
		t.Fatalf("expected exit 2, got %d", code)
	}
}

func TestInvalidYamlFails(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, root, map[string]string{
		"overlay/kustomization.yaml": ":\n  not valid\n\tcontent\n",
	})
	_, _, code := runCLI(t, "-o", "dirs", filepath.Join(root, "overlay"))
	if code != 1 {
		t.Fatalf("expected exit 1, got %d", code)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
