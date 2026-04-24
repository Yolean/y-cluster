package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/Yolean/y-cluster/pkg/kustomize/traverse"
)

const usage = `Usage: kustomize-traverse [flags] <path>

Flags:
  -o, --output FORMAT   Output format (default: dirs)
                        dirs       One local directory per line, depth-first
                        namespace  Print only the resolved namespace
                        json       JSON object with dirs and namespace
  -q, --quiet           Suppress warnings (e.g. unresolvable remote refs)
`

type options struct {
	output string
	quiet  bool
	path   string
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	opts, code, err := parseArgs(args, stderr)
	if err != nil {
		fmt.Fprintln(stderr, err)
	}
	if code != 0 || err != nil {
		return code
	}

	var warn traverse.WarnFunc
	if !opts.quiet {
		warn = func(format string, a ...any) {
			fmt.Fprintf(stderr, "warning: "+format+"\n", a...)
		}
	}

	result, err := traverse.Walk(opts.path, warn)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}

	rels, err := result.RelDirs(opts.path)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}

	switch opts.output {
	case "dirs":
		for _, r := range rels {
			fmt.Fprintln(stdout, r)
		}
	case "namespace":
		if result.Namespace != "" {
			fmt.Fprintln(stdout, result.Namespace)
		}
	case "json":
		out := struct {
			Namespace string   `json:"namespace"`
			Dirs      []string `json:"dirs"`
		}{Namespace: result.Namespace, Dirs: rels}
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(&out); err != nil {
			fmt.Fprintf(stderr, "error: %v\n", err)
			return 1
		}
	default:
		fmt.Fprintf(stderr, "error: unknown output format: %s\n", opts.output)
		return 2
	}
	return 0
}

func parseArgs(args []string, stderr io.Writer) (options, int, error) {
	var opts options
	fs := flag.NewFlagSet("kustomize-traverse", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprint(stderr, usage) }
	fs.StringVar(&opts.output, "output", "dirs", "Output format")
	fs.StringVar(&opts.output, "o", "dirs", "Output format (alias)")
	fs.BoolVar(&opts.quiet, "quiet", false, "Suppress warnings")
	fs.BoolVar(&opts.quiet, "q", false, "Suppress warnings (alias)")

	if err := fs.Parse(args); err != nil {
		return opts, 2, nil
	}
	rest := fs.Args()
	if len(rest) == 0 {
		fs.Usage()
		return opts, 2, fmt.Errorf("error: missing <path> argument")
	}
	if len(rest) > 1 {
		return opts, 2, fmt.Errorf("error: unexpected extra arguments: %v", rest[1:])
	}
	opts.path = rest[0]
	return opts, 0, nil
}
