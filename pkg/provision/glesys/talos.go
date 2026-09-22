package glesys

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"go.uber.org/zap"

	machineapi "github.com/siderolabs/talos/pkg/machinery/api/machine"
	"github.com/siderolabs/talos/pkg/machinery/client"
	clientconfig "github.com/siderolabs/talos/pkg/machinery/client/config"
	talosconfig "github.com/siderolabs/talos/pkg/machinery/config"
	"github.com/siderolabs/talos/pkg/machinery/config/generate"
	"github.com/siderolabs/talos/pkg/machinery/config/machine"
	"github.com/siderolabs/talos/pkg/machinery/constants"

	"github.com/Yolean/y-cluster/pkg/kubeconfig"
)

// installDisk is the system disk as a GleSYS KVM guest sees it: the
// one virtio disk the server is created with.
const installDisk = "/dev/vda"

// talosVersion pins the machine config's version contract to the
// Talos release the template default boots, so the generated config
// carries no field that release does not know. An operator who
// picks a newer template gets a config the newer Talos still
// accepts; the reverse is what this protects against.
var talosVersion = talosconfig.TalosVersion1_14

// machineConfigs is what the provisioner generates for one node:
// the control-plane machine config that goes into the server's
// cloudconfig, and the talosconfig that authenticates against it.
type machineConfigs struct {
	controlPlane []byte
	talosconfig  *clientconfig.Config
}

// generateMachineConfigs produces the Talos config for a single
// control-plane node at ipv4 that also schedules workloads. The
// cluster endpoint is the node's own public address: with one node
// there is nothing to balance, and naming the address here is why
// Provision reserves it before the server exists. clusterName
// doubles as the kubeconfig cluster name, which is how the CLI's
// cluster.Lookup finds the context again.
func generateMachineConfigs(clusterName, ipv4 string) (*machineConfigs, error) {
	input, err := generate.NewInput(clusterName, "https://"+ipv4+":6443", constants.DefaultKubernetesVersion,
		generate.WithVersionContract(talosVersion),
		generate.WithAllowSchedulingOnControlPlanes(true),
		generate.WithInstallDisk(installDisk),
		generate.WithEndpointList([]string{ipv4}),
		generate.WithAdditionalSubjectAltNames([]string{ipv4}),
	)
	if err != nil {
		return nil, fmt.Errorf("talos config input: %w", err)
	}
	cp, err := input.Config(machine.TypeControlPlane)
	if err != nil {
		return nil, fmt.Errorf("talos control-plane config: %w", err)
	}
	cpBytes, err := cp.Bytes()
	if err != nil {
		return nil, fmt.Errorf("encode talos config: %w", err)
	}
	tc, err := input.Talosconfig()
	if err != nil {
		return nil, fmt.Errorf("talosconfig: %w", err)
	}
	return &machineConfigs{controlPlane: cpBytes, talosconfig: tc}, nil
}

// talosClient dials the node's Talos API with the context's
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

// bootstrap runs the one-time etcd bootstrap. Talos rejects a second
// call with an error naming the reason; that is treated as done,
// since the only way to get there is a provision retried past its
// first bootstrap.
func bootstrap(ctx context.Context, c *client.Client) error {
	err := c.Bootstrap(ctx, &machineapi.BootstrapRequest{})
	if err == nil || strings.Contains(err.Error(), "already") {
		return nil
	}
	return fmt.Errorf("talos bootstrap: %w", err)
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
