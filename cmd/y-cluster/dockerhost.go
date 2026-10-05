package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/spf13/cobra"
	"go.uber.org/zap"

	"github.com/Yolean/y-cluster/pkg/dockerhost"
)

// dockerhostCmd is `y-cluster dockerhost`: dockerd and buildkitd in a
// KVM guest, for machines that must not run a container daemon on the
// host. See DOCKERHOST.md.
func dockerhostCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "dockerhost",
		Short: "Run dockerd and buildkitd in a KVM guest and print the client environment",
		Long: `dockerhost runs dockerd and buildkitd in one KVM guest per machine, made by
the qemu provider, and clients on the host reach them over TLS with
client certificates:

  y-cluster dockerhost provision        # idempotent; run before builds and tests
  eval "$(y-cluster dockerhost env)"    # DOCKER_HOST, DOCKER_TLS_VERIFY, DOCKER_CERT_PATH,
                                        # BUILDKIT_HOST, BUILDKIT_TLS_DIR
  docker run ...; y-build ...; mvn verify (Testcontainers) ...

Every session of this user on the machine shares the one daemon, its
images and its build cache, as with host Docker. The guest is root only
inside itself; its client certificate is the credential, in a 0700
directory.

The guest sits on a tap device that root prepared once (DOCKERHOST.md,
One-time root setup), named with its subnet in
~/.config/y-cluster/dockerhost.yaml:

  network:
    ifname: ycl1
    guestAddress: 10.88.1.2/24

y-cluster never creates network devices, never needs root and never
changes the host's firewall. State lives in ~/.cache/y-cluster-dockerhost
($` + dockerhost.DirEnv + ` overrides it).`,
	}
	cmd.AddCommand(dockerhostProvisionCmd(), dockerhostTeardownCmd(), dockerhostEnvCmd(), dockerhostStatusCmd())
	return cmd
}

func dockerhostDir() (string, error) { return dockerhost.DefaultDir() }

