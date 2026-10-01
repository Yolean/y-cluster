package glesys

import (
	"context"
	"encoding/json"
	"fmt"
	"net/netip"
	"os/exec"
	"strings"
	"time"

	"go.uber.org/zap"

	machineapi "github.com/siderolabs/talos/pkg/machinery/api/machine"
	"github.com/siderolabs/talos/pkg/machinery/client"
	clientconfig "github.com/siderolabs/talos/pkg/machinery/client/config"
	talosconfig "github.com/siderolabs/talos/pkg/machinery/config"
	configdoc "github.com/siderolabs/talos/pkg/machinery/config/config"
	"github.com/siderolabs/talos/pkg/machinery/config/container"
	"github.com/siderolabs/talos/pkg/machinery/config/generate"
	"github.com/siderolabs/talos/pkg/machinery/config/machine"
	"github.com/siderolabs/talos/pkg/machinery/config/types/k8s"
	"github.com/siderolabs/talos/pkg/machinery/config/types/network"
	"github.com/siderolabs/talos/pkg/machinery/constants"
	"github.com/siderolabs/talos/pkg/machinery/nethelpers"

	"github.com/Yolean/y-cluster/pkg/kubeconfig"
)

// installDisk is the system disk as a GleSYS KVM guest sees it: the
// one disk the server is created with, on a SCSI controller (seen
// as /dev/sda on a running node; there is no /dev/vda). Only an
// install or upgrade reads it, the template image is pre-installed.
const installDisk = "/dev/sda"

// talosVersion pins the machine config's version contract to the
// Talos release the template default boots, so the generated config
// carries no field that release does not know. An operator who
// picks a newer template gets a config the newer Talos still
// accepts; the reverse is what this protects against.
var talosVersion = talosconfig.TalosVersion1_14

// timeServers is where every node syncs its clock. ntp.se is the
// Swedish national time service, traceable to UTC(SP), and local to
// the hosting country; Talos's default is time.cloudflare.com. Not a
// config field: there is one right answer for the clusters this
// provider is for.
var timeServers = []string{"ntp.se"}

// Ports, by who may reach them, for the ingress firewall.
const (
	portKubeAPI    = 6443
	portTalosAPI   = 50000
	portTrustd     = 50001
	portKubelet    = 10250
	portEtcdLo     = 2379
	portEtcdHi     = 2380
	portCiliumHlth = 4240
	portVXLAN      = 8472
	portWireGuard  = 51871
	portHTTP       = 80
	portHTTPS      = 443
)

// nodeSpec is one server's role and address, the inputs that differ
// per node when generating configs.
type nodeSpec struct {
	Role machine.Type
	IPv4 string
}

// clusterSpec is what generateMachineConfigs needs to describe the
// whole cluster: every node (the control plane first), and the
// operator's choices that shape the config.
type clusterSpec struct {
	Name  string
	Nodes []nodeSpec
	// Cilium drops Talos's flannel from the config; the provisioner
	// installs Cilium after bootstrap.
	Cilium bool
	// APIAllowedCIDRs, when non-empty, enables the ingress firewall
	// (see firewallDocuments).
	APIAllowedCIDRs []string
}

func (s clusterSpec) controlPlane() nodeSpec { return s.Nodes[0] }

func (s clusterSpec) workers() []nodeSpec { return s.Nodes[1:] }

// ingressNodes are the nodes whose address serves 80 and 443: the
// workers, or the one node when there are none.
func (s clusterSpec) ingressNodes() []nodeSpec {
	if len(s.Nodes) == 1 {
		return s.Nodes
	}
	return s.workers()
}

func (s clusterSpec) addresses() []string {
	out := make([]string, len(s.Nodes))
	for i, n := range s.Nodes {
		out[i] = n.IPv4
	}
	return out
}

// machineConfigs is what the provisioner generates for a cluster:
// one machine config per node, in the order of clusterSpec.Nodes,
// and the talosconfig that authenticates against all of them.
type machineConfigs struct {
	nodes       [][]byte
	talosconfig *clientconfig.Config
}

