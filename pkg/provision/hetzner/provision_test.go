package hetzner

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/hetznercloud/hcloud-go/v2/hcloud/schema"
	"go.uber.org/zap"

	"github.com/Yolean/y-cluster/pkg/provision/config"
	"github.com/Yolean/y-cluster/pkg/sshexec"
)

// k3sKubeconfig is what k3s writes on the node.
const k3sKubeconfig = `apiVersion: v1
kind: Config
clusters:
- cluster:
    certificate-authority-data: Zm9v
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
    client-certificate-data: Zm9v
    client-key-data: Zm9v
`

// fakeNode stands in for the server's sshd: it answers the commands
// Provision runs on the node. failOn makes any command containing
// that text fail, which is how a test breaks Provision at a step.
type fakeNode struct {
	commands []string
	failOn   string
}

func newFakeNode(t *testing.T) *fakeNode {
	t.Helper()
	n := &fakeNode{}
	prev := sshExec
	sshExec = func(_ context.Context, _ sshexec.Target, command string, _ io.Reader) ([]byte, error) {
		n.commands = append(n.commands, command)
		switch {
		case n.failOn != "" && strings.Contains(command, n.failOn):
			return []byte("simulated failure"), errors.New("exit status 1")
		case command == "true", strings.Contains(command, "get.k3s.io"):
			return nil, nil
		case strings.Contains(command, "--raw=/readyz"):
			return []byte("ok\n"), nil
		case strings.HasPrefix(command, "sudo cat /etc/rancher/k3s/k3s.yaml"):
			return []byte(k3sKubeconfig), nil
		}
		t.Errorf("fake node: unexpected command %q", command)
		return nil, errors.New("unexpected command")
	}
	t.Cleanup(func() { sshExec = prev })
	return n
}

// testEnv points everything Provision writes on the host at temp
// dirs, and returns the kubeconfig path.
func testEnv(t *testing.T) (kubeconfigPath string) {
	t.Helper()
	t.Setenv(CacheDirEnv, t.TempDir())
	kubeconfigPath = filepath.Join(t.TempDir(), "kubeconfig")
	t.Setenv("KUBECONFIG", kubeconfigPath)
	return kubeconfigPath
}

// testConfig is a minimal config for contextName in lb-group "team",
// without the parts that need a real cluster behind the kubeconfig
// (Envoy Gateway, the lifetime reaper).
func testConfig(t *testing.T, contextName string) config.HetznerConfig {
	t.Helper()
	cfg := config.HetznerConfig{CommonConfig: config.CommonConfig{Provider: config.ProviderHetzner, Context: contextName}}
	cfg.LBGroup = "team"
	cfg.Gateway.Skip = true
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("test config: %v", err)
	}
	return cfg
}

