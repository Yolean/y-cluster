package glesys

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	glesysapi "github.com/glesys/glesys-go/v8"
	"sigs.k8s.io/yaml"

	"github.com/Yolean/y-cluster/pkg/kubeconfig"
)

// The machine config is the whole of what the server boots with, so
// its contents are the contract: one control-plane node that
// schedules workloads, an endpoint at the reserved address, and the
// disk as a GleSYS KVM guest names it. Talos 1.14 writes it as a
// stream of documents; the checks find each by kind.
func TestGenerateMachineConfigs(t *testing.T) {
	mc, err := generateMachineConfigs("qa-glesys", "203.0.113.10")
	if err != nil {
		t.Fatal(err)
	}
	docs := map[string]map[string]any{}
	for _, raw := range strings.Split(string(mc.controlPlane), "\n---\n") {
		var d map[string]any
		if err := yaml.Unmarshal([]byte(raw), &d); err != nil {
			t.Fatalf("parse document: %v\n%s", err, raw)
		}
		kind, _ := d["kind"].(string)
		if kind == "" {
			kind = "v1alpha1"
		}
		if _, dup := docs[kind]; dup {
			kind += "#2"
		}
		docs[kind] = d
	}
	dig := func(kind string, path ...string) any {
		var cur any = docs[kind]
		for _, k := range path {
			m, ok := cur.(map[string]any)
			if !ok {
				return nil
			}
			cur = m[k]
		}
		return cur
	}

	if got := dig("v1alpha1", "machine", "type"); got != "controlplane" {
		t.Errorf("machine type: got %v", got)
	}
	if got := dig("KubeClusterConfig", "endpoint"); got != "https://203.0.113.10:6443" {
		t.Errorf("endpoint: got %v", got)
	}
	if got := dig("KubeClusterConfig", "clusterName"); got != "qa-glesys" {
		t.Errorf("clusterName: got %v", got)
	}
	if taints := dig("KubeNodeConfig", "taints"); taints != nil {
		t.Errorf("a single node must schedule workloads; got taints %v", taints)
	}
	if got := fmt.Sprint(dig("UnattendedInstallConfig", "provisioning", "diskSelector", "match")); !strings.Contains(got, installDisk) {
		t.Errorf("install disk selector: got %q, want it to name %s", got, installDisk)
	}
	if got := fmt.Sprint(dig("v1alpha1", "machine", "certSANs")); got != "[203.0.113.10]" {
		t.Errorf("the Talos API cert must name the public address, got %v", got)
	}
	if got := fmt.Sprint(dig("KubeAPIServerConfig", "certExtraSANs")); got != "[203.0.113.10]" {
		t.Errorf("the apiserver cert must name the public address, got %v", got)
	}
	// The talosconfig points at the same node and is minted from
	// the same CA, or talosctl could not connect.
	tc := mc.talosconfig
	c := tc.Contexts[tc.Context]
	if c == nil || len(c.Endpoints) != 1 || c.Endpoints[0] != "203.0.113.10" {
		t.Errorf("talosconfig endpoints: got %+v", c)
	}
	if got := dig("v1alpha1", "machine", "ca", "crt"); got != c.CA {
		t.Error("talosconfig CA is not the machine config's OS CA")
	}
}

func TestMatchTemplate(t *testing.T) {
	offered := []glesysapi.ServerPlatformTemplateDetails{
		{Name: "ubuntu-24-04", OS: "Ubuntu"},
		{Name: "Talos 1.14", OS: "Talos Linux"},
		{Name: "Talos 1.13", OS: "Talos Linux"},
	}
	if err := matchTemplate(offered, "KVM", "Talos 1.14"); err != nil {
		t.Errorf("offered template refused: %v", err)
	}
	err := matchTemplate(offered, "KVM", "Talos 1.15")
	if err == nil || !strings.Contains(err.Error(), "Talos 1.14, Talos 1.13") {
		t.Errorf("want the Talos templates listed, got %v", err)
	}
	err = matchTemplate(offered[:1], "KVM", "Talos 1.14")
	if err == nil || !strings.Contains(err.Error(), "no Talos template") {
		t.Errorf("want to hear that no Talos template is offered, got %v", err)
	}
}