// generateMachineConfigs produces the Talos configs for the cluster.
// The cluster endpoint is the control plane's own public address:
// with one control plane there is nothing to balance, and naming the
// address here is why Provision reserves it before the server
// exists. The control plane schedules workloads only when it is the
// only node. Every node's config gets the same additions: the time
// servers, the firewall when asked for, and no flannel when Cilium
// is the CNI. spec.Name doubles as the kubeconfig cluster name,
// which is how the CLI's cluster.Lookup finds the context again.
func generateMachineConfigs(spec clusterSpec) (*machineConfigs, error) {
	if len(spec.Nodes) == 0 || spec.Nodes[0].Role != machine.TypeControlPlane {
		return nil, fmt.Errorf("the first node must be the control plane")
	}
	cp := spec.controlPlane()
	input, err := generate.NewInput(spec.Name, "https://"+cp.IPv4+":6443", constants.DefaultKubernetesVersion,
		generate.WithVersionContract(talosVersion),
		generate.WithAllowSchedulingOnControlPlanes(len(spec.Nodes) == 1),
		generate.WithInstallDisk(installDisk),
		generate.WithEndpointList([]string{cp.IPv4}),
		generate.WithAdditionalSubjectAltNames(spec.addresses()),
		// No cluster discovery: the default registers every node
		// with Sidero's hosted discovery.talos.dev, a dependency
		// outside the hosting country that operators would have to
		// be told about. It is only needed
		// by KubeSpan, which this provider does not enable; a
		// self-hosted discovery service is the way back if it ever
		// is.
		generate.WithClusterDiscovery(false),
	)
	if err != nil {
		return nil, fmt.Errorf("talos config input: %w", err)
	}
	out := &machineConfigs{}
	for _, n := range spec.Nodes {
		cfg, err := input.Config(n.Role)
		if err != nil {
			return nil, fmt.Errorf("talos %s config: %w", n.Role, err)
		}
		docs := cfg.Documents()
		if spec.Cilium {
			docs = withoutKind(docs, k8s.KubeFlannelCNIConfig)
		}
		docs = append(docs, timeSyncDocument())
		docs = append(docs, firewallDocuments(spec, n)...)
		c, err := container.New(docs...)
		if err != nil {
			return nil, fmt.Errorf("assemble %s config: %w", n.Role, err)
		}
		b, err := c.Bytes()
		if err != nil {
			return nil, fmt.Errorf("encode %s config: %w", n.Role, err)
		}
		out.nodes = append(out.nodes, b)
	}
	tc, err := input.Talosconfig()
	if err != nil {
		return nil, fmt.Errorf("talosconfig: %w", err)
	}
	out.talosconfig = tc
	return out, nil
}

func withoutKind(docs []configdoc.Document, kind string) []configdoc.Document {
	out := docs[:0:0]
	for _, d := range docs {
		if d.Kind() != kind {
			out = append(out, d)
		}
	}
	return out
}

// timeSyncDocument points the node's clock at timeServers.
func timeSyncDocument() configdoc.Document {
	d := network.NewTimeSyncConfigV1Alpha1()
	d.TimeNTP = &network.NTPConfig{Servers: timeServers}
	return d
}

// firewallDocuments renders the Talos ingress firewall for one node:
// block everything not listed, then
//
//   - the Talos API from the cluster's addresses and the operator's
//     CIDRs, on every node;
//   - trustd, the Kubernetes API and etcd on the control plane:
//     trustd and the Kubernetes API from the cluster and the
//     operator, etcd from the control plane itself;
//   - kubelet, Cilium's health check, the VXLAN overlay and the
//     WireGuard tunnel from the cluster's addresses, on every node;
//   - 80 and 443 from anywhere on the nodes that serve ingress.
//
// Nothing when spec.APIAllowedCIDRs is empty: the firewall is opt-in,
// because a node that blocks its operator is a node only the
// provider's API can reach.
func firewallDocuments(spec clusterSpec, n nodeSpec) []configdoc.Document {
	if len(spec.APIAllowedCIDRs) == 0 {
		return nil
	}
	// The cluster is its nodes' addresses plus the pod and service
	// networks: a pod on the control plane reaches its own node's
	// apiserver with its pod address as the source (nothing
	// masquerades node-local traffic), and CoreDNS and every
	// controller that talks to kubernetes.default does exactly
	// that. Without the pod network here, DNS never becomes ready.
	cluster := []string{constants.DefaultIPv4PodCIDR, constants.DefaultIPv4ServiceCIDR}
	for _, a := range spec.addresses() {
		cluster = append(cluster, a+"/32")
	}
	operator := append(append([]string{}, cluster...), spec.APIAllowedCIDRs...)
	cpOnly := []string{spec.controlPlane().IPv4 + "/32"}
	anywhere := []string{"0.0.0.0/0"}

	block := network.NewDefaultActionConfigV1Alpha1()
	block.Ingress = nethelpers.DefaultActionBlock
	docs := []configdoc.Document{block,
		rule("talos-api", nethelpers.ProtocolTCP, operator, portTalosAPI),
		rule("kubelet", nethelpers.ProtocolTCP, cluster, portKubelet),
		rule("cilium-health", nethelpers.ProtocolTCP, cluster, portCiliumHlth),
		rule("vxlan", nethelpers.ProtocolUDP, cluster, portVXLAN),
		rule("wireguard", nethelpers.ProtocolUDP, cluster, portWireGuard),
	}
	if n.Role == machine.TypeControlPlane {
		docs = append(docs,
			rule("kubernetes-api", nethelpers.ProtocolTCP, operator, portKubeAPI),
			rule("trustd", nethelpers.ProtocolTCP, operator, portTrustd),
			ruleRange("etcd", nethelpers.ProtocolTCP, cpOnly, portEtcdLo, portEtcdHi),
		)
	}
	for _, in := range spec.ingressNodes() {
		if in.IPv4 == n.IPv4 {
			docs = append(docs,
				rule("http", nethelpers.ProtocolTCP, anywhere, portHTTP),
				rule("https", nethelpers.ProtocolTCP, anywhere, portHTTPS),
			)
		}
	}
	return docs
}

