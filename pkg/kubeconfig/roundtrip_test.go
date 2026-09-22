package kubeconfig

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"sigs.k8s.io/yaml"
)

// foreignKubeconfig is an operator's kubeconfig that uses fields
// y-cluster's types do not model, on entries y-cluster does not own.
const foreignKubeconfig = `apiVersion: v1
kind: Config
current-context: work
preferences:
  colors: true
extensions:
- name: kubie
  extension:
    last-context: work
clusters:
- name: work
  cluster:
    server: https://work.example.test:6443
    proxy-url: socks5://127.0.0.1:1080
    tls-server-name: kube.internal
    disable-compression: true
    extensions:
    - name: minikube
      extension:
        provider: none
contexts:
- name: work
  context:
    cluster: work
    user: work
    namespace: team-a
    extensions:
    - name: kubie
      extension:
        shell: zsh
users:
- name: work
  user:
    as: deployer
    as-groups: [system:masters, qa]
    as-uid: "1234"
    as-user-extra:
      reason: [audit]
    exec:
      apiVersion: client.authentication.k8s.io/v1
      command: gke-gcloud-auth-plugin
      interactiveMode: IfAvailable
`

const k3sKubeconfig = `apiVersion: v1
kind: Config
current-context: default
clusters:
- name: default
  cluster:
    server: https://127.0.0.1:6443
    certificate-authority-data: Q0E=
contexts:
- name: default
  context:
    cluster: default
    user: default
users:
- name: default
  user:
    client-certificate-data: Q0VSVA==
    client-key-data: S0VZ
`

func generic(t *testing.T, data []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := yaml.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// entry returns the named element of one of the clusters / contexts /
// users lists.
func entry(t *testing.T, doc map[string]any, list, name string) any {
	t.Helper()
	items, _ := doc[list].([]any)
	for _, it := range items {
		if m, ok := it.(map[string]any); ok && m["name"] == name {
			return m
		}
	}
	t.Fatalf("no %s entry named %q", list, name)
	return nil
}

// Import and cleanup touch y-cluster's own entries only. Everything
// else in the operator's file has to come back as it was, including
// fields these types have never heard of.
func TestRoundTrip_PreservesWhatItDoesNotOwn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(path, []byte(foreignKubeconfig), 0o600); err != nil {
		t.Fatal(err)
	}
	want := generic(t, []byte(foreignKubeconfig))

	m, err := New(path, "local", "y-cluster", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Import([]byte(k3sKubeconfig)); err != nil {
		t.Fatal(err)
	}
	afterImport, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got := generic(t, afterImport)
	for _, list := range []string{"clusters", "contexts", "users"} {
		if !reflect.DeepEqual(entry(t, got, list, "work"), entry(t, want, list, "work")) {
			t.Errorf("%s entry \"work\" changed by Import:\ngot  %v\nwant %v", list, entry(t, got, list, "work"), entry(t, want, list, "work"))
		}
		entry(t, got, list, map[string]string{"clusters": "y-cluster", "contexts": "local", "users": "y-cluster"}[list])
	}
	for _, key := range []string{"extensions", "preferences"} {
		if !reflect.DeepEqual(got[key], want[key]) {
			t.Errorf("top-level %s changed by Import:\ngot  %v\nwant %v", key, got[key], want[key])
		}
	}

	m.CleanupStale()
	afterCleanup, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got = generic(t, afterCleanup)
	// Import made the new context current and cleanup unset it; the
	// rest of the document is the operator's original.
	delete(got, "current-context")
	delete(want, "current-context")
	if !reflect.DeepEqual(got, want) {
		t.Errorf("import + cleanup did not give the operator's kubeconfig back:\ngot  %v\nwant %v", got, want)
	}
}

// A teardown of a cluster that has no entries in the kubeconfig must
// not rewrite the file, and must not create one.
func TestCleanupStale_LeavesUnrelatedKubeconfigAlone(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "kubeconfig")
	if err := os.WriteFile(path, []byte(foreignKubeconfig), 0o600); err != nil {
		t.Fatal(err)
	}
	m, _ := New(path, "local", "y-cluster", nil)
	m.CleanupStale()
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != foreignKubeconfig {
		t.Errorf("file was rewritten although nothing was removed:\n%s", after)
	}

	missing := filepath.Join(dir, "nope", "kubeconfig")
	m2, _ := New(missing, "local", "y-cluster", nil)
	m2.CleanupStale()
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Errorf("cleanup created %s", missing)
	}
}

func TestSave_LeavesNoTempFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "kubeconfig")
	if err := emptyFile().Save(path); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 || entries[0].Name() != "kubeconfig" {
		t.Errorf("unexpected directory content after Save: %v", entries)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Errorf("mode %v, want 0600", info.Mode().Perm())
	}
}

// ystack keeps a read-only ~/.kube/config so that tools cannot write
// clusters into it. A rename needs no permission on the file itself,
// so the atomic write has to refuse on its own.
func TestSave_RefusesReadOnlyKubeconfig(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root may write anywhere")
	}
	path := filepath.Join(t.TempDir(), "config")
	const dummy = "# read-only on purpose\n"
	if err := os.WriteFile(path, []byte(dummy), 0o444); err != nil {
		t.Fatal(err)
	}
	if err := emptyFile().Save(path); err == nil {
		t.Fatal("Save replaced a read-only kubeconfig")
	}
	if after, _ := os.ReadFile(path); string(after) != dummy {
		t.Errorf("read-only kubeconfig was modified: %q", after)
	}
}

func TestSave_KeepsSymlink(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real-kubeconfig")
	link := filepath.Join(dir, "kubeconfig")
	if err := os.WriteFile(real, []byte("apiVersion: v1\nkind: Config\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	f := emptyFile()
	f.CurrentContext = "marker"
	if err := f.Save(link); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Lstat(link); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("the symlink was replaced by a regular file (err=%v)", err)
	}
	if data, _ := os.ReadFile(real); !strings.Contains(string(data), "marker") {
		t.Errorf("the symlink's target was not updated: %s", data)
	}
}

// Parallel provisions each load, modify and save the same file.
// Without the lock the slower one writes back a copy that lacks the
// other's entries.
func TestImport_ConcurrentImportsKeepEveryContext(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kubeconfig")
	const n = 12
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			m, err := New(path, fmt.Sprintf("ctx-%d", i), fmt.Sprintf("cluster-%d", i), nil)
			if err != nil {
				errs <- err
				return
			}
			errs <- m.Import([]byte(k3sKubeconfig))
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	f, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Contexts) != n || len(f.Clusters) != n || len(f.Users) != n {
		t.Fatalf("lost entries under concurrency: %d contexts, %d clusters, %d users; want %d each",
			len(f.Contexts), len(f.Clusters), len(f.Users), n)
	}
}
