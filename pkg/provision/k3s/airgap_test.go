package k3s

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"go.uber.org/zap"
)

// releaseServer stands in for GitHub releases and records the request
// paths it served.
func releaseServer(t *testing.T) (requests func() []string) {
	t.Helper()
	var mu sync.Mutex
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.EscapedPath())
		mu.Unlock()
		_, _ = w.Write([]byte("content of " + filepath.Base(r.URL.Path)))
	}))
	t.Cleanup(srv.Close)
	prev := releaseBaseURL
	releaseBaseURL = srv.URL + "/download"
	t.Cleanup(func() { releaseBaseURL = prev })
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), paths...)
	}
}

type copied struct{ hostContent, nodePath string }

func recordingCopy(t *testing.T, into *[]copied) NodeCopy {
	return func(_ context.Context, hostPath, nodePath string) error {
		body, err := os.ReadFile(hostPath)
		if err != nil {
			t.Errorf("copy source unreadable: %v", err)
		}
		*into = append(*into, copied{string(body), nodePath})
		return nil
	}
}

func unameNode(machine string) *fakeNode {
	return &fakeNode{answer: func(command string) ([]byte, error) {
		if command == "uname -m" {
			return []byte(machine + "\n"), nil
		}
		return nil, nil
	}}
}

func TestInstallAirgap_AMD64(t *testing.T) {
	requests := releaseServer(t)
	cacheDir := t.TempDir()
	t.Setenv("Y_CLUSTER_CACHE_DIR", cacheDir)

	node := unameNode("x86_64")
	var copies []copied
	err := InstallAirgap(context.Background(), node.exec, recordingCopy(t, &copies), "v1.35.3+k3s1", ServerFlags(), zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}

	// The `+` has to reach GitHub percent-encoded.
	wantRequests := []string{
		"/download/v1.35.3%2Bk3s1/k3s",
		"/download/v1.35.3%2Bk3s1/k3s-airgap-images-amd64.tar.zst",
	}
	if got := requests(); !reflect.DeepEqual(got, wantRequests) {
		t.Errorf("requests = %q, want %q", got, wantRequests)
	}
	wantCopies := []copied{
		{"content of k3s", "/tmp/k3s"},
		{"content of k3s-airgap-images-amd64.tar.zst", "/tmp/k3s-airgap-images-amd64.tar.zst"},
	}
	if !reflect.DeepEqual(copies, wantCopies) {
		t.Errorf("copies = %q, want %q", copies, wantCopies)
	}
	wantCommands := []string{
		"uname -m",
		"sudo install -m 755 /tmp/k3s /usr/local/bin/k3s",
		"sudo mkdir -p /var/lib/rancher/k3s/agent/images",
		"sudo mv /tmp/k3s-airgap-images-amd64.tar.zst /var/lib/rancher/k3s/agent/images/",
		installCommand("v1.35.3+k3s1", ServerFlags(), true),
	}
	if !reflect.DeepEqual(node.commands, wantCommands) {
		t.Errorf("commands = %q, want %q", node.commands, wantCommands)
	}
	if !strings.Contains(node.commands[4], "INSTALL_K3S_SKIP_DOWNLOAD=true") {
		t.Errorf("airgap install must not download on the node: %s", node.commands[4])
	}

	// Same version again, any cluster on this host: no download.
	if err := InstallAirgap(context.Background(), unameNode("x86_64").exec, recordingCopy(t, &copies), "v1.35.3+k3s1", ServerFlags(), zap.NewNop()); err != nil {
		t.Fatal(err)
	}
	if got := requests(); len(got) != len(wantRequests) {
		t.Errorf("second install downloaded again: %q", got)
	}
	if _, err := os.Stat(filepath.Join(cacheDir, "k3s", "v1.35.3+k3s1", "k3s")); err != nil {
		t.Errorf("cache layout: %v", err)
	}
}

// An arm64 node (multipass on Apple silicon) gets arm64 artifacts,
// next to any amd64 ones of the same version in the cache.
func TestInstallAirgap_ARM64(t *testing.T) {
	requests := releaseServer(t)
	t.Setenv("Y_CLUSTER_CACHE_DIR", t.TempDir())

	node := unameNode("aarch64")
	var copies []copied
	if err := InstallAirgap(context.Background(), node.exec, recordingCopy(t, &copies), "v1.35.3+k3s1", ServerFlags(), zap.NewNop()); err != nil {
		t.Fatal(err)
	}
	wantRequests := []string{
		"/download/v1.35.3%2Bk3s1/k3s-arm64",
		"/download/v1.35.3%2Bk3s1/k3s-airgap-images-arm64.tar.zst",
	}
	if got := requests(); !reflect.DeepEqual(got, wantRequests) {
		t.Errorf("requests = %q, want %q", got, wantRequests)
	}
	// The binary is installed under its plain name whatever the
	// release file is called.
	if copies[0] != (copied{"content of k3s-arm64", "/tmp/k3s"}) {
		t.Errorf("binary copy = %q", copies[0])
	}
	if want := "sudo mv /tmp/k3s-airgap-images-arm64.tar.zst /var/lib/rancher/k3s/agent/images/"; node.commands[3] != want {
		t.Errorf("images step = %q, want %q", node.commands[3], want)
	}
}

func TestInstallAirgap_UnknownArchitecture(t *testing.T) {
	requests := releaseServer(t)
	t.Setenv("Y_CLUSTER_CACHE_DIR", t.TempDir())

	err := InstallAirgap(context.Background(), unameNode("riscv64").exec, nil, "v1.35.3+k3s1", ServerFlags(), zap.NewNop())
	if err == nil || !strings.Contains(err.Error(), "riscv64") {
		t.Errorf("want an error naming the architecture, got %v", err)
	}
	if got := requests(); len(got) != 0 {
		t.Errorf("downloaded for an unsupported architecture: %q", got)
	}
}