func rule(name string, proto nethelpers.Protocol, subnets []string, port uint16) configdoc.Document {
	return ruleRange(name, proto, subnets, port, port)
}

func ruleRange(name string, proto nethelpers.Protocol, subnets []string, lo, hi uint16) configdoc.Document {
	r := network.NewRuleConfigV1Alpha1()
	r.MetaName = name
	r.PortSelector = network.RulePortSelector{
		Ports:    network.PortRanges{{Lo: lo, Hi: hi}},
		Protocol: proto,
	}
	for _, s := range subnets {
		// Validated by the config, and the cluster's own addresses
		// are what the provider returned.
		r.Ingress = append(r.Ingress, network.IngressRule{Subnet: netip.MustParsePrefix(s)})
	}
	return r
}

// talosClient dials a node's Talos API with the cluster's
// talosconfig. The returned context carries the node address, which
// apid needs on every call.
func talosClient(ctx context.Context, tc *clientconfig.Config, ipv4 string) (*client.Client, context.Context, error) {
	c, err := client.New(ctx, client.WithConfig(tc), client.WithEndpoints(ipv4))
	if err != nil {
		return nil, nil, fmt.Errorf("talos client: %w", err)
	}
	return c, client.WithNode(ctx, ipv4), nil
}

// waitForTalosAPI polls the node's Talos API until it answers a
// Version call or timeout fires. The first answer means the machine
// config in the cloudconfig was applied: apid serves with the
// certificate the config carries, and the talosconfig was minted
// against the same CA.
func waitForTalosAPI(ctx context.Context, c *client.Client, timeout time.Duration, logger *zap.Logger) error {
	deadline := time.Now().Add(timeout)
	for {
		callCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		resp, err := c.Version(callCtx)
		cancel()
		if err == nil {
			for _, m := range resp.GetMessages() {
				logger.Info("talos api reachable", zap.String("version", m.GetVersion().GetTag()))
			}
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("talos api not reachable after %s: %w", timeout, err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}

// bootstrap runs the one-time etcd bootstrap, retrying while Talos
// says it is not available yet: apid answers before the machine has
// reached the stage where bootstrap is accepted. A second bootstrap
// is rejected with an error naming the reason; that is treated as
// done, since the only way to get there is a provision retried past
// its first bootstrap.
func bootstrap(ctx context.Context, c *client.Client, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		err := c.Bootstrap(ctx, &machineapi.BootstrapRequest{})
		if err == nil || strings.Contains(err.Error(), "already") {
			return nil
		}
		if !strings.Contains(err.Error(), "not available yet") || time.Now().After(deadline) {
			return fmt.Errorf("talos bootstrap: %w", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}

// waitForKubeconfig asks the node for its admin kubeconfig until the
// call succeeds, which it does once kube-apiserver is up and serving,
// or timeout fires.
func waitForKubeconfig(ctx context.Context, c *client.Client, timeout time.Duration) ([]byte, error) {
	deadline := time.Now().Add(timeout)
	for {
		callCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		raw, err := c.Kubeconfig(callCtx)
		cancel()
		if err == nil {
			return raw, nil
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("kubeconfig not available after %s: %w", timeout, err)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}

// waitForKubeAPI polls `kubectl get --raw=/readyz` on the merged
// context until the apiserver answers 200. Talos hands out the
// kubeconfig as soon as its PKI exists, which is before
// kube-apiserver listens on 6443; the gateway install that follows
// drives the same kubectl path, so the probe is on the path the
// next caller uses.
func waitForKubeAPI(ctx context.Context, contextName string, timeout time.Duration, logger *zap.Logger) error {
	logger.Info("waiting for the Kubernetes API to be ready", zap.String("context", contextName), zap.Duration("timeout", timeout))
	deadline := time.Now().Add(timeout)
	for {
		probe := exec.CommandContext(ctx, "kubectl", "--context="+contextName, "get", "--raw=/readyz")
		out, err := probe.CombinedOutput()
		if err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("apiserver /readyz never returned 200 within %s on context %q: %v: %s", timeout, contextName, err, strings.TrimSpace(string(out)))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}

// renameKubeconfig rewrites the names in a kubeconfig Talos issued
// (context and user `admin@<cluster>`, cluster `<cluster>`) to the
// y-cluster convention: the context is the configured context and
// the cluster and user entries carry the cluster name. The result is
// what kubeconfig.Manager.Import merges.
func renameKubeconfig(raw []byte, contextName, clusterName string) ([]byte, error) {
	f, err := kubeconfig.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("parse talos kubeconfig: %w", err)
	}
	if len(f.Contexts) != 1 || len(f.Clusters) != 1 || len(f.Users) != 1 {
		return nil, fmt.Errorf("talos kubeconfig has %d contexts, %d clusters, %d users; expected one of each", len(f.Contexts), len(f.Clusters), len(f.Users))
	}
	f.Contexts[0].Name = contextName
	f.Contexts[0].Context.Cluster = clusterName
	f.Contexts[0].Context.User = clusterName
	f.Clusters[0].Name = clusterName
	f.Users[0].Name = clusterName
	f.CurrentContext = contextName
	return json.Marshal(f)
}
