package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strings"
	"syscall"

	"github.com/spf13/cobra"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"github.com/Yolean/y-cluster/pkg/dockerexec"
	"github.com/Yolean/y-cluster/pkg/provision/config"
	"github.com/Yolean/y-cluster/pkg/provision/docker"
	"github.com/Yolean/y-cluster/pkg/provision/multipass"
	"github.com/Yolean/y-cluster/pkg/provision/qemu"
	"github.com/Yolean/y-cluster/pkg/yconverge"
)

// version is the human-readable release tag. Default "dev" applies
// to any local build; release tooling overrides via
// `-ldflags '-X main.version=v0.4.0'`. The short git ref and an
// optional `-dirty` marker get appended at runtime from the VCS
// metadata Go's toolchain embeds in the binary -- so even a
// tagged release prints something like "v0.4.0 (abc1234)" and
// a dev build is "dev (abc1234-dirty)" without any extra ldflags.
var version = "dev"

// shortSHALen is how many hex chars of the git revision we
// surface. 7 matches the git default and what skaffold-style
// version strings use.
const shortSHALen = 7

// formatVersion combines the release tag with the VCS metadata
// from debug.BuildInfo into a single user-facing string.
//
// Output shape (cobra prints "<binary> version <this>"):
//
//	v0.4.0 (abc1234)        -- tagged, clean
//	v0.4.0 (abc1234-dirty)  -- tagged, uncommitted local changes
//	dev (abc1234)           -- untagged build, clean
//	dev (abc1234-dirty)     -- untagged, uncommitted local changes
//	v0.4.0                  -- no VCS info (built from tarball)
//
// Pure function on its inputs so it's easy to test against
// synthetic BuildSetting slices without spinning up a real build.
func formatVersion(release string, settings []debug.BuildSetting) string {
	var rev string
	var dirty bool
	for _, s := range settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	if rev == "" {
		return release
	}
	short := rev
	if len(short) > shortSHALen {
		short = short[:shortSHALen]
	}
	if dirty {
		short += "-dirty"
	}
	return release + " (" + short + ")"
}

// versionString resolves the runtime version once, at root-command
// construction time. debug.ReadBuildInfo is guaranteed available
// for any binary built with the go toolchain; if it's somehow not,
// we fall back to the bare release tag.
func versionString() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return version
	}
	return formatVersion(version, info.Settings)
}

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	cmd := rootCmd()

	// When invoked as kubectl-yconverge, act as the yconverge subcommand directly
	if strings.HasPrefix(filepath.Base(os.Args[0]), "kubectl-yconverge") {
		cmd = yconvergePluginCmd()
	}

	if err := cmd.ExecuteContext(ctx); err != nil {
		os.Exit(1)
	}
}

func rootCmd() *cobra.Command {
	var verbose bool

	root := &cobra.Command{
		Use:     binaryName(),
		Short:   "Idempotent Kubernetes convergence with dependency ordering and checks",
		Version: versionString(),
		PersistentPreRun: func(cmd *cobra.Command, args []string) {
			logger := newLogger(verbose)
			cmd.SetContext(withLogger(cmd.Context(), logger))
		},
	}
	// Anchor --version output to the binary identity rather than
	// cobra's default "<Use> version ...". The plugin entry point
	// overrides this with an "(as kubectl-yconverge)" suffix to
	// preserve the invocation-path signal.
	root.SetVersionTemplate("y-cluster version {{.Version}}\n")

	root.PersistentFlags().BoolVarP(&verbose, "verbose", "v", false, "debug logging")

	root.AddCommand(yconvergeCmd())
	root.AddCommand(provisionCmd())
	root.AddCommand(teardownCmd())
	root.AddCommand(pauseCmd())
	root.AddCommand(resumeCmd())
	root.AddCommand(stopCmd())
	root.AddCommand(startCmd())
	root.AddCommand(prepareExportCmd())
	root.AddCommand(exportCmd())
	root.AddCommand(importCmd())
	root.AddCommand(serveCmd())
	root.AddCommand(imagesCmd())
	root.AddCommand(manifestsCmd())
	root.AddCommand(detectCmd())
	root.AddCommand(ctrCmd())
	root.AddCommand(crictlCmd())
	root.AddCommand(cacheCmd())
	root.AddCommand(echoCmd())
	root.AddCommand(gatewayCmd())
	root.AddCommand(localstorageCmd())

	return root
}

