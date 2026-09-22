package k3s

import (
	"context"
	"errors"
	"io"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// fakeNode answers commands by substring match and records what ran.
type fakeNode struct {
	commands []string
	answer   func(command string) ([]byte, error)
}

func (n *fakeNode) exec(_ context.Context, command string, _ io.Reader) ([]byte, error) {
	n.commands = append(n.commands, command)
	if n.answer == nil {
		return nil, nil
	}
	return n.answer(command)
}

func TestServerFlags(t *testing.T) {
	if got, want := ServerFlags(), "--write-kubeconfig-mode=644 --disable=traefik --disable=local-storage"; got != want {
		t.Errorf("ServerFlags() = %q, want %q", got, want)
	}
	got := ServerFlags("--tls-san=10.0.0.5", "--node-external-ip=10.0.0.5")
	want := "--write-kubeconfig-mode=644 --disable=traefik --disable=local-storage --tls-san=10.0.0.5 --node-external-ip=10.0.0.5"
	if got != want {
		t.Errorf("ServerFlags(extra) = %q, want %q", got, want)
	}
	// A caller appending to the result of one call must not see it
	// in the next: DisableFlags is shared with the docker provider.
	if len(DisableFlags) != 2 {
		t.Errorf("DisableFlags grew: %v", DisableFlags)
	}
}

// The installer reads its settings from the environment of `sh`, so
// the requirement is about what a shell makes of the command line.
func TestInstallCommand_EnvironmentAsTheInstallerSeesIt(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh on PATH")
	}
	for _, c := range []struct {
		name         string
		skipDownload bool
		wantSkip     string
	}{
		{"script", false, ""},
		{"airgap", true, "true"},
	} {
		t.Run(c.name, func(t *testing.T) {
			cmd := installCommand("v1.35.3+k3s1", ServerFlags("--tls-san=10.0.0.5"), c.skipDownload)
			const prefix, suffix = "curl -sfL https://get.k3s.io | ", " sudo -E sh -"
			if !strings.HasPrefix(cmd, prefix) || !strings.HasSuffix(cmd, suffix) {
				t.Fatalf("unexpected shape: %s", cmd)
			}
			env := strings.TrimSuffix(strings.TrimPrefix(cmd, prefix), suffix)
			out, err := exec.Command(sh, "-c", env+` sh -c 'printf "%s|%s|%s" "$INSTALL_K3S_VERSION" "$INSTALL_K3S_SKIP_DOWNLOAD" "$INSTALL_K3S_EXEC"'`).Output()
			if err != nil {
				t.Fatal(err)
			}
			want := "v1.35.3+k3s1|" + c.wantSkip + "|--write-kubeconfig-mode=644 --disable=traefik --disable=local-storage --tls-san=10.0.0.5"
			if string(out) != want {
				t.Errorf("installer would see %q, want %q", out, want)
			}
		})
	}
}

func TestInstallScript(t *testing.T) {
	node := &fakeNode{}
	if err := InstallScript(context.Background(), node.exec, "v1.35.3+k3s1", ServerFlags()); err != nil {
		t.Fatal(err)
	}
	if len(node.commands) != 1 || !strings.Contains(node.commands[0], "get.k3s.io") {
		t.Errorf("commands: %q", node.commands)
	}
	if strings.Contains(node.commands[0], "SKIP_DOWNLOAD") {
		t.Errorf("script install must let the installer download: %s", node.commands[0])
	}
}

func TestInstallScript_FailureCarriesNodeOutput(t *testing.T) {
	node := &fakeNode{answer: func(string) ([]byte, error) {
		return []byte("curl: (6) Could not resolve host"), errors.New("exit status 6")
	}}
	err := InstallScript(context.Background(), node.exec, "v1", ServerFlags())
	if err == nil || !strings.Contains(err.Error(), "Could not resolve host") || !strings.Contains(err.Error(), "exit status 6") {
		t.Errorf("error should carry output and cause: %v", err)
	}
}

func TestInstall_EmptyVersionRefused(t *testing.T) {
	node := &fakeNode{}
	if err := InstallScript(context.Background(), node.exec, "", ServerFlags()); err == nil {
		t.Error("InstallScript accepted an empty version")
	}
	if err := InstallAirgap(context.Background(), node.exec, nil, "", ServerFlags(), nil); err == nil {
		t.Error("InstallAirgap accepted an empty version")
	}
	if len(node.commands) != 0 {
		t.Errorf("nothing should run on the node: %q", node.commands)
	}
}

func fastPoll(t *testing.T) {
	t.Helper()
	prev := readyPollInterval
	readyPollInterval = time.Millisecond
	t.Cleanup(func() { readyPollInterval = prev })
}

