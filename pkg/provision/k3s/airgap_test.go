package k3s

import (
	"context"
	"crypto/sha256"
	"fmt"
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
// paths it served. Every file's content is derived from its name, and
// the sha256sum-<arch>.txt files list the digests of that content.
// tamper names a file that is served with other bytes than the
// checksum file promises.
type releaseServer struct {
	mu     sync.Mutex
	paths  []string
	tamper string
}

func content(name string) []byte { return []byte("content of " + name) }

func newReleaseServer(t *testing.T) *releaseServer {
	t.Helper()
	rs := &releaseServer{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rs.mu.Lock()
		rs.paths = append(rs.paths, r.URL.EscapedPath())
		tamper := rs.tamper
		rs.mu.Unlock()
		name := filepath.Base(r.URL.Path)
		if strings.HasPrefix(name, "sha256sum-") {
			arch := strings.TrimSuffix(strings.TrimPrefix(name, "sha256sum-"), ".txt")
			binary := "k3s"
			if arch != "amd64" {
				binary = "k3s-" + arch
			}
			for _, file := range []string{binary, "k3s-airgap-images-" + arch + ".tar.zst"} {
				fmt.Fprintf(w, "%x  %s\n", sha256.Sum256(content(file)), file)
			}
			return
		}
		if name == tamper {
			_, _ = w.Write([]byte("something else"))
			return
		}
		_, _ = w.Write(content(name))
	}))
	t.Cleanup(srv.Close)
	prev := releaseBaseURL
	releaseBaseURL = srv.URL + "/download"
	t.Cleanup(func() { releaseBaseURL = prev })
	return rs
}

func (rs *releaseServer) requests() []string {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	return append([]string(nil), rs.paths...)
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
	release := newReleaseServer(t)
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
		"/download/v1.35.3%2Bk3s1/sha256sum-amd64.txt",
		"/download/v1.35.3%2Bk3s1/k3s",
		"/download/v1.35.3%2Bk3s1/k3s-airgap-images-amd64.tar.zst",
	}
	if got := release.requests(); !reflect.DeepEqual(got, wantRequests) {
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
	if got := release.requests(); len(got) != len(wantRequests) {
		t.Errorf("second install downloaded again: %q", got)
	}
	if _, err := os.Stat(filepath.Join(cacheDir, "k3s", "v1.35.3+k3s1", "k3s")); err != nil {
		t.Errorf("cache layout: %v", err)
	}
}

// An arm64 node (multipass on Apple silicon) gets arm64 artifacts,
// next to any amd64 ones of the same version in the cache.
func TestInstallAirgap_ARM64(t *testing.T) {
	release := newReleaseServer(t)
	t.Setenv("Y_CLUSTER_CACHE_DIR", t.TempDir())

	node := unameNode("aarch64")
	var copies []copied
	if err := InstallAirgap(context.Background(), node.exec, recordingCopy(t, &copies), "v1.35.3+k3s1", ServerFlags(), zap.NewNop()); err != nil {
		t.Fatal(err)
	}
	wantRequests := []string{
		"/download/v1.35.3%2Bk3s1/sha256sum-arm64.txt",
		"/download/v1.35.3%2Bk3s1/k3s-arm64",
		"/download/v1.35.3%2Bk3s1/k3s-airgap-images-arm64.tar.zst",
	}
	if got := release.requests(); !reflect.DeepEqual(got, wantRequests) {
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
	release := newReleaseServer(t)
	t.Setenv("Y_CLUSTER_CACHE_DIR", t.TempDir())

	err := InstallAirgap(context.Background(), unameNode("riscv64").exec, nil, "v1.35.3+k3s1", ServerFlags(), zap.NewNop())
	if err == nil || !strings.Contains(err.Error(), "riscv64") {
		t.Errorf("want an error naming the architecture, got %v", err)
	}
	if got := release.requests(); len(got) != 0 {
		t.Errorf("downloaded for an unsupported architecture: %q", got)
	}
}

// What is downloaded here is installed as root on the node. A file
// that is not what the release's checksum file says is refused, and
// it does not stay in the cache, where the next provision would take
// it for good.
func TestInstallAirgap_RefusesAnArtifactThatFailsItsChecksum(t *testing.T) {
	release := newReleaseServer(t)
	cacheDir := t.TempDir()
	t.Setenv("Y_CLUSTER_CACHE_DIR", cacheDir)
	release.tamper = "k3s"

	node := unameNode("x86_64")
	var copies []copied
	err := InstallAirgap(context.Background(), node.exec, recordingCopy(t, &copies), "v1.35.3+k3s1", ServerFlags(), zap.NewNop())
	if err == nil || !strings.Contains(err.Error(), "sha256") {
		t.Fatalf("want a checksum refusal, got %v", err)
	}
	if len(copies) != 0 || len(node.commands) != 1 {
		t.Errorf("nothing may reach the node: copies %v, commands %q", copies, node.commands)
	}
	left, err := os.ReadDir(filepath.Join(cacheDir, "k3s", "v1.35.3+k3s1"))
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 0 {
		t.Errorf("the refused download left %d files in the cache", len(left))
	}

	// The upstream is fine again: the next run downloads afresh.
	release.tamper = ""
	if err := InstallAirgap(context.Background(), unameNode("x86_64").exec, recordingCopy(t, &copies), "v1.35.3+k3s1", ServerFlags(), zap.NewNop()); err != nil {
		t.Fatalf("after the upstream recovered: %v", err)
	}
	if len(copies) != 2 || copies[0].hostContent != string(content("k3s")) {
		t.Errorf("copies after recovery: %q", copies)
	}
}
