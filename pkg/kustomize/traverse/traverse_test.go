package traverse

import (
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

func relDirs(t *testing.T, result *Result, root string) []string {
	t.Helper()
	rels, err := result.RelDirs(root)
	if err != nil {
		t.Fatal(err)
	}
	return rels
}

func TestWalk_DirsHappyPath(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, root, map[string]string{
		"base/kustomization.yaml":    "namespace: dev\nresources:\n- deployment.yaml\n",
		"base/deployment.yaml":       "kind: Deployment\n",
		"overlay/kustomization.yaml": "resources:\n- ../base\n",
	})

	result, err := Walk(filepath.Join(root, "overlay"), nil)
	if err != nil {
		t.Fatal(err)
	}
	got := relDirs(t, result, filepath.Join(root, "overlay"))
	want := []string{"../base", "."}
	if !equalStrings(got, want) {
		t.Fatalf("dirs: got %v want %v", got, want)
	}
}

func TestWalk_NamespaceOutermostWins(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, root, map[string]string{
		"base/kustomization.yaml":    "namespace: base-ns\n",
		"overlay/kustomization.yaml": "namespace: overlay-ns\nresources:\n- ../base\n",
	})
	result, err := Walk(filepath.Join(root, "overlay"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Namespace != "overlay-ns" {
		t.Fatalf("namespace=%q want overlay-ns", result.Namespace)
	}
}

func TestWalk_NamespaceFallsBackThroughSingleBase(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, root, map[string]string{
		"site-apply/kustomization.yaml":            "namespace: dev\n",
		"site-apply-namespaced/kustomization.yaml": "resources:\n- ../site-apply\n",
	})
	result, err := Walk(filepath.Join(root, "site-apply-namespaced"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Namespace != "dev" {
		t.Fatalf("namespace=%q want dev", result.Namespace)
	}
}

func TestWalk_NamespaceEmptyWhenMultipleBases(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, root, map[string]string{
		"a/kustomization.yaml":    "",
		"b/kustomization.yaml":    "",
		"root/kustomization.yaml": "resources:\n- ../a\n- ../b\n",
	})
	result, err := Walk(filepath.Join(root, "root"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Namespace != "" {
		t.Fatalf("expected empty namespace, got %q", result.Namespace)
	}
}

func TestWalk_ComponentsAreTraversed(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, root, map[string]string{
		"comp/kustomization.yaml":    "apiVersion: kustomize.config.k8s.io/v1alpha1\nkind: Component\n",
		"overlay/kustomization.yaml": "components:\n- ../comp\n",
	})
	result, err := Walk(filepath.Join(root, "overlay"), nil)
	if err != nil {
		t.Fatal(err)
	}
	got := relDirs(t, result, filepath.Join(root, "overlay"))
	want := []string{"../comp", "."}
	if !equalStrings(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestWalk_LegacyBasesField(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, root, map[string]string{
		"base/kustomization.yaml":    "namespace: legacy\n",
		"overlay/kustomization.yaml": "bases:\n- ../base\n",
	})
	result, err := Walk(filepath.Join(root, "overlay"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Namespace != "legacy" {
		t.Fatalf("namespace=%q want legacy", result.Namespace)
	}
}

func TestWalk_DedupeAcrossOverlayAndBase(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, root, map[string]string{
		"shared/kustomization.yaml": "",
		"a/kustomization.yaml":     "resources:\n- ../shared\n",
		"b/kustomization.yaml":     "resources:\n- ../shared\n",
		"top/kustomization.yaml":   "resources:\n- ../a\n- ../b\n",
	})
	result, err := Walk(filepath.Join(root, "top"), nil)
	if err != nil {
		t.Fatal(err)
	}
	got := relDirs(t, result, filepath.Join(root, "top"))
	want := []string{"../shared", "../a", "../b", "."}
	if !equalStrings(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestWalk_DepthFirstBasesBeforeOverlay(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, root, map[string]string{
		"leaf/kustomization.yaml": "",
		"mid/kustomization.yaml":  "resources:\n- ../leaf\n",
		"top/kustomization.yaml":  "resources:\n- ../mid\n",
	})
	result, err := Walk(filepath.Join(root, "top"), nil)
	if err != nil {
		t.Fatal(err)
	}
	got := relDirs(t, result, filepath.Join(root, "top"))
	want := []string{"../leaf", "../mid", "."}
	if !equalStrings(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestWalk_RemoteRefsSkippedSilently(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, root, map[string]string{
		"overlay/kustomization.yaml": strings.Join([]string{
			"resources:",
			"- github.com/owner/repo//path?ref=v1",
			"- https://example.com/thing.yaml",
			"- git@github.com:owner/repo.git",
		}, "\n") + "\n",
	})
	var warnings []string
	warn := func(format string, a ...any) {
		warnings = append(warnings, format)
	}
	result, err := Walk(filepath.Join(root, "overlay"), warn)
	if err != nil {
		t.Fatal(err)
	}
	got := relDirs(t, result, filepath.Join(root, "overlay"))
	if len(got) != 1 || got[0] != "." {
		t.Fatalf("got %v, want [.]", got)
	}
	if len(warnings) > 0 {
		t.Fatalf("remote refs should not warn: %v", warnings)
	}
}

func TestWalk_ResourceFilesAreSkipped(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, root, map[string]string{
		"overlay/kustomization.yaml": "resources:\n- deployment.yaml\n- service.yaml\n",
		"overlay/deployment.yaml":    "kind: Deployment\n",
		"overlay/service.yaml":       "kind: Service\n",
	})
	result, err := Walk(filepath.Join(root, "overlay"), nil)
	if err != nil {
		t.Fatal(err)
	}
	got := relDirs(t, result, filepath.Join(root, "overlay"))
	if len(got) != 1 || got[0] != "." {
		t.Fatalf("got %v, want [.]", got)
	}
}

func TestWalk_MissingDirWarns(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, root, map[string]string{
		"overlay/kustomization.yaml": "resources:\n- ../gone\n",
	})
	var warnings []string
	warn := func(format string, a ...any) {
		warnings = append(warnings, format)
	}
	result, err := Walk(filepath.Join(root, "overlay"), warn)
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) == 0 {
		t.Fatal("expected warning for missing dir")
	}
	got := relDirs(t, result, filepath.Join(root, "overlay"))
	if len(got) != 1 || got[0] != "." {
		t.Fatalf("got %v", got)
	}
}

func TestWalk_MissingDirSilentWithNilWarn(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, root, map[string]string{
		"overlay/kustomization.yaml": "resources:\n- ../gone\n",
	})
	result, err := Walk(filepath.Join(root, "overlay"), nil)
	if err != nil {
		t.Fatal(err)
	}
	got := relDirs(t, result, filepath.Join(root, "overlay"))
	if len(got) != 1 || got[0] != "." {
		t.Fatalf("got %v", got)
	}
}

func TestWalk_NoKustomizationFile(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := Walk(filepath.Join(root, "empty"), nil)
	if err == nil {
		t.Fatal("expected error for missing kustomization")
	}
	if !strings.Contains(err.Error(), "no kustomization") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestWalk_InvalidYaml(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, root, map[string]string{
		"overlay/kustomization.yaml": ":\n  not valid\n\tcontent\n",
	})
	_, err := Walk(filepath.Join(root, "overlay"), nil)
	if err == nil {
		t.Fatal("expected error for invalid YAML")
	}
}

func TestWalk_KustomizationYmlExtension(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, root, map[string]string{
		"overlay/kustomization.yml": "namespace: via-yml\n",
	})
	result, err := Walk(filepath.Join(root, "overlay"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Namespace != "via-yml" {
		t.Fatalf("namespace=%q want via-yml", result.Namespace)
	}
}

func TestIsRemote(t *testing.T) {
	tests := []struct {
		entry  string
		remote bool
	}{
		{"../base", false},
		{"./local", false},
		{"deployment.yaml", false},
		{".hidden/dir", false},
		{"github.com/owner/repo//path?ref=v1", true},
		{"https://example.com/thing.yaml", true},
		{"http://y-kustomize.svc/v1/base.yaml", true},
		{"git@github.com:owner/repo.git", true},
		{"git://example.com/repo", true},
	}
	for _, tt := range tests {
		t.Run(tt.entry, func(t *testing.T) {
			if got := IsRemote(tt.entry); got != tt.remote {
				t.Errorf("IsRemote(%q) = %v, want %v", tt.entry, got, tt.remote)
			}
		})
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