func kubeContexts(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// Provision then Teardown, against a project that starts and has to
// end empty. This is the whole of what the e2e does that needs no
// real server, and the e2e cannot run without a paid account.
func TestProvisionTeardown(t *testing.T) {
	cloud := newFakeCloud(t)
	newFakeNode(t)
	kubeconfigPath := testEnv(t)
	cfg := testConfig(t, "qa-one")

	c, err := Provision(context.Background(), cfg, zap.NewNop())
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	want := []string{"certificate/qa-one", "load_balancer/y-cluster-team", "server/qa-one", "ssh_key/qa-one"}
	if got := cloud.inventory(); !reflect.DeepEqual(got, want) {
		t.Errorf("project after provision = %v, want %v", got, want)
	}
	if got := cloud.lbCertificates("y-cluster-team"); len(got) != 1 || got[0] != c.State().CertificateID {
		t.Errorf("LB 443 certificates = %v, want this context's %d", got, c.State().CertificateID)
	}
	if kc := kubeContexts(t, kubeconfigPath); !strings.Contains(kc, "name: qa-one") || !strings.Contains(kc, "server: https://127.0.0.1:6443") {
		t.Errorf("kubeconfig after provision:\n%s", kc)
	}
	if !HasState("qa-one") {
		t.Error("no state sidecar after provision")
	}

	if err := Teardown(context.Background(), "qa-one", zap.NewNop()); err != nil {
		t.Fatalf("Teardown: %v", err)
	}
	if got := cloud.inventory(); len(got) != 0 {
		t.Errorf("project after teardown still holds %v", got)
	}
	if HasState("qa-one") {
		t.Error("state sidecar survived teardown")
	}
}

// Teardown is given a context name and nothing else, so it removes
// the kubeconfig entries of that name too. The other provisioners do;
// a context left behind points at an address Hetzner hands to the
// next customer.
func TestTeardown_RemovesTheKubeContext(t *testing.T) {
	newFakeCloud(t)
	newFakeNode(t)
	kubeconfigPath := testEnv(t)

	if _, err := Provision(context.Background(), testConfig(t, "qa-one"), zap.NewNop()); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if err := Teardown(context.Background(), "qa-one", zap.NewNop()); err != nil {
		t.Fatalf("Teardown: %v", err)
	}
	if kc := kubeContexts(t, kubeconfigPath); strings.Contains(kc, "qa-one") {
		t.Errorf("kubeconfig still mentions the context after teardown:\n%s", kc)
	}
}

// The sidecar is a cache: losing it (another machine, a cleaned home
// directory) must not decide whether the load balancer keeps billing.
// What teardown needs to know is on the resources themselves, as the
// labels Provision put there.
func TestTeardown_WithoutTheSidecar(t *testing.T) {
	cloud := newFakeCloud(t)
	newFakeNode(t)
	testEnv(t)

	if _, err := Provision(context.Background(), testConfig(t, "qa-one"), zap.NewNop()); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if err := deleteState(CacheDir(), "qa-one"); err != nil {
		t.Fatal(err)
	}
	if err := Teardown(context.Background(), "qa-one", zap.NewNop()); err != nil {
		t.Fatalf("Teardown: %v", err)
	}
	if got := cloud.inventory(); len(got) != 0 {
		t.Errorf("project after teardown without a sidecar still holds %v", got)
	}
}

// The same, after someone has also deleted the server by hand in the
// Hetzner console: the certificate still says which lb-group it was
// for.
func TestTeardown_WithoutTheSidecarOrTheServer(t *testing.T) {
	cloud := newFakeCloud(t)
	newFakeNode(t)
	testEnv(t)

	c, err := Provision(context.Background(), testConfig(t, "qa-one"), zap.NewNop())
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if err := deleteState(CacheDir(), "qa-one"); err != nil {
		t.Fatal(err)
	}
	delete(cloud.servers, c.State().ServerID)

	if err := Teardown(context.Background(), "qa-one", zap.NewNop()); err != nil {
		t.Fatalf("Teardown: %v", err)
	}
	if got := cloud.inventory(); len(got) != 0 {
		t.Errorf("project still holds %v", got)
	}
}

// Two contexts of one lb-group share the load balancer. The first
// teardown takes its own certificate off the LB and leaves the rest;
// the second takes the LB with it.
func TestTeardown_SharedLoadBalancer(t *testing.T) {
	cloud := newFakeCloud(t)
	newFakeNode(t)
	testEnv(t)

	one, err := Provision(context.Background(), testConfig(t, "qa-one"), zap.NewNop())
	if err != nil {
		t.Fatalf("Provision qa-one: %v", err)
	}
	two, err := Provision(context.Background(), testConfig(t, "qa-two"), zap.NewNop())
	if err != nil {
		t.Fatalf("Provision qa-two: %v", err)
	}
	wantCerts := []int64{one.State().CertificateID, two.State().CertificateID}
	if got := cloud.lbCertificates("y-cluster-team"); !reflect.DeepEqual(got, wantCerts) {
		t.Fatalf("LB certificates = %v, want both contexts' %v", got, wantCerts)
	}

	if err := Teardown(context.Background(), "qa-one", zap.NewNop()); err != nil {
		t.Fatalf("Teardown qa-one: %v", err)
	}
	want := []string{"certificate/qa-two", "load_balancer/y-cluster-team", "server/qa-two", "ssh_key/qa-two"}
	if got := cloud.inventory(); !reflect.DeepEqual(got, want) {
		t.Errorf("project after the first teardown = %v, want %v", got, want)
	}
	if got := cloud.lbCertificates("y-cluster-team"); !reflect.DeepEqual(got, wantCerts[1:]) {
		t.Errorf("LB certificates = %v, want only qa-two's", got)
	}

	if err := Teardown(context.Background(), "qa-two", zap.NewNop()); err != nil {
		t.Fatalf("Teardown qa-two: %v", err)
	}
	if got := cloud.inventory(); len(got) != 0 {
		t.Errorf("project after the last teardown still holds %v", got)
	}
}

// Every step of Provision after the first resource exists can fail,
// and a failed Provision returns no handle to clean up with. What it
// made has to be gone again: a server bills by the hour, and the
// reaper that would end it is installed almost last.
func TestProvision_FailureLeavesNothingBehind(t *testing.T) {
	for _, tc := range []struct {
		name     string
		sabotage func(cloud *fakeCloud, node *fakeNode)
		want     string
	}{
		{"the server's create action fails", func(c *fakeCloud, _ *fakeNode) {
			c.failedActions["POST /servers"] = "no capacity in this location"
		}, "no capacity"},
		{"sshd never answers", func(_ *fakeCloud, n *fakeNode) { n.failOn = "true" }, "SSH"},
		{"the k3s install fails", func(_ *fakeCloud, n *fakeNode) { n.failOn = "get.k3s.io" }, "k3s"},
		{"the certificate upload is refused", func(c *fakeCloud, _ *fakeNode) {
			c.failures["POST /certificates"] = "certificate limit reached"
		}, "certificate limit"},
		{"the load balancer is refused", func(c *fakeCloud, _ *fakeNode) {
			c.failures["POST /load_balancers"] = "load balancer limit reached"
		}, "load balancer limit"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cloud := newFakeCloud(t)
			node := newFakeNode(t)
			kubeconfigPath := testEnv(t)
			prev := sshWaitTimeout
			sshWaitTimeout = 0
			t.Cleanup(func() { sshWaitTimeout = prev })
			tc.sabotage(cloud, node)

			_, err := Provision(context.Background(), testConfig(t, "qa-one"), zap.NewNop())
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want the original failure (%q) reported, got %v", tc.want, err)
			}
			if got := cloud.inventory(); len(got) != 0 {
				t.Errorf("a failed provision left %v in the project", got)
			}
			if HasState("qa-one") {
				t.Error("a failed provision left a state sidecar")
			}
			if kc := kubeContexts(t, kubeconfigPath); strings.Contains(kc, "qa-one") {
				t.Errorf("a failed provision left a kube context:\n%s", kc)
			}
		})
	}
}

