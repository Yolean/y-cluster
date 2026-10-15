package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"go.uber.org/zap"

	"github.com/Yolean/y-cluster/pkg/kubeconfig"
	"github.com/Yolean/y-cluster/pkg/provision/config"
	"github.com/Yolean/y-cluster/pkg/provision/host"
	"github.com/Yolean/y-cluster/pkg/provision/k3s"
	"github.com/Yolean/y-cluster/pkg/provision/platform"
	"github.com/Yolean/y-cluster/pkg/provision/qemu"
)

// The three steps qemu provision is composed of after the VM is up,
// as verbs of their own for a k3s host y-cluster did not create
// (provider: host). A qemu config is accepted too: it gives the bundle
// and the platform its provision would have installed.

func k3sCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "k3s",
		Short: "k3s node artifacts for a host y-cluster does not manage",
	}
	cmd.AddCommand(k3sBundleCmd())
	return cmd
}

func k3sBundleCmd() *cobra.Command {
	var (
		configDir         string
		outDir            string
		withoutRegistries bool
	)
	cmd := &cobra.Command{
		Use:   "bundle",
		Short: "Write the files a k3s server node needs, with a manifest, for any root-capable tool to apply",
		Long: `Writes into --out everything y-cluster puts on a k3s server node
before it installs the platform, without contacting the node and
without root:

  k3s                              the pinned binary (airgap install)
  k3s-airgap-images-<arch>.tar.zst its images (airgap install)
  install.sh                       the installer at the release tag
  config.yaml                      the server settings, for
                                   /etc/rancher/k3s/config.yaml
  registries.yaml                  when registries are configured,
                                   unless --without-registries
  manifest.yaml                    per file its destination, mode and
                                   sha256; the installer's environment;
                                   the commands to run after it

Apply it as root, in this order: install each file at its dest with
its mode (creating missing parent directories 0755), run install.sh
with the manifest's install.env, run each postInstall command. The
installer starts k3s. Nothing is written to config.yaml.d/, which
k3s reads after config.yaml: drop-ins there belong to whatever else
configures the host. Without registries.yaml in the bundle the
host's own file, if any, stays as it is.

Then read /etc/rancher/k3s/k3s.yaml off the node and pipe it to
'y-cluster kubeconfig import', and run 'y-cluster platform install'.

-c takes a provider host config, or a qemu config (the bundle its
provision applies).`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			logger := loggerFromContext(cmd.Context())
			loaded, err := loadProvision(configDir)
			if err != nil {
				return err
			}
			var spec k3s.BundleSpec
			switch c := loaded.(type) {
			case *config.HostConfig:
				spec, err = host.BundleSpec(c, withoutRegistries)
			case *config.QEMUConfig:
				spec, err = qemu.FromConfig(c).K3sBundleSpec()
				if withoutRegistries {
					spec.Registries = nil
				}
			default:
				return fmt.Errorf("k3s bundle takes a provider %q or %q config, got %q", config.ProviderHost, config.ProviderQEMU, loaded.Common().Provider)
			}
			if err != nil {
				return err
			}
			if _, err := k3s.WriteBundle(cmd.Context(), spec, outDir, logger); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), filepath.Join(outDir, k3s.BundleManifestFile))
			return nil
		},
	}
	cmd.Flags().StringVarP(&configDir, "config", "c", "", "directory containing y-cluster-provision.yaml")
	cmd.Flags().StringVar(&outDir, "out", "", "directory to write the bundle into: new, empty, or holding an earlier bundle")
	cmd.Flags().BoolVar(&withoutRegistries, "without-registries", false, "leave registries.yaml out even when registries are configured")
	for _, f := range []string{"config", "out"} {
		if err := cmd.MarkFlagRequired(f); err != nil {
			panic(err)
		}
	}
	return cmd
}

func platformCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "platform",
		Short: "The cluster software y-cluster installs on k3s, through a kube context",
	}
	cmd.AddCommand(platformInstallCmd())
	return cmd
}

// errDrift is platform install --check finding something to change.
var errDrift = errors.New("platform drift")

