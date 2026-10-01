package glesys

import (
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"

	glesysapi "github.com/glesys/glesys-go/v8"
	"github.com/siderolabs/talos/pkg/machinery/config/machine"
	"sigs.k8s.io/yaml"

	"github.com/Yolean/y-cluster/pkg/kubeconfig"
)

// parseDocuments splits a Talos multi-document config into a map by
// kind. Named documents (firewall rules) key as kind/name.
func parseDocuments(t *testing.T, raw []byte) map[string]map[string]any {
	t.Helper()
	docs := map[string]map[string]any{}
	for _, part := range strings.Split(string(raw), "\n---\n") {
		var d map[string]any
		if err := yaml.Unmarshal([]byte(part), &d); err != nil {
			t.Fatalf("parse document: %v\n%s", err, part)
		}
		kind, _ := d["kind"].(string)
		if kind == "" {
			kind = "v1alpha1"
		}
		if name, ok := d["name"].(string); ok && kind == "NetworkRuleConfig" {
			kind += "/" + name
		}
		if _, dup := docs[kind]; dup {
			kind += "#2"
		}
		docs[kind] = d
	}
	return docs
}

func dig(docs map[string]map[string]any, kind string, path ...string) any {
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

// The machine config is the whole of what the server boots with, so
// its contents are the contract: one control-plane node that
// schedules workloads, an endpoint at the reserved address, and the
// disk as a GleSYS KVM guest names it. Talos 1.14 writes it as a
// stream of documents; the checks find each by kind.
func TestGenerateMachineConfigs_SingleNode(t *testing.T) {
	mc, err := generateMachineConfigs(clusterSpec{Name: "qa-glesys",
		Nodes: []nodeSpec{{Role: machine.TypeControlPlane, IPv4: "203.0.113.10"}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(mc.nodes) != 1 {
		t.Fatalf("want one config, got %d", len(mc.nodes))
	}
	docs := parseDocuments(t, mc.nodes[0])

	if got := dig(docs, "v1alpha1", "machine", "type"); got != "controlplane" {
		t.Errorf("machine type: got %v", got)
	}
	if got := dig(docs, "KubeClusterConfig", "endpoint"); got != "https://203.0.113.10:6443" {
		t.Errorf("endpoint: got %v", got)
	}
	if got := dig(docs, "KubeClusterConfig", "clusterName"); got != "qa-glesys" {
		t.Errorf("clusterName: got %v", got)
	}
	if taints := dig(docs, "KubeNodeConfig", "taints"); taints != nil {
		t.Errorf("a single node must schedule workloads; got taints %v", taints)
	}
	if got := fmt.Sprint(dig(docs, "UnattendedInstallConfig", "provisioning", "diskSelector", "match")); !strings.Contains(got, installDisk) {
		t.Errorf("install disk selector: got %q, want it to name %s", got, installDisk)
	}
	if got := fmt.Sprint(dig(docs, "v1alpha1", "machine", "certSANs")); got != "[203.0.113.10]" {
		t.Errorf("the Talos API cert must name the public address, got %v", got)
	}
	// The DiscoveryServiceConfig document is the one that names
	// discovery.talos.dev; DiscoveryIdentityConfig is only the
	// cluster's random id and secret and stays.
	if docs["DiscoveryServiceConfig"] != nil {
		t.Error("cluster discovery must be off: no node may register with discovery.talos.dev")
	}
	if strings.Contains(string(mc.nodes[0]), "discovery.talos.dev") {
		t.Error("the machine config must not name discovery.talos.dev anywhere")
	}
	if got := fmt.Sprint(dig(docs, "KubeAPIServerConfig", "certExtraSANs")); got != "[203.0.113.10]" {
		t.Errorf("the apiserver cert must name the public address, got %v", got)
	}
	// Time from the Swedish national service, never the Talos
	// default, on every config.
	if got := fmt.Sprint(dig(docs, "TimeSyncConfig", "ntp", "servers")); got != "[ntp.se]" {
		t.Errorf("time servers: got %v, want [ntp.se]", got)
	}
	// Flannel stays without Cilium; no firewall without CIDRs.
	if docs["KubeFlannelCNIConfig"] == nil {
		t.Error("flannel must stay when cilium is not asked for")
	}
	if docs["NetworkDefaultActionConfig"] != nil {
		t.Error("no firewall without apiAllowedCIDRs")
	}
	// The talosconfig points at the same node and is minted from
	// the same CA, or talosctl could not connect.
	tc := mc.talosconfig
	c := tc.Contexts[tc.Context]
	if c == nil || len(c.Endpoints) != 1 || c.Endpoints[0] != "203.0.113.10" {
		t.Errorf("talosconfig endpoints: got %+v", c)
	}
	if got := dig(docs, "v1alpha1", "machine", "ca", "crt"); got != c.CA {
		t.Error("talosconfig CA is not the machine config's OS CA")
	}
}

// A control plane with workers: the control plane keeps its taint,
// the workers join the same endpoint, Cilium replaces flannel, and
// the firewall opens exactly what each role needs.
func TestGenerateMachineConfigs_ControlPlaneAndWorkers(t *testing.T) {
	spec := clusterSpec{Name: "qa-glesys", Cilium: true, APIAllowedCIDRs: []string{"198.51.100.7/32"},
		Nodes: []nodeSpec{
			{Role: machine.TypeControlPlane, IPv4: "203.0.113.10"},
			{Role: machine.TypeWorker, IPv4: "203.0.113.11"},
			{Role: machine.TypeWorker, IPv4: "203.0.113.12"},
		}}
	mc, err := generateMachineConfigs(spec)
	if err != nil {
		t.Fatal(err)
	}
	if len(mc.nodes) != 3 {
		t.Fatalf("want three configs, got %d", len(mc.nodes))
	}
	cp := parseDocuments(t, mc.nodes[0])
	w := parseDocuments(t, mc.nodes[1])

	if got := dig(cp, "v1alpha1", "machine", "type"); got != "controlplane" {
		t.Errorf("control plane type: got %v", got)
	}
	if got := dig(w, "v1alpha1", "machine", "type"); got != "worker" {
		t.Errorf("worker type: got %v", got)
	}
	if taints := dig(cp, "KubeNodeConfig", "taints"); taints == nil {
		t.Error("a dedicated control plane must keep its NoSchedule taint")
	}
	if got := dig(w, "KubeClusterConfig", "endpoint"); got != "https://203.0.113.10:6443" {
		t.Errorf("worker endpoint: got %v", got)
	}
	if cp["KubeFlannelCNIConfig"] != nil {
		t.Error("flannel must go when cilium is the CNI")
	}
	for _, docs := range []map[string]map[string]any{cp, w} {
		if got := dig(docs, "NetworkDefaultActionConfig", "ingress"); got != "block" {
			t.Errorf("default ingress action: got %v, want block", got)
		}
		if got := fmt.Sprint(dig(docs, "TimeSyncConfig", "ntp", "servers")); got != "[ntp.se]" {
			t.Errorf("time servers: got %v", got)
		}
	}
	// Who may reach what.
	subnets := func(docs map[string]map[string]any, rule string) string {
		var out []string
		in, _ := dig(docs, "NetworkRuleConfig/"+rule, "ingress").([]any)
		for _, r := range in {
			out = append(out, fmt.Sprint(r.(map[string]any)["subnet"]))
		}
		return strings.Join(out, " ")
	}
	// The pod and service networks count as the cluster: a pod's
	// traffic to its own node's apiserver carries the pod address.
	cluster := "10.244.0.0/16 10.96.0.0/12 203.0.113.10/32 203.0.113.11/32 203.0.113.12/32"
	if got := subnets(cp, "talos-api"); got != cluster+" 198.51.100.7/32" {
		t.Errorf("talos-api on the control plane: %q", got)
	}
	if got := subnets(cp, "kubernetes-api"); got != cluster+" 198.51.100.7/32" {
		t.Errorf("kubernetes-api: %q", got)
	}
	if got := subnets(cp, "etcd"); got != "203.0.113.10/32" {
		t.Errorf("etcd must be reachable from the control plane only: %q", got)
	}
	if got := subnets(w, "wireguard"); got != cluster {
		t.Errorf("wireguard on a worker: %q", got)
	}
	if w["NetworkRuleConfig/kubernetes-api"] != nil || w["NetworkRuleConfig/etcd"] != nil || w["NetworkRuleConfig/trustd"] != nil {
		t.Error("a worker must not open control-plane ports")
	}
	if got := subnets(w, "https"); got != "0.0.0.0/0" {
		t.Errorf("https on a worker must be open to anyone: %q", got)
	}
	if cp["NetworkRuleConfig/https"] != nil || cp["NetworkRuleConfig/http"] != nil {
		t.Error("a dedicated control plane serves no ingress")
	}
	if got := fmt.Sprint(dig(cp, "v1alpha1", "machine", "certSANs")); got != "[203.0.113.10 203.0.113.11 203.0.113.12]" {
		t.Errorf("cert SANs: %v", got)
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
	s := state{Context: "qa-glesys", ServerID: "kvm123", DataCenter: "Stockholm", IPv4: "203.0.113.10", Bootstrapped: true,
		Workers: []nodeState{{ServerID: "kvm124", IPv4: "203.0.113.11"}}}
	if err := saveState(dir, s); err != nil {
		t.Fatal(err)
	}
	got, err := loadState(dir, "qa-glesys")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, s) {
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
	if err := os.WriteFile(workerConfigPath(dir, "qa-glesys", 0), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := s.servers(); fmt.Sprint(got) != "[kvm123 kvm124]" {
		t.Errorf("servers: %v", got)
	}
	if got := s.workerAddresses(); fmt.Sprint(got) != "[203.0.113.11]" {
		t.Errorf("worker addresses: %v", got)
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
	if _, err := os.Stat(workerConfigPath(dir, "qa-glesys", 0)); !os.IsNotExist(err) {
		t.Error("deleteState should remove the worker configs")
	}
	if err := deleteState(dir, "qa-glesys"); err != nil {
		t.Errorf("deleteState must be idempotent: %v", err)
	}
}