// Rolling back one context must not touch its lb-group neighbours.
func TestProvision_FailureLeavesNeighboursAlone(t *testing.T) {
	cloud := newFakeCloud(t)
	newFakeNode(t)
	testEnv(t)

	neighbour, err := Provision(context.Background(), testConfig(t, "qa-neighbour"), zap.NewNop())
	if err != nil {
		t.Fatalf("Provision qa-neighbour: %v", err)
	}
	before := cloud.inventory()

	// qa-one gets as far as having its certificate on the shared LB.
	cloud.failures["POST /load_balancers/"] = "update_service refused"
	if _, err := Provision(context.Background(), testConfig(t, "qa-one"), zap.NewNop()); err == nil {
		t.Fatal("Provision should have failed at the certificate attach")
	}
	delete(cloud.failures, "POST /load_balancers/")

	if got := cloud.inventory(); !reflect.DeepEqual(got, before) {
		t.Errorf("project = %v, want the neighbour's %v", got, before)
	}
	if got := cloud.lbCertificates("y-cluster-team"); !reflect.DeepEqual(got, []int64{neighbour.State().CertificateID}) {
		t.Errorf("LB certificates = %v, want only the neighbour's", got)
	}
}

// Creating the ssh key is what claims the context's name. If that
// call fails, nothing in the project belongs to this run, whatever is
// named after the context by then belongs to whoever won the race,
// and the failed run deletes nothing.
func TestProvision_LosingTheNameDeletesNothing(t *testing.T) {
	cloud := newFakeCloud(t)
	newFakeNode(t)
	testEnv(t)
	cloud.failures["POST /ssh_keys"] = "SSH key name is already used"

	if _, err := Provision(context.Background(), testConfig(t, "qa-one"), zap.NewNop()); err == nil {
		t.Fatal("Provision should have failed at the ssh key")
	}
	for _, req := range cloud.requests {
		if strings.HasPrefix(req, "DELETE ") {
			t.Errorf("a run that created nothing sent %s", req)
		}
	}
	if _, err := os.Stat(filepath.Join(CacheDir(), "qa-one-ssh")); !os.IsNotExist(err) {
		t.Errorf("local keypair left behind: %v", err)
	}
}

// Names from an earlier run that was not cleaned up are found before
// anything is created, not after the server has been bought and k3s
// installed on it.
func TestProvision_StaleNamesAreRefusedUpFront(t *testing.T) {
	for _, kind := range []string{"ssh_key", "certificate"} {
		t.Run(kind, func(t *testing.T) {
			cloud := newFakeCloud(t)
			newFakeNode(t)
			testEnv(t)
			switch kind {
			case "ssh_key":
				cloud.keys[1] = &schema.SSHKey{ID: 1, Name: "qa-one"}
			case "certificate":
				cloud.certs[1] = &schema.Certificate{ID: 1, Name: "qa-one"}
			}
			before := cloud.inventory()

			_, err := Provision(context.Background(), testConfig(t, "qa-one"), zap.NewNop())
			if err == nil || !strings.Contains(err.Error(), "already exists") {
				t.Fatalf("want an already-exists refusal, got %v", err)
			}
			if got := cloud.inventory(); !reflect.DeepEqual(got, before) {
				t.Errorf("project = %v, want it untouched: %v", got, before)
			}
			for _, req := range cloud.requests {
				if strings.HasPrefix(req, "POST ") || strings.HasPrefix(req, "DELETE ") {
					t.Errorf("a refused provision sent %s", req)
				}
			}
		})
	}
}