func platformInstallCmd() *cobra.Command {
	var (
		configDir   string
		contextName string
		check       bool
	)
	cmd := &cobra.Command{
		Use:   "install",
		Short: "Install Cilium, local-path storage, Envoy Gateway and cert-manager on an existing k3s context",
		Long: `Installs what provision installs on a k3s cluster, with the same
versions, options and readiness waits, through a kubeconfig context:
Cilium (and waits for the node to be Ready), the local-path
provisioner, Envoy Gateway with merged Gateways, and cert-manager with
y-cluster's CA ClusterIssuer. gateway.skip and certManager.skip leave
those out as they do at provision.

The k3s server must run with the settings of 'y-cluster k3s bundle'
(no flannel, no network policy controller, traefik and local-storage
disabled), and the context must exist ('y-cluster kubeconfig import').

Every step is a server-side apply, so a second run on an installed
cluster changes nothing. --check applies nothing: it prints what an
install would change (kubectl diff --server-side per manifest) and
exits non-zero when anything would.

-c takes a provider host config, or a qemu config (what its provision
installs). --context overrides the config's context.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			logger := loggerFromContext(cmd.Context())
			loaded, err := loadProvision(configDir)
			if err != nil {
				return err
			}
			var opts platform.Options
			switch c := loaded.(type) {
			case *config.HostConfig:
				opts = host.PlatformOptions(c, logger)
			case *config.QEMUConfig:
				opts = qemu.FromConfig(c).PlatformOptions(logger)
			default:
				return fmt.Errorf("platform install takes a provider %q or %q config, got %q", config.ProviderHost, config.ProviderQEMU, loaded.Common().Provider)
			}
			if contextName != "" {
				opts.Context = contextName
			}
			if check {
				drift, err := platform.Check(cmd.Context(), opts, cmd.OutOrStdout())
				if err != nil {
					return err
				}
				if len(drift) > 0 {
					cmd.SilenceUsage = true
					return fmt.Errorf("%w: an install would change %s", errDrift, strings.Join(drift, ", "))
				}
				logger.Info("platform in sync", zap.String("context", opts.Context))
				return nil
			}
			if err := platform.Install(cmd.Context(), opts); err != nil {
				return err
			}
			logger.Info("platform ready", zap.String("context", opts.Context))
			return nil
		},
	}
	cmd.Flags().StringVarP(&configDir, "config", "c", "", "directory containing y-cluster-provision.yaml")
	cmd.Flags().StringVar(&contextName, "context", "", "kubeconfig context to install on (default: the config's context)")
	cmd.Flags().BoolVar(&check, "check", false, "report what an install would change, apply nothing, exit non-zero on drift")
	if err := cmd.MarkFlagRequired("config"); err != nil {
		panic(err)
	}
	return cmd
}

func kubeconfigCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "kubeconfig",
		Short: "Manage the k3s contexts in $KUBECONFIG",
	}
	cmd.AddCommand(kubeconfigImportCmd())
	return cmd
}

func kubeconfigImportCmd() *cobra.Command {
	var (
		contextName string
		clusterName string
		server      string
	)
	cmd := &cobra.Command{
		Use:   "import",
		Short: "Merge a k3s node's k3s.yaml from stdin into $KUBECONFIG",
		Long: `Reads the kubeconfig k3s writes (/etc/rancher/k3s/k3s.yaml) from
stdin, points it at --server and merges it into $KUBECONFIG under
--context, replacing an earlier entry of that name. Reading the file
is the caller's: on a host where it is root-only, for example

  ssh <host> sudo cat /etc/rancher/k3s/k3s.yaml | \
    y-cluster kubeconfig import --context <ctx> --server https://127.0.0.1:16443

with an ssh tunnel from local port 16443 to the node's 6443. The
server's host must be in the apiserver's certificate: 127.0.0.1 and
the node's own addresses are, others need tlsSANs.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			logger := loggerFromContext(cmd.Context())
			if f, ok := cmd.InOrStdin().(*os.File); ok {
				if info, err := f.Stat(); err == nil && info.Mode()&os.ModeCharDevice != 0 {
					return errors.New("stdin is a terminal; pipe the node's k3s.yaml into this command")
				}
			}
			raw, err := io.ReadAll(cmd.InOrStdin())
			if err != nil {
				return err
			}
			if clusterName == "" {
				clusterName = contextName
			}
			m, err := kubeconfig.FromEnv(contextName, clusterName, logger)
			if err != nil {
				return err
			}
			if err := k3s.ImportKubeconfig(m, raw, server); err != nil {
				return err
			}
			logger.Info("kubeconfig context imported", zap.String("context", contextName), zap.String("server", server), zap.String("kubeconfig", m.Path))
			return nil
		},
	}
	cmd.Flags().StringVar(&contextName, "context", "", "context name to write")
	cmd.Flags().StringVar(&clusterName, "cluster", "", "cluster and user entry name (default: the context name)")
	cmd.Flags().StringVar(&server, "server", "", "apiserver address as the operator reaches it: https://host:port")
	for _, f := range []string{"context", "server"} {
		if err := cmd.MarkFlagRequired(f); err != nil {
			panic(err)
		}
	}
	return cmd
}
