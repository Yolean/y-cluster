package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"sigs.k8s.io/kustomize/api/types"
	"sigs.k8s.io/yaml"
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

	kust, _, err := loadKustomization(opts.path)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	if kust == nil {
		fmt.Fprintf(stderr, "error: no kustomization file in %s\n", opts.path)
		return 1
	}

	warn := func(format string, a ...any) {
		if !opts.quiet {
			fmt.Fprintf(stderr, "warning: "+format+"\n", a...)
		}
	}

	rootAbs, err := filepath.Abs(opts.path)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}

	dirs, err := collectDirs(rootAbs, warn)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	rels := make([]string, 0, len(dirs))
	for _, d := range dirs {
		rel, err := filepath.Rel(rootAbs, d)
		if err != nil {
			fmt.Fprintf(stderr, "error: %v\n", err)
			return 1
		}
		rels = append(rels, rel)
	}

	ns := resolveNamespace(rootAbs)

	switch opts.output {
	case "dirs":
		for _, r := range rels {
			fmt.Fprintln(stdout, r)
		}
	case "namespace":
		if ns != "" {
			fmt.Fprintln(stdout, ns)
		}
	case "json":
		out := struct {
			Namespace string   `json:"namespace"`
			Dirs      []string `json:"dirs"`
		}{Namespace: ns, Dirs: rels}
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

// loadKustomization reads the kustomization file in dir, returning the parsed
// struct, the file path that was read, and any error. Returns (nil, "", nil)
// if no kustomization file exists.
func loadKustomization(dir string) (*types.Kustomization, string, error) {
	for _, name := range []string{"kustomization.yaml", "kustomization.yml", "Kustomization"} {
		p := filepath.Join(dir, name)
		data, err := os.ReadFile(p)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, p, err
		}
		var k types.Kustomization
		if err := yaml.Unmarshal(data, &k); err != nil {
			return nil, p, fmt.Errorf("parse %s: %w", p, err)
		}
		k.FixKustomization()
		return &k, p, nil
	}
	return nil, "", nil
}

func hasKustomization(dir string) bool {
	for _, name := range []string{"kustomization.yaml", "kustomization.yml", "Kustomization"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			return true
		}
	}
	return false
}

// isRemote detects refs that are not local filesystem paths. kustomize
// accepts URLs (http://, https://, git://, ssh://), git+SSH forms
// (git@host:path), and shorthand like github.com/owner/repo//path. The
// common signal is either a scheme (://), git@ prefix, or a first path
// segment that looks like a domain (contains a dot).
func isRemote(entry string) bool {
	if strings.Contains(entry, "://") {
		return true
	}
	if strings.HasPrefix(entry, "git@") {
		return true
	}
	first := entry
	if i := strings.Index(entry, "/"); i >= 0 {
		first = entry[:i]
	}
	return strings.Contains(first, ".") && !strings.HasPrefix(first, ".")
}

// localBases returns the absolute paths of resources/components entries
// that resolve to local directories containing a kustomization file.
func localBases(dir string, k *types.Kustomization, warn func(string, ...any)) []string {
	var bases []string
	entries := append([]string{}, k.Resources...)
	entries = append(entries, k.Components...)
	for _, e := range entries {
		if isRemote(e) {
			continue
		}
		abs := filepath.Clean(filepath.Join(dir, e))
		info, err := os.Stat(abs)
		if err != nil {
			if warn != nil {
				warn("skipping unresolvable ref %q from %s", e, dir)
			}
			continue
		}
		if !info.IsDir() {
			continue
		}
		if !hasKustomization(abs) {
			continue
		}
		bases = append(bases, abs)
	}
	return bases
}

func collectDirs(rootAbs string, warn func(string, ...any)) ([]string, error) {
	visited := map[string]bool{}
	var results []string
	if err := walk(rootAbs, visited, &results, warn); err != nil {
		return nil, err
	}
	return results, nil
}

func walk(dirAbs string, visited map[string]bool, results *[]string, warn func(string, ...any)) error {
	if visited[dirAbs] {
		return nil
	}
	visited[dirAbs] = true
	k, _, err := loadKustomization(dirAbs)
	if err != nil {
		return err
	}
	if k == nil {
		return nil
	}
	for _, base := range localBases(dirAbs, k, warn) {
		if err := walk(base, visited, results, warn); err != nil {
			return err
		}
	}
	*results = append(*results, dirAbs)
	return nil
}

func resolveNamespace(dirAbs string) string {
	k, _, err := loadKustomization(dirAbs)
	if err != nil || k == nil {
		return ""
	}
	if k.Namespace != "" {
		return k.Namespace
	}
	bases := localBases(dirAbs, k, nil)
	if len(bases) == 1 {
		return resolveNamespace(bases[0])
	}
	return ""
}
