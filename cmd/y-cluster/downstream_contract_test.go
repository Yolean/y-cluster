package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/Yolean/y-cluster/pkg/cluster"
	"github.com/Yolean/y-cluster/pkg/provision/config"
	"github.com/Yolean/y-cluster/pkg/provision/qemu"
)

// downstreamCLI lists the invocations that repos depending on
// y-cluster make: ystack (bin/, e2e/, k3s/) and checkit
// (cluster-local/e2e/). Removing or renaming any of these is a
// breaking change for them even when every other test stays green,
// because their acceptance tests only ever run a released binary or
// need cloud credentials. See testdata/downstream/README.md.
var downstreamCLI = []struct {
	user  string
	verb  string // space-separated command path
	flags []string
}{
	{"ystack, checkit", "provision", []string{"config"}},
	{"ystack, checkit", "teardown", []string{"config", "keep-disk"}},
	{"ystack, checkit", "yconverge", []string{"context", "skip-checks", "dry-run", "print-deps"}},
	{"ystack", "images cache", nil},
	{"ystack", "images list", nil},
	{"ystack, checkit", "images load", []string{"context"}},
	{"ystack", "cache info", nil},
	{"checkit", "detect", []string{"context"}},
	{"checkit", "stop", []string{"context"}},
	{"checkit", "prepare-export", []string{"context"}},
	{"checkit", "export", []string{"context", "format"}},
	{"checkit", "import", []string{"config"}},
	{"checkit", "manifests add", []string{"context"}},
	{"checkit", "manifests rm", []string{"context"}},
	{"checkit", "gateway hostnames", []string{"context", "csv"}},
	{"y-cluster scripts", "echo render", nil},
	{"y-cluster scripts", "lifetime gcp-flags", []string{"config"}},
}

func TestDownstreamContract_CLI(t *testing.T) {
	root := rootCmd()
	for _, tc := range downstreamCLI {
		cmd, rest, err := root.Find(strings.Fields(tc.verb))
		if err != nil || len(rest) != 0 || cmd == root {
			t.Errorf("`y-cluster %s` (used by %s) no longer resolves: rest=%v err=%v", tc.verb, tc.user, rest, err)
			continue
		}
		for _, f := range tc.flags {
			if lookupFlag(cmd, f) == nil {
				t.Errorf("`y-cluster %s` lost --%s (used by %s)", tc.verb, f, tc.user)
			}
		}
	}
}

func lookupFlag(cmd *cobra.Command, name string) any {
	if f := cmd.Flags().Lookup(name); f != nil {
		return f
	}
	if f := cmd.InheritedFlags().Lookup(name); f != nil {
		return f
	}
	return nil
}

// checkit's scripts gate on `y-cluster --version`.
func TestDownstreamContract_VersionFlag(t *testing.T) {
	if rootCmd().Version == "" {
		t.Error("root command has no Version, so --version is gone")
	}
}

// yconverge is also installed as the kubectl plugin kubectl-yconverge
// (ystack tracks a symlink); `-k` is its kustomize directory flag.
func TestDownstreamContract_YconvergeShorthand(t *testing.T) {
	for name, cmd := range map[string]*cobra.Command{"yconverge": yconvergeCmd(), "kubectl-yconverge": yconvergePluginCmd()} {
		if cmd.Flags().ShorthandLookup("k") == nil {
			t.Errorf("%s lost -k", name)
		}
	}
	if provisionCmd().Flags().ShorthandLookup("c") == nil {
		t.Error("provision lost -c")
	}
}

// ystack runs `yconverge --dry-run=server`; the empty default means a
// real apply.
func TestDownstreamContract_DryRunServer(t *testing.T) {
	f := yconvergeCmd().Flags().Lookup("dry-run")
	if f == nil {
		t.Fatal("no --dry-run")
	}
	if f.DefValue != "" {
		t.Errorf("--dry-run default %q; downstream relies on unset meaning apply", f.DefValue)
	}
	if !strings.Contains(f.Usage, "server") {
		t.Errorf("--dry-run usage no longer documents server: %q", f.Usage)
	}
}