// "Ready" means the apiserver answered. A kubeconfig file that is
// already on disk (a VM that has booted before) is not an answer.
func TestWaitReady_WaitsForTheAPIServerNotTheFile(t *testing.T) {
	fastPoll(t)
	answers := [][2]string{
		{"", "exit status 1"}, // no kubeconfig yet
		{"Error from server (ServiceUnavailable)", "exit status 1"}, // file there, apiserver not
		{"[-]etcd failed: reason withheld", "exit status 1"},
		{"ok", ""},
	}
	node := &fakeNode{}
	node.answer = func(string) ([]byte, error) {
		a := answers[len(node.commands)-1]
		if a[1] != "" {
			return []byte(a[0]), errors.New(a[1])
		}
		return []byte(a[0] + "\n"), nil
	}
	if err := WaitReady(context.Background(), node.exec, time.Minute); err != nil {
		t.Fatal(err)
	}
	if len(node.commands) != len(answers) {
		t.Errorf("probed %d times, want %d", len(node.commands), len(answers))
	}
	for _, want := range []string{"test -s " + KubeconfigPath, "k3s kubectl get --raw=/readyz"} {
		if !strings.Contains(node.commands[0], want) {
			t.Errorf("probe lacks %q: %s", want, node.commands[0])
		}
	}
}

func TestWaitReady_TimeoutReportsLastAnswer(t *testing.T) {
	fastPoll(t)
	node := &fakeNode{answer: func(string) ([]byte, error) {
		return []byte("[-]etcd failed"), errors.New("exit status 1")
	}}
	err := WaitReady(context.Background(), node.exec, 20*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "etcd failed") {
		t.Errorf("want a timeout naming the last answer, got %v", err)
	}
}

func TestWaitReady_StopsOnCancel(t *testing.T) {
	fastPoll(t)
	ctx, cancel := context.WithCancel(context.Background())
	node := &fakeNode{}
	node.answer = func(string) ([]byte, error) {
		if len(node.commands) == 3 {
			cancel()
		}
		return nil, errors.New("exit status 1")
	}
	if err := WaitReady(ctx, node.exec, time.Minute); !errors.Is(err, context.Canceled) {
		t.Errorf("want context.Canceled, got %v", err)
	}
}

const k3sKubeconfig = `apiVersion: v1
clusters:
- cluster:
    certificate-authority-data: Zm9v
    server: https://127.0.0.1:6443
  name: default
`

func TestReadKubeconfig_PointsAtTheHostSideAddress(t *testing.T) {
	node := &fakeNode{answer: func(string) ([]byte, error) { return []byte(k3sKubeconfig), nil }}
	got, err := ReadKubeconfig(context.Background(), node.exec, "192.168.64.7:6443")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "server: https://192.168.64.7:6443\n") {
		t.Errorf("server not rewritten:\n%s", got)
	}
	if strings.Contains(string(got), "127.0.0.1") {
		t.Errorf("node-local address left behind:\n%s", got)
	}
	if len(node.commands) != 1 || !strings.Contains(node.commands[0], KubeconfigPath) {
		t.Errorf("commands: %q", node.commands)
	}
}

func TestReadKubeconfig_NodeFailure(t *testing.T) {
	node := &fakeNode{answer: func(string) ([]byte, error) {
		return []byte("cat: no such file"), errors.New("exit status 1")
	}}
	_, err := ReadKubeconfig(context.Background(), node.exec, "127.0.0.1:26443")
	if err == nil || !strings.Contains(err.Error(), "no such file") {
		t.Errorf("error should carry node output: %v", err)
	}
}

// A forward that keeps 6443 on the host's loopback names the same
// address the node does; the file passes through unchanged.
func TestRewriteKubeconfigServer_SameAddress(t *testing.T) {
	got, err := RewriteKubeconfigServer([]byte(k3sKubeconfig), "127.0.0.1:6443")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != k3sKubeconfig {
		t.Errorf("kubeconfig changed:\n%s", got)
	}
}

// Whatever came back is not the kubeconfig k3s writes; merging it
// would give the operator a context that dials the wrong place.
func TestRewriteKubeconfigServer_RefusesUnexpectedInput(t *testing.T) {
	for _, raw := range []string{"", "sudo: a password is required\n", strings.ReplaceAll(k3sKubeconfig, "127.0.0.1:6443", "10.0.0.1:6443")} {
		if _, err := RewriteKubeconfigServer([]byte(raw), "127.0.0.1:26443"); err == nil {
			t.Errorf("accepted %q", raw)
		}
	}
}