// Talos names the kubeconfig entries after its cluster and admin
// role; the merged kubeconfig follows the y-cluster convention that
// cluster.Lookup reads the cluster name back from.
func TestRenameKubeconfig(t *testing.T) {
	raw := []byte(`apiVersion: v1
kind: Config
current-context: admin@qa-glesys
clusters:
- name: qa-glesys
  cluster:
    server: https://203.0.113.10:6443
    certificate-authority-data: Zm9v
contexts:
- name: admin@qa-glesys
  context:
    cluster: qa-glesys
    user: admin@qa-glesys
    namespace: default
users:
- name: admin@qa-glesys
  user:
    client-certificate-data: Zm9v
    client-key-data: YmFy
`)
	out, err := renameKubeconfig(raw, "qa-glesys", "qa-glesys-cluster")
	if err != nil {
		t.Fatal(err)
	}
	f, err := kubeconfig.Parse(out)
	if err != nil {
		t.Fatal(err)
	}
	if f.CurrentContext != "qa-glesys" || f.Contexts[0].Name != "qa-glesys" {
		t.Errorf("context: got current=%q name=%q", f.CurrentContext, f.Contexts[0].Name)
	}
	if f.Contexts[0].Context.Cluster != "qa-glesys-cluster" || f.Contexts[0].Context.User != "qa-glesys-cluster" {
		t.Errorf("context references: got %+v", f.Contexts[0].Context)
	}
	if f.Clusters[0].Name != "qa-glesys-cluster" || f.Users[0].Name != "qa-glesys-cluster" {
		t.Errorf("entries: cluster=%q user=%q", f.Clusters[0].Name, f.Users[0].Name)
	}
	if f.Clusters[0].Cluster.Server != "https://203.0.113.10:6443" || f.Users[0].User.ClientKeyData != "YmFy" {
		t.Error("credentials and server must survive the rename")
	}
	if f.Contexts[0].Context.Namespace != "default" {
		t.Error("the namespace must survive the rename")
	}

	if _, err := renameKubeconfig([]byte("apiVersion: v1\nkind: Config\n"), "x", "x"); err == nil {
		t.Error("a kubeconfig without exactly one entry of each kind must be refused")
	}
}

func TestStateRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s := state{Context: "qa-glesys", ServerID: "kvm123", DataCenter: "Stockholm", IPv4: "203.0.113.10", Bootstrapped: true}
	if err := saveState(dir, s); err != nil {
		t.Fatal(err)
	}
	got, err := loadState(dir, "qa-glesys")
	if err != nil {
		t.Fatal(err)
	}
	if got != s {
		t.Errorf("got %+v, want %+v", got, s)
	}
	// What cluster.Lookup reads from the sidecar, by json key.
	data, _ := os.ReadFile(statePath(dir, "qa-glesys"))
	var probe struct {
		IPv4 string `json:"ipv4"`
	}
	if err := json.Unmarshal(data, &probe); err != nil || probe.IPv4 != s.IPv4 {
		t.Errorf("sidecar ipv4 key: %q %v", probe.IPv4, err)
	}
	t.Setenv(CacheDirEnv, dir)
	if !HasState("qa-glesys") || HasState("other") {
		t.Error("HasState should follow the sidecar")
	}
	if err := os.WriteFile(TalosconfigPath(dir, "qa-glesys"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := deleteState(dir, "qa-glesys"); err != nil {
		t.Fatal(err)
	}
	if HasState("qa-glesys") {
		t.Error("deleteState should remove the sidecar")
	}
	if _, err := os.Stat(TalosconfigPath(dir, "qa-glesys")); !os.IsNotExist(err) {
		t.Error("deleteState should remove the talosconfig")
	}
	if err := deleteState(dir, "qa-glesys"); err != nil {
		t.Errorf("deleteState must be idempotent: %v", err)
	}
}
