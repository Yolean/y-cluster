package main

import (
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Yolean/y-cluster/pkg/buildctl"
	"github.com/Yolean/y-cluster/pkg/dockerhost"
)

func buildctlCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "buildctl [buildctl flags] <command> ...",
		Short: "BuildKit's buildctl, at the dockerhost guest unless told otherwise",
		Long: `BuildKit's buildctl ` + buildctl.Version + `, compiled in, the release the
dockerhost guest runs. All arguments go to buildctl as they are;
` + "`y-cluster buildctl --help`" + ` is buildctl's help.

The buildkitd it talks to, first match wins:

  --addr in the arguments          as given
  BUILDKIT_HOST                    as given, with --tlsdir $BUILDKIT_TLS_DIR
                                   unless the arguments have --tlsdir
  a provisioned dockerhost guest   its address and client directory, the
                                   values ` + "`y-cluster dockerhost env`" + ` exports;
                                   an error naming the provision command
                                   when the guest does not run
  otherwise                        buildctl's default socket`,
		DisableFlagParsing: true,
		SilenceUsage:       true,
		RunE: func(cmd *cobra.Command, args []string) error {
			global, err := buildctlGlobals(args, os.Getenv, dockerhostDir)
			if err != nil {
				return err
			}
			buildctl.Main(append(append([]string{"buildctl"}, global...), args...))
			return nil
		},
	}
}

// buildctlGlobals returns the global flags to put before args, per
// the precedence in buildctlCmd's help.
func buildctlGlobals(args []string, getenv func(string) string, dir func() (string, error)) ([]string, error) {
	if hasFlag(args, "addr") {
		return nil, nil
	}
	if getenv("BUILDKIT_HOST") != "" {
		if tlsDir := getenv("BUILDKIT_TLS_DIR"); tlsDir != "" && !hasFlag(args, "tlsdir") {
			return []string{"--tlsdir", tlsDir}, nil
		}
		return nil, nil
	}
	d, err := dir()
	if err != nil {
		return nil, err
	}
	addr, tlsDir, found, err := dockerhost.BuildkitClient(d)
	if err != nil || !found {
		return nil, err
	}
	global := []string{"--addr", addr}
	if !hasFlag(args, "tlsdir") {
		global = append(global, "--tlsdir", tlsDir)
	}
	return global, nil
}

// hasFlag reports -name or --name, alone or with =value, before any "--".
func hasFlag(args []string, name string) bool {
	for _, a := range args {
		if a == "--" {
			return false
		}
		f := strings.TrimPrefix(strings.TrimPrefix(a, "-"), "-")
		if f != a && (f == name || strings.HasPrefix(f, name+"=")) {
			return true
		}
	}
	return false
}