func yconvergePluginCmd() *cobra.Command {
	cmd := yconvergeCmd()
	cmd.Use = "kubectl-yconverge"
	cmd.Short = "kubectl plugin: apply a kustomize base with dependency resolution and checks"
	// Plugin invocations bypass rootCmd, so the Version, --verbose,
	// and PersistentPreRun the plugin still wants have to be wired
	// here too. `kubectl yconverge --version` would otherwise error
	// "unknown flag --version" because cobra needs Version on the
	// command --version is dispatched to.
	cmd.Version = versionString()
	// The version line names the binary that actually shipped
	// (y-cluster), with the invocation path called out so a
	// stack trace pasted into a bug report still says how the
	// process was reached.
	cmd.SetVersionTemplate("y-cluster version {{.Version}} as kubectl-yconverge\n")
	var verbose bool
	cmd.PersistentFlags().BoolVarP(&verbose, "verbose", "v", false, "debug logging")
	cmd.PersistentPreRun = func(c *cobra.Command, args []string) {
		logger := newLogger(verbose)
		c.SetContext(withLogger(c.Context(), logger))
	}
	return cmd
}

func yconvergeCmd() *cobra.Command {
	var opts yconverge.Options

	cmd := &cobra.Command{
		Use:   "yconverge",
		Short: "Apply a kustomize base with dependency resolution and checks",
		Long: `yconverge is the convergence primitive: apply a kustomize base, then
run the checks that verify the apply landed.

Two mechanisms control behaviour, deliberately separate:

  Ordering across modules  -- CUE imports in yconverge.cue.
    Each import is converged as its OWN yconverge invocation:
    its own apply, its own checks, before the importing base.
    Use this to express "mysql must be healthy before keycloak
    starts." Tree-wide aggregation pulls in CUE files reachable
    via kustomize resources/components/bases, so an overlay
    inherits its base's imports automatically.

  Checks after one apply   -- yconverge.cue files anywhere in
    the kustomize tree of the target. Every check runs after
    the apply that produced its resources. An overlay's checks
    include the base's, since both files are in the same tree.

Subcommands of the yconverge primitive:

  --print-deps      print the topological order without applying
  --checks-only     run checks against an already-applied state
                    (also propagates to deps so you can verify a
                    whole chain without re-applying anywhere)
  --skip-checks     apply but don't verify (useful in
                    --dry-run=server)
  --dry-run=server  validate against the apiserver, no mutation
  -l, --selector    kubectl label selector applied to every apply
                    invocation. ANDed with the converge-mode label
                    routing so a -l app=foo run still picks the right
                    apply strategy per resource. Propagates to deps so
                    a filtered run is filtered everywhere.

Define checks in yconverge.cue:

  package my_base
  import "yolean.se/ystack/yconverge/verify"
  step: verify.#Step & {
      checks: [
          {kind: "rollout", resource: "deployment/my-app", timeout: "120s"},
          {kind: "wait",    resource: "ns/dev", for: "jsonpath={.status.phase}=Active"},
          {kind: "exec",    command: "curl -sf http://$NAMESPACE/", description: "app responds"},
      ]
  }

Three check kinds: wait (kubectl-wait semantics, condition= or
jsonpath=), rollout (apps/v1 rollout-status semantics on
Deployment / StatefulSet / DaemonSet), exec (arbitrary shell
retried until timeout). Exec sees $CONTEXT and $NAMESPACE.

Symlink the binary as kubectl-yconverge for kubectl-plugin use.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			logger := loggerFromContext(cmd.Context())
			result, err := yconverge.Run(cmd.Context(), opts, logger)
			if err != nil {
				logger.Error("convergence failed", zap.Error(err))
				return err
			}
			if opts.PrintDeps {
				for _, step := range result.Steps {
					fmt.Fprintln(cmd.OutOrStdout(), step)
				}
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&opts.Context, "context", "", "Kubernetes context name (required)")
	cmd.Flags().StringVarP(&opts.KustomizeDir, "kustomize-dir", "k", "", "path to kustomize base (required)")
	cmd.Flags().StringVar(&opts.DryRun, "dry-run", "", "dry-run mode (server|none)")
	cmd.Flags().BoolVar(&opts.ChecksOnly, "checks-only", false, "run checks without applying")
	cmd.Flags().BoolVar(&opts.PrintDeps, "print-deps", false, "print dependency order and exit")
	cmd.Flags().BoolVar(&opts.SkipChecks, "skip-checks", false, "skip checks after apply")
	cmd.Flags().StringVarP(&opts.Selector, "selector", "l", "",
		"label selector (kubectl -l form) applied to every kubectl invocation; propagates to dependencies")

	if err := cmd.MarkFlagRequired("context"); err != nil {
		panic(err)
	}
	if err := cmd.MarkFlagRequired("kustomize-dir"); err != nil {
		panic(err)
	}

	return cmd
}

// binaryName returns "y-cluster" or "kubectl-yconverge" depending on
// how the binary was invoked, supporting kubectl plugin symlinks.
func binaryName() string {
	name := filepath.Base(os.Args[0])
	if strings.HasPrefix(name, "kubectl-") {
		return name
	}
	return "y-cluster"
}

func newLogger(verbose bool) *zap.Logger {
	level := zapcore.InfoLevel
	if verbose {
		level = zapcore.DebugLevel
	}
	cfg := zap.Config{
		Level:            zap.NewAtomicLevelAt(level),
		Encoding:         "console",
		EncoderConfig:    zap.NewDevelopmentEncoderConfig(),
		OutputPaths:      []string{"stderr"},
		ErrorOutputPaths: []string{"stderr"},
	}
	logger, err := cfg.Build()
	if err != nil {
		panic(err)
	}
	return logger
}

type loggerKey struct{}

func withLogger(ctx context.Context, logger *zap.Logger) context.Context {
	return context.WithValue(ctx, loggerKey{}, logger)
}

func loggerFromContext(ctx context.Context) *zap.Logger {
	if l, ok := ctx.Value(loggerKey{}).(*zap.Logger); ok {
		return l
	}
	return zap.NewNop()
}

// loadProvision is shared by provision/teardown/export/import. Each
// subcommand reads y-cluster-provision.yaml from its -c <dir>;
// provider-specific data dispatches via config.LoadProvision.
func loadProvision(dir string) (any, error) {
	if dir == "" {
		return nil, fmt.Errorf("--config (-c) is required")
	}
	return config.LoadProvision(dir)
}

// asQEMU narrows for subcommands that are qemu-specific (export,
// import: VMDK conversion makes no sense for docker).
func asQEMU(cfg any) (*config.QEMUConfig, error) {
	q, ok := cfg.(*config.QEMUConfig)
	if !ok {
		return nil, fmt.Errorf("provider %T not supported by this subcommand (qemu only)", cfg)
	}
	return q, nil
}

func provisionCmd() *cobra.Command {
	var configDir string
	cmd := &cobra.Command{
		Use:   "provision",
		Short: "Create a local Kubernetes cluster",
		Long: `Create a local Kubernetes cluster from y-cluster-provision.yaml in
the -c directory. The config's 'provider:' field selects the
backend (qemu, docker, or multipass). When 'provider:' is omitted,
y-cluster runs a runtime probe and picks one:

  multipass CLI + daemon reachable         -> multipass
  Linux + /dev/kvm + qemu-system-x86_64    -> qemu
  docker CLI + 'docker info' OK            -> docker

multipass wins ahead of qemu/docker because it's the only macOS
path to a real VM, and on Linux it isn't installed by default --
its presence means the user explicitly chose it.

qemu wins over docker on Linux because it has the full
disk-and-appliance feature surface (cloud-init seed, persistent
disk, snapshots) that the docker provisioner doesn't implement.
On a host where no probe matches, provision errors with a
message naming what was checked.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			logger := loggerFromContext(cmd.Context())
			loaded, err := loadProvision(configDir)
			if err != nil {
				return err
			}
			switch v := loaded.(type) {
			case *config.QEMUConfig:
				rt := qemu.FromConfig(v)
				if err := qemu.CheckPrerequisites(); err != nil {
					return err
				}
				if _, err := qemu.Provision(cmd.Context(), rt, logger); err != nil {
					return err
				}
				logger.Info("cluster ready",
					zap.String("ssh", fmt.Sprintf("ssh -p %s -i %s ystack@localhost",
						rt.SSHPort, filepath.Join(rt.CacheDir, rt.Name+"-ssh"))),
				)
				return nil
			case *config.DockerConfig:
				if _, err := docker.Provision(cmd.Context(), *v, logger); err != nil {
					return err
				}
				logger.Info("cluster ready",
					zap.String("docker", fmt.Sprintf("docker exec -it %s sh", v.Name)),
				)
				return nil
			case *config.MultipassConfig:
				rt := multipass.FromConfig(v)
				if _, err := multipass.Provision(cmd.Context(), rt, logger); err != nil {
					return err
				}
				logger.Info("cluster ready",
					zap.String("multipass", fmt.Sprintf("multipass shell %s", rt.Name)),
				)
				return nil
			default:
				return fmt.Errorf("provider %T not supported by provision", v)
			}
		},
	}
	cmd.Flags().StringVarP(&configDir, "config", "c", "", "directory containing y-cluster-provision.yaml")
	if err := cmd.MarkFlagRequired("config"); err != nil {
		panic(err)
	}
	return cmd
}