func dockerhostProvisionCmd() *cobra.Command {
	var configPath string
	cmd := &cobra.Command{
		Use:   "provision",
		Short: "Make sure the dockerhost guest runs and answers; idempotent",
		Long: `Makes sure this machine's dockerhost guest runs and both daemons answer
over TLS, then renews the guest's lease against its idle reaper. Cheap
when the guest is healthy: run it before every build or test run.

  - A healthy guest is reused.
  - A guest its idle reaper powered off (idleTimeout without use) boots
    again from its disk; images and build cache are still there.
  - A guest past maxAge is replaced by a new one from the newest signed
    Ubuntu cloud image as soon as nobody uses it, and a day later in any
    case. So is a guest whose daemon versions this y-cluster no longer
    pins, once nobody uses it.
  - A guest that does not answer, or was made with another network or
    size, is replaced.

Replacing a guest discards its images and build cache, and issues a new
CA, server and client certificate.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			logger := loggerFromContext(cmd.Context())
			if configPath == "" {
				p, err := dockerhost.DefaultConfigPath()
				if err != nil {
					return err
				}
				configPath = p
			}
			cfg, err := dockerhost.LoadConfig(configPath)
			if errors.Is(err, dockerhost.ErrNoConfig) {
				return fmt.Errorf("%w\nWrite it once per machine, after root prepared the tap device (DOCKERHOST.md, One-time root setup), e.g.:\n  network:\n    ifname: ycl1\n    guestAddress: 10.88.1.2/24", err)
			}
			if err != nil {
				return err
			}
			dir, err := dockerhostDir()
			if err != nil {
				return err
			}
			res, err := dockerhost.Provision(cmd.Context(), dockerhost.Options{Dir: dir, Config: cfg}, logger)
			if err != nil {
				return err
			}
			fields := []zap.Field{
				zap.String("outcome", string(res.Outcome)),
				zap.String("docker", res.State.DockerHostURL()),
				zap.String("buildkit", res.State.BuildkitHostURL()),
			}
			if res.Replaced != "" {
				fields = append(fields, zap.String("replacedBecause", res.Replaced))
			}
			logger.Info("dockerhost ready; eval \"$(y-cluster dockerhost env)\"", fields...)
			return nil
		},
	}
	cmd.Flags().StringVarP(&configPath, "config", "c", "", "dockerhost configuration file (default ~/.config/y-cluster/dockerhost.yaml)")
	return cmd
}

func dockerhostTeardownCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "teardown",
		Short: "Stop and remove the dockerhost guest, its disk and its certificates",
		Long: `Stops the guest (a graceful poweroff first) and removes its disk, its
certificates and its state. Images and build cache in the guest are gone
with it. Shells that eval'd the environment can run
` + "`eval \"$(y-cluster dockerhost env)\"`" + ` again to unset it.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			dir, err := dockerhostDir()
			if err != nil {
				return err
			}
			return dockerhost.Teardown(dir, loggerFromContext(cmd.Context()))
		},
	}
}

func dockerhostEnvCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "env",
		Short: "Print the client environment for the running dockerhost guest",
		Long: `Prints shell lines for ` + "`eval \"$(y-cluster dockerhost env)\"`" + `:

  export DOCKER_HOST='tcp://<guest address>:2376'
  export DOCKER_TLS_VERIFY='1'
  export DOCKER_CERT_PATH='<client dir>'
  export BUILDKIT_HOST='tcp://<guest address>:8547'
  export BUILDKIT_TLS_DIR='<client dir>'

docker, Testcontainers (Java, Go, Node) and buildctl through ystack's
y-buildctl read these. Without a running guest it prints nothing, so a
machine with plain Docker, or a DOCKER_HOST set on purpose, is left as
it is; only a shell that still carries this dockerhost's variables gets
an unset line for them. Reads files only: fast, and safe in a shell
profile.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			dir, err := dockerhostDir()
			if err != nil {
				return err
			}
			out, err := dockerhost.Env(dir, os.Getenv)
			if err != nil {
				return err
			}
			_, err = io.WriteString(cmd.OutOrStdout(), out)
			return err
		},
	}
}

func dockerhostStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show the dockerhost guest and ask both daemons; exits non-zero unless healthy",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			dir, err := dockerhostDir()
			if err != nil {
				return err
			}
			s, err := dockerhost.GetStatus(cmd.Context(), dir)
			if err != nil {
				return err
			}
			printDockerhostStatus(cmd.OutOrStdout(), s)
			if !s.Healthy() {
				cmd.SilenceUsage = true
				return fmt.Errorf("dockerhost is not running and healthy")
			}
			return nil
		},
	}
}

func printDockerhostStatus(out io.Writer, s dockerhost.Status) {
	if s.State == nil {
		fmt.Fprintf(out, "dockerhost: none (state dir %s)\n", s.Dir)
		return
	}
	st := s.State
	now := time.Now()
	state := "stopped"
	if s.Running {
		state = fmt.Sprintf("running (qemu pid %d)", s.PID)
	} else if st.ReadyAt.IsZero() {
		state = "never finished its first boot"
	}
	fmt.Fprintf(out, "dockerhost:  %s\n", state)
	if st.TestForwards != nil {
		fmt.Fprintf(out, "network:     test harness only: user-mode networking, forwards on 127.0.0.1 (published container ports are unreachable)\n")
	} else {
		fmt.Fprintf(out, "network:     tap %s, guest %s, gateway %s, dns %v\n", st.Network.Ifname, st.Network.GuestAddress, st.Network.Gateway, st.Network.DNS)
	}
	fmt.Fprintf(out, "created:     %s (age %s, maxAge %s)\n", st.CreatedAt.Format(time.RFC3339), now.Sub(st.CreatedAt).Round(time.Minute), st.MaxAge)
	if !st.BootedAt.IsZero() {
		fmt.Fprintf(out, "booted:      %s\n", st.BootedAt.Format(time.RFC3339))
	}
	fmt.Fprintf(out, "idle:        powers off after %s without use\n", st.IdleTimeout)
	if s.Activity != "" {
		fmt.Fprintf(out, "activity:    %s\n", s.Activity)
	}
	if h := s.Health; h != nil {
		docker := "ok, " + h.Docker
		if h.DockerErr != nil {
			docker = "FAILING: " + h.DockerErr.Error()
		}
		bk := "ok"
		if h.BuildkitErr != nil {
			bk = "FAILING: " + h.BuildkitErr.Error()
		}
		fmt.Fprintf(out, "dockerd:     %s  %s\n", st.DockerHostURL(), docker)
		fmt.Fprintf(out, "buildkitd:   %s  %s\n", st.BuildkitHostURL(), bk)
	} else {
		fmt.Fprintf(out, "dockerd:     %s\n", st.DockerHostURL())
		fmt.Fprintf(out, "buildkitd:   %s\n", st.BuildkitHostURL())
	}
	fmt.Fprintf(out, "client:      %s (certificates valid until %s)\n", s.ClientDir, st.CertNotAfter.Format(time.RFC3339))
	fmt.Fprintf(out, "pins:        docker %s, containerd %s, buildkit %s\n", st.Pins.Docker, st.Pins.Containerd, st.Pins.BuildKit)
	fmt.Fprintf(out, "image:       %s sha256:%s, signed %s by %s\n", st.CloudImage.URL, st.CloudImage.SHA256, st.CloudImage.SignedAt.Format("2006-01-02"), st.CloudImage.SignedBy)
	if s.SSH != "" {
		fmt.Fprintf(out, "ssh:         %s\n", s.SSH)
	}
}