// The provision configs downstream repos keep must load, and keep
// meaning what those repos rely on.
func TestDownstreamContract_ProvisionConfigs(t *testing.T) {
	t.Run("ystack local-qemu", func(t *testing.T) {
		loaded, err := config.LoadProvision(filepath.Join("..", "..", "testdata", "downstream", "ystack-local-qemu"))
		if err != nil {
			t.Fatal(err)
		}
		c, ok := loaded.(*config.QEMUConfig)
		if !ok {
			t.Fatalf("loaded %T", loaded)
		}
		if c.Context != "local" || c.Name != "local" {
			t.Errorf("context/name: %q/%q", c.Context, c.Name)
		}
		// ystack's y-k8s-ingress-hosts writes this address into
		// /etc/hosts from the GatewayClass dns-hint-ip annotation.
		if got := c.HostRoutableIP(); got != "127.0.0.1" {
			t.Errorf("HostRoutableIP %q; ystack expects loopback for a guest:80 forward", got)
		}
		if len(c.Registries.Mirrors) != 2 {
			t.Errorf("registry mirrors: %v", c.Registries.Mirrors)
		}
	})
	t.Run("ystack local-docker", func(t *testing.T) {
		loaded, err := config.LoadProvision(filepath.Join("..", "..", "testdata", "downstream", "ystack-local-docker"))
		if err != nil {
			t.Fatal(err)
		}
		c, ok := loaded.(*config.DockerConfig)
		if !ok {
			t.Fatalf("loaded %T", loaded)
		}
		if got := c.HostRoutableIP(); got != "127.0.0.1" {
			t.Errorf("HostRoutableIP %q", got)
		}
	})
	// checkit's appliance configs name no provider (discovery picks
	// qemu on a kvm host) and share one relative dataDisk between the
	// build config and the import config: the import boots a new
	// image on the build's data disk.
	for _, name := range []string{"checkit-appliance-test", "checkit-appliance-import"} {
		t.Run(name, func(t *testing.T) {
			prev := config.DiscoverProviderFn
			config.DiscoverProviderFn = func() string { return config.ProviderQEMU }
			t.Cleanup(func() { config.DiscoverProviderFn = prev })

			dir := filepath.Join("..", "..", "testdata", "downstream", name)
			loaded, err := config.LoadProvision(dir)
			if err != nil {
				t.Fatal(err)
			}
			c, ok := loaded.(*config.QEMUConfig)
			if !ok {
				t.Fatalf("loaded %T", loaded)
			}
			abs, err := filepath.Abs(filepath.Join(dir, "..", "appliance-test-data.qcow2"))
			if err != nil {
				t.Fatal(err)
			}
			if got := qemu.FromConfig(c).DataDisk; got != abs {
				t.Errorf("dataDisk resolved to %q, want %q (relative to the config dir)", got, abs)
			}
		})
	}
}

// A provider is registered in three places that the compiler cannot
// tie together: its config type (config.AllProviders), the CLI's
// adapters (providers), and the runtime backend that detect, ctr,
// stop and friends resolve a kubeconfig context to
// (cluster.AllBackends). One that is missing from any of them loads
// and validates and then cannot be provisioned, or can be provisioned
// and then not be found.
func TestProviders_RegisteredEverywhere(t *testing.T) {
	backends := map[string]bool{}
	for _, b := range cluster.AllBackends {
		backends[string(b)] = true
	}
	for _, name := range config.AllProviders {
		ops, ok := providers[name]
		if config.ConfigOnly(name) {
			// The day a provisioner lands, this is what says to take
			// the provider out of the config-only set.
			if ok || backends[name] {
				t.Errorf("provider %q is marked config-only but has CLI adapters (%v) or a backend (%v)", name, ok, backends[name])
			}
			continue
		}
		if !ok {
			t.Errorf("provider %q has a config type but no entry in the CLI's providers table", name)
			continue
		}
		if ops.provision == nil || ops.teardown == nil || ops.hostPorts == nil || ops.stop == nil {
			t.Errorf("provider %q: incomplete providerOps %+v", name, ops)
		}
		if !backends[name] {
			t.Errorf("provider %q is not in cluster.AllBackends, so a cluster it provisions cannot be looked up", name)
		}
		if config.NewProviderConfig(name).Common() == nil {
			t.Errorf("provider %q: config type does not embed CommonConfig", name)
		}
	}
	for name := range providers {
		if config.NewProviderConfig(name) == nil {
			t.Errorf("providers table has %q, which has no config type", name)
		}
	}
}

// The inventory record is what `teardown` without -c lists and what
// preflight blames a port conflict on.
func TestProviders_HostPorts(t *testing.T) {
	forwards := []config.PortForward{{Host: "6443", Guest: "6443"}, {Host: "", Guest: "8080"}}
	q := &config.QEMUConfig{CommonConfig: config.CommonConfig{PortForwards: forwards}, SSHPort: "2222"}
	if got := providers[config.ProviderQEMU].hostPorts(q); strings.Join(got, ",") != "6443,2222" {
		t.Errorf("qemu binds its forwards and the ssh port: %v", got)
	}
	d := &config.DockerConfig{CommonConfig: config.CommonConfig{PortForwards: forwards}}
	if got := providers[config.ProviderDocker].hostPorts(d); strings.Join(got, ",") != "6443" {
		t.Errorf("docker: a forward without a host port is assigned by docker and not recorded: %v", got)
	}
	if got := providers[config.ProviderHetzner].hostPorts(&config.HetznerConfig{}); len(got) != 0 {
		t.Errorf("a remote server binds nothing on this host: %v", got)
	}
}