func teardownCmd() *cobra.Command {
	var (
		configDir string
		keepDisk  bool
	)
	cmd := &cobra.Command{
		Use:   "teardown",
		Short: "Stop and remove the local cluster",
		RunE: func(cmd *cobra.Command, args []string) error {
			logger := loggerFromContext(cmd.Context())
			loaded, err := loadProvision(configDir)
			if err != nil {
				return err
			}
			switch v := loaded.(type) {
			case *config.QEMUConfig:
				return qemu.TeardownConfig(qemu.FromConfig(v), keepDisk, logger)
			case *config.DockerConfig:
				// docker has no persistent disk; keepDisk is
				// a no-op for this provider.
				cluster, err := docker.Provision(cmd.Context(), *v, logger)
				if err != nil {
					// Even if Provision fails (container already
					// gone, etc.), Teardown's docker rm -f is
					// idempotent.
					return (&dockerNamedTeardown{name: v.Name, ctx: v.Context, logger: logger}).run()
				}
				return cluster.Teardown(false)
			case *config.MultipassConfig:
				return multipass.TeardownConfig(multipass.FromConfig(v), keepDisk, logger)
			default:
				return fmt.Errorf("provider %T not supported by teardown", v)
			}
		},
	}
	cmd.Flags().StringVarP(&configDir, "config", "c", "", "directory containing y-cluster-provision.yaml")
	cmd.Flags().BoolVar(&keepDisk, "keep-disk", false, "preserve disk image / VM for faster re-provision (qemu, multipass)")
	if err := cmd.MarkFlagRequired("config"); err != nil {
		panic(err)
	}
	return cmd
}

