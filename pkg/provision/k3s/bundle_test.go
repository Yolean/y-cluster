package k3s

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"go.uber.org/zap"
)

func testSpec(t *testing.T) BundleSpec {
	t.Helper()
	server, err := CiliumServerSettings(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return BundleSpec{
		Version:     "v1.35.3+k3s1",
		Arch:        "amd64",
		Server:      server,
		Registries:  []byte("mirrors: {}\n"),
		PostInstall: []string{"ln -sfn /a /b"},
	}
}

func sum(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func TestWriteBundle_Airgap(t *testing.T) {
	newReleaseServer(t)
	t.Setenv("Y_CLUSTER_CACHE_DIR", t.TempDir())
	dir := filepath.Join(t.TempDir(), "bundle")
	m, err := WriteBundle(context.Background(), testSpec(t), dir, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}

	config, _ := os.ReadFile(filepath.Join(dir, "config.yaml"))
	wantFiles := []BundleFile{
		{Path: "k3s", Dest: "/usr/local/bin/k3s", Mode: "0755", SHA256: sum(content("k3s"))},
		{Path: "k3s-airgap-images-amd64.tar.zst", Dest: "/var/lib/rancher/k3s/agent/images/k3s-airgap-images-amd64.tar.zst", Mode: "0644", SHA256: sum(content("k3s-airgap-images-amd64.tar.zst"))},
		{Path: "config.yaml", Dest: "/etc/rancher/k3s/config.yaml", Mode: "0600", SHA256: sum(config)},
		{Path: "registries.yaml", Dest: "/etc/rancher/k3s/registries.yaml", Mode: "0600", SHA256: sum([]byte("mirrors: {}\n"))},
	}
	if !reflect.DeepEqual(m.Files, wantFiles) {
		t.Errorf("files:\n got %+v\nwant %+v", m.Files, wantFiles)
	}
	wantEnv := map[string]string{"INSTALL_K3S_VERSION": "v1.35.3+k3s1", "INSTALL_K3S_EXEC": "server", "INSTALL_K3S_SKIP_DOWNLOAD": "true"}
	if m.Install.Script != "install.sh" || m.Install.SHA256 != sum(content("install.sh")) || !reflect.DeepEqual(m.Install.Env, wantEnv) {
		t.Errorf("install %+v", m.Install)
	}
	if m.K3s.Version != "v1.35.3+k3s1" || m.K3s.Install != "airgap" || m.K3s.Arch != "amd64" {
		t.Errorf("k3s %+v", m.K3s)
	}
	if !reflect.DeepEqual(m.PostInstall, []string{"ln -sfn /a /b"}) {
		t.Errorf("post-install %v", m.PostInstall)
	}
	if !strings.Contains(string(config), "flannel-backend: none") {
		t.Errorf("config.yaml:\n%s", config)
	}
	for _, f := range []string{"config.yaml", "registries.yaml"} {
		if info, err := os.Stat(filepath.Join(dir, f)); err != nil || info.Mode().Perm() != 0o600 {
			t.Errorf("%s may hold secrets, want 0600: %v %v", f, info.Mode(), err)
		}
	}
	// What was written is what ReadBundle returns, verified.
	read, err := ReadBundle(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(read, m) {
		t.Errorf("read back:\n%+v\n%+v", read, m)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 6 {
		t.Errorf("bundle dir has %d entries", len(entries))
	}
}

func TestWriteBundle_ScriptHasNoBinaryAndDownloads(t *testing.T) {
	newReleaseServer(t)
	t.Setenv("Y_CLUSTER_CACHE_DIR", t.TempDir())
	spec := testSpec(t)
	spec.Install = "script"
	spec.Registries = nil
	m, err := WriteBundle(context.Background(), spec, t.TempDir(), zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, f := range m.Files {
		paths = append(paths, f.Path)
	}
	if !reflect.DeepEqual(paths, []string{"config.yaml"}) {
		t.Errorf("a script bundle without registries carries config.yaml only: %v", paths)
	}
	if _, skip := m.Install.Env["INSTALL_K3S_SKIP_DOWNLOAD"]; skip || m.K3s.Arch != "" {
		t.Errorf("script install downloads on the node: %+v %+v", m.Install.Env, m.K3s)
	}
}

func TestWriteBundle_ReplacesAnEarlierBundleOnly(t *testing.T) {
	newReleaseServer(t)
	t.Setenv("Y_CLUSTER_CACHE_DIR", t.TempDir())
	dir := t.TempDir()
	if _, err := WriteBundle(context.Background(), testSpec(t), dir, zap.NewNop()); err != nil {
		t.Fatal(err)
	}
	spec := testSpec(t)
	spec.Registries = nil
	if _, err := WriteBundle(context.Background(), spec, dir, zap.NewNop()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "registries.yaml")); !os.IsNotExist(err) {
		t.Errorf("registries.yaml of the earlier bundle left behind: %v", err)
	}

	foreign := t.TempDir()
	if err := os.WriteFile(filepath.Join(foreign, "notes.txt"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := WriteBundle(context.Background(), testSpec(t), foreign, zap.NewNop()); err == nil {
		t.Error("a directory with other files is refused")
	}
}

func TestReadBundle_RefusesAlteredFiles(t *testing.T) {
	newReleaseServer(t)
	t.Setenv("Y_CLUSTER_CACHE_DIR", t.TempDir())
	dir := t.TempDir()
	if _, err := WriteBundle(context.Background(), testSpec(t), dir, zap.NewNop()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("flannel-backend: vxlan\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadBundle(dir); err == nil || !strings.Contains(err.Error(), "config.yaml") {
		t.Errorf("altered config.yaml: %v", err)
	}
}

// bundleNode is a fake node that records commands with their stdin.
type bundleNode struct {
	machine  string
	commands []string
	stdin    map[string]string
}

func (n *bundleNode) exec(_ context.Context, command string, stdin io.Reader) ([]byte, error) {
	n.commands = append(n.commands, command)
	if stdin != nil {
		b, _ := io.ReadAll(stdin)
		if n.stdin == nil {
			n.stdin = map[string]string{}
		}
		n.stdin[command] = string(b)
	}
	switch {
	case command == "uname -m":
		return []byte(n.machine + "\n"), nil
	case strings.HasPrefix(command, "mktemp -d"):
		return []byte("/tmp/y-cluster-k3s-bundle.AbC123\n"), nil
	}
	return nil, nil
}

func TestApplyBundle(t *testing.T) {
	newReleaseServer(t)
	t.Setenv("Y_CLUSTER_CACHE_DIR", t.TempDir())
	dir := t.TempDir()
	m, err := WriteBundle(context.Background(), testSpec(t), dir, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	node := &bundleNode{machine: "x86_64"}
	var copies []copied
	if err := ApplyBundle(context.Background(), node.exec, recordingCopy(t, &copies), dir, zap.NewNop()); err != nil {
		t.Fatal(err)
	}
	const st = "/tmp/y-cluster-k3s-bundle.AbC123"
	var copiedTo []string
	for _, c := range copies {
		copiedTo = append(copiedTo, c.nodePath)
	}
	wantCopies := []string{st + "/k3s", st + "/k3s-airgap-images-amd64.tar.zst", st + "/config.yaml", st + "/registries.yaml", st + "/install.sh"}
	if !reflect.DeepEqual(copiedTo, wantCopies) {
		t.Errorf("copies %v", copiedTo)
	}
	wantCommands := []string{
		"uname -m",
		"mktemp -d /tmp/y-cluster-k3s-bundle.XXXXXX",
		"sudo install -D -m 0755 '" + st + "/k3s' '/usr/local/bin/k3s'",
		"sudo install -D -m 0644 '" + st + "/k3s-airgap-images-amd64.tar.zst' '/var/lib/rancher/k3s/agent/images/k3s-airgap-images-amd64.tar.zst'",
		"sudo install -D -m 0600 '" + st + "/config.yaml' '/etc/rancher/k3s/config.yaml'",
		"sudo install -D -m 0600 '" + st + "/registries.yaml' '/etc/rancher/k3s/registries.yaml'",
		"sudo sha256sum -c --quiet -",
		"sudo env INSTALL_K3S_EXEC='server' INSTALL_K3S_SKIP_DOWNLOAD='true' INSTALL_K3S_VERSION='v1.35.3+k3s1' sh '" + st + "/install.sh'",
		"sudo sh -c 'ln -sfn /a /b'",
		"rm -rf '" + st + "'",
	}
	if !reflect.DeepEqual(node.commands, wantCommands) {
		t.Errorf("commands:\n got %q\nwant %q", node.commands, wantCommands)
	}
	var sums strings.Builder
	for _, f := range m.Files {
		sums.WriteString(f.SHA256 + "  " + f.Dest + "\n")
	}
	if got := node.stdin["sudo sha256sum -c --quiet -"]; got != sums.String() {
		t.Errorf("checksums checked on the node:\n%s", got)
	}
}

func TestApplyBundle_RefusesAnotherArchitecture(t *testing.T) {
	newReleaseServer(t)
	t.Setenv("Y_CLUSTER_CACHE_DIR", t.TempDir())
	dir := t.TempDir()
	if _, err := WriteBundle(context.Background(), testSpec(t), dir, zap.NewNop()); err != nil {
		t.Fatal(err)
	}
	node := &bundleNode{machine: "aarch64"}
	var copies []copied
	err := ApplyBundle(context.Background(), node.exec, recordingCopy(t, &copies), dir, zap.NewNop())
	if err == nil || !strings.Contains(err.Error(), "amd64") {
		t.Errorf("err %v", err)
	}
	if len(copies) != 0 || len(node.commands) != 1 {
		t.Errorf("nothing is copied or run on a node of another architecture: %v %v", copies, node.commands)
	}
}
