package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Yolean/y-cluster/pkg/kubeconfig"
)

// k3s.yaml as k3s writes it, trimmed to what the import touches.
const nodeK3sYAML = `apiVersion: v1
kind: Config
clusters:
- cluster:
    certificate-authority-data: Q0E=
    server: https://127.0.0.1:6443
  name: default
contexts:
- context:
    cluster: default
    user: default
  name: default
current-context: default
users:
- name: default
  user:
    client-certificate-data: Q0VSVA==
    client-key-data: S0VZ
`

func TestKubeconfigImport(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kubeconfig")
	t.Setenv("KUBECONFIG", path)
	root := rootCmd()
	root.SetArgs([]string{"kubeconfig", "import", "--context", "edge", "--server", "https://127.0.0.1:16443"})
	root.SetIn(strings.NewReader(nodeK3sYAML))
	root.SetOut(&bytes.Buffer{})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	f, err := kubeconfig.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := os.ReadFile(path)
	if len(f.Contexts) != 1 || f.Contexts[0].Name != "edge" || !strings.Contains(string(body), "server: https://127.0.0.1:16443") {
		t.Errorf("kubeconfig after import:\n%s", body)
	}

	// Something else on stdin, e.g. what sudo says without a password:
	// refused, and the file is left as it was.
	root = rootCmd()
	root.SetArgs([]string{"kubeconfig", "import", "--context", "other", "--server", "https://127.0.0.1:16444"})
	root.SetIn(strings.NewReader("sudo: a terminal is required to read the password\n"))
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	if err := root.Execute(); err == nil {
		t.Error("non-kubeconfig input accepted")
	}
	if after, _ := os.ReadFile(path); !bytes.Equal(after, body) {
		t.Errorf("kubeconfig changed by a refused import:\n%s", after)
	}
}

func TestK3sBundle_RefusesOtherProviders(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "y-cluster-provision.yaml"), []byte("provider: docker\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"k3s", "bundle", "-c", dir, "--out", t.TempDir()},
		{"platform", "install", "-c", dir, "--check"},
	} {
		root := rootCmd()
		root.SetArgs(args)
		root.SetOut(&bytes.Buffer{})
		root.SetErr(&bytes.Buffer{})
		if err := root.Execute(); err == nil || !strings.Contains(err.Error(), `"host"`) {
			t.Errorf("%v: %v", args, err)
		}
	}
}

// fakeGatewayKubectl puts a kubectl on PATH that reports liveImage as
// the running Envoy Gateway controller and fails anything else, so a
// test notices when more than the version check ran.
func fakeGatewayKubectl(t *testing.T, liveImage string) {
	t.Helper()
	dir := t.TempDir()
	script := `#!/bin/sh
case "$*" in
*" get deployment envoy-gateway "*) printf '%s' '` + liveImage + `'; exit 0 ;;
esac
echo "unexpected kubectl $*" >&2
exit 3
`
	if err := os.WriteFile(filepath.Join(dir, "kubectl"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// gateway.version is refused at load without its digest, and a minor
// upgrade without the flag names the flag, for install and --check.
func TestPlatformInstall_GatewayVersion(t *testing.T) {
	write := func(gateway string) string {
		dir := t.TempDir()
		body := "provider: host\ncontext: edge\ngateway:\n" + gateway
		if err := os.WriteFile(filepath.Join(dir, "y-cluster-provision.yaml"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	run := func(args ...string) error {
		root := rootCmd()
		root.SetArgs(args)
		root.SetOut(&bytes.Buffer{})
		root.SetErr(&bytes.Buffer{})
		return root.Execute()
	}
	noDigest := write("  version: v1.9.2\n")
	for _, extra := range [][]string{nil, {"--check"}} {
		err := run(append([]string{"platform", "install", "-c", noDigest}, extra...)...)
		if err == nil || !strings.Contains(err.Error(), "gateway.installSha256") {
			t.Errorf("%v without a digest: %v", extra, err)
		}
	}

	fakeGatewayKubectl(t, "envoyproxy/gateway:v1.8.5")
	minor := write("  version: v1.9.2\n  installSha256: 0412a72907e57ff9b73c56a7bf6df5190bf0f6e4f8bb4bba34e38630bbab5778\n")
	for _, extra := range [][]string{nil, {"--check"}} {
		err := run(append([]string{"platform", "install", "-c", minor}, extra...)...)
		if err == nil || !strings.Contains(err.Error(), "v1.8.5 is installed and v1.9.2 is configured") || !strings.Contains(err.Error(), "--gateway-allow-minor allows it") {
			t.Errorf("%v minor upgrade: %v", extra, err)
		}
	}
	fakeGatewayKubectl(t, "envoyproxy/gateway:v1.9.3")
	err := run("platform", "install", "-c", minor, "--gateway-allow-minor")
	if err == nil || !strings.Contains(err.Error(), "a downgrade is refused") || strings.Contains(err.Error(), "--gateway-allow-minor allows it") {
		t.Errorf("downgrade: %v", err)
	}
}