// dockerNamedTeardown is a fallback for when a teardown is asked
// for a container that we can't fully connect to (e.g. exited, no
// kubeconfig). Just removes the named container and cleans up the
// context entry in the host's kubeconfig.
type dockerNamedTeardown struct {
	name   string
	ctx    string
	logger *zap.Logger
}

func (k *dockerNamedTeardown) run() error {
	cli, err := dockerexec.New()
	if err != nil {
		return fmt.Errorf("docker client: %w", err)
	}
	defer func() { _ = cli.Close() }()
	return dockerexec.Remove(context.Background(), cli, k.name)
}


func importCmd() *cobra.Command {
	var configDir string
	cmd := &cobra.Command{
		Use:   "import <input.vmdk>",
		Short: "Import a VMware appliance as the cluster disk",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			loaded, err := loadProvision(configDir)
			if err != nil {
				return err
			}
			q, err := asQEMU(loaded)
			if err != nil {
				return err
			}
			rc := qemu.FromConfig(q)
			return qemu.ImportVMDK(args[0], filepath.Join(rc.CacheDir, rc.Name+".qcow2"))
		},
	}
	cmd.Flags().StringVarP(&configDir, "config", "c", "", "directory containing y-cluster-provision.yaml")
	if err := cmd.MarkFlagRequired("config"); err != nil {
		panic(err)
	}
	return cmd
}
