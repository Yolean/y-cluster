package yconverge

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"go.uber.org/zap"

	"github.com/Yolean/y-cluster/pkg/kustomize/traverse"
)

// Options configures a yconverge run.
type Options struct {
	Context      string // Kubernetes context name (required)
	KustomizeDir string // path to kustomize base (required)
	DryRun       string // "server" or "" (empty = real apply)
	ChecksOnly   bool   // skip apply, run checks only
	PrintDeps    bool   // print dependency order and exit
	SkipChecks   bool   // skip checks after apply

	// Selector is a `kubectl apply -l <selector>` filter applied to
	// every kubectl invocation in the apply phase. Empty (default)
	// means "no filter": every resource the kustomize tree renders
	// gets applied. Matches the syntax kubectl accepts directly --
	// equality, set, and inequality forms ("app=foo",
	// "app in (a,b)", "!app").
	//
	// The filter is ANDed with the converge-mode label routing in
	// applyGroups, so a `-l app=foo` user run still picks the right
	// apply strategy per resource: a `converge-mode=replace`,
	// `app=foo` resource takes the replace path; an
	// `app=foo` resource without a converge-mode label takes the
	// default path; an `app=bar` resource is skipped entirely.
	//
	// Propagated to dependency recursion so a `-l app=foo` run on a
	// target with deps applies only the matching subset of every
	// dep too -- otherwise the user would get unfiltered apply on
	// deps and a filtered apply on the target, which is incoherent
	// when both sides should be a coherent slice of the system.
	Selector string

	// Stdout is where user-facing progress and forwarded kubectl
	// output is written. nil -> os.Stdout. Tests pass a buffer to
	// capture; the CLI leaves it nil. Distinct from the zap logger
	// (which is the diagnostic channel on stderr) -- this writer
	// is the command's UI, like kubectl's own per-resource lines.
	Stdout io.Writer
}

// progressOut returns the writer the four "yconverge ..." headers
// and the forwarded kubectl stdout share. Always non-nil.
func (o Options) progressOut() io.Writer {
	if o.Stdout != nil {
		return o.Stdout
	}
	return os.Stdout
}

// userPath turns an absolute filesystem path into something
// readable to the user: a CWD-relative form that matches the
// shape of the -k argument they typed. Tab-completion and a
// follow-up `cd <shown>` Just Work.
//
// Falls back to the absolute path when filepath.Rel can't
// compute one (rare; happens across drive letters on Windows
// or when the cwd is otherwise unrelated to the target).
//
// The diagnostic zap log lines keep RelPath(cueRoot, step) --
// structured fields meant for log aggregation are more useful
// in module-relative form, which doesn't depend on cwd-at-log-time.
func userPath(absPath string) string {
	cwd, err := os.Getwd()
	if err != nil {
		return absPath
	}
	rel, err := filepath.Rel(cwd, absPath)
	if err != nil {
		return absPath
	}
	return rel
}

// Result holds the outcome of a yconverge run.
type Result struct {
	// Steps lists the directories that were converged, in order.
	Steps []string
}

// Run performs a full yconverge: resolve dependencies, apply each step
// (with kustomize server-side apply), and run checks.
func Run(ctx context.Context, opts Options, logger *zap.Logger) (*Result, error) {
	if opts.Context == "" {
		return nil, fmt.Errorf("--context is required")
	}
	if opts.KustomizeDir == "" {
		return nil, fmt.Errorf("-k is required")
	}

	absDir, err := filepath.Abs(opts.KustomizeDir)
	if err != nil {
		return nil, fmt.Errorf("resolve path: %w", err)
	}

	// Find the CUE module root for resolving import paths
	cueRoot := FindCueModuleRoot(absDir)

	// Resolve dependency order from all CUE files in the kustomize
	// tree. An overlay (e.g. backend/qa) inherits dependencies from
	// its base (e.g. backend/base/yconverge.cue imports db). The
	// traversal must succeed: if it fails (corrupt kustomization,
	// permission denied, symlink cycle) the apply might still
	// succeed but no checks would be discovered, leaving the apply
	// silently unverified. Treat traversal errors as fatal.
	var steps []string
	if cueRoot != "" {
		tResult, walkErr := traverse.Walk(absDir, func(format string, a ...any) {
			logger.Warn(fmt.Sprintf(format, a...))
		})
		if walkErr != nil {
			return nil, fmt.Errorf("traverse %s: %w", absDir, walkErr)
		}
		cueDirs := FindCueFiles(tResult.Dirs)
		visited := make(map[string]bool)
		for _, cueDir := range cueDirs {
			depSteps, depErr := ResolveDeps(cueRoot, cueDir)
			if depErr != nil {
				return nil, fmt.Errorf("resolve deps from %s: %w", cueDir, depErr)
			}
			for _, s := range depSteps {
				if !visited[s] {
					visited[s] = true
					steps = append(steps, s)
				}
			}
		}
		if !contains(steps, absDir) {
			steps = append(steps, absDir)
		}
	} else {
		steps = []string{absDir}
	}

	if opts.PrintDeps {
		return &Result{Steps: steps}, nil
	}

	// Multi-step: if more than one step, each step before the last
	// is a dependency that gets its own full convergence cycle.
	hasDeps := len(steps) > 1
	if hasDeps {
		logger.Debug("converge plan",
			zap.String("context", opts.Context),
			zap.Int("steps", len(steps)),
		)
		for _, step := range steps[:len(steps)-1] {
			logger.Debug("converge dependency", zap.String("dir", RelPath(cueRoot, step)))
			fmt.Fprintf(opts.progressOut(), "yconverge dependency %s\n", userPath(step))
			depOpts := Options{
				Context:      opts.Context,
				KustomizeDir: step,
				DryRun:       opts.DryRun,
				SkipChecks:   opts.SkipChecks,
				// Q14: --checks-only must propagate so callers can
				// verify a whole chain without applying anywhere.
				// Earlier this field was dropped, so deps re-applied
				// even when the user only wanted a health check.
				ChecksOnly: opts.ChecksOnly,
				// Selector propagates so a `-l app=foo` run on a
				// target with deps filters every dep's apply by the
				// same selector. Without propagation the deps would
				// apply unfiltered while the target applied filtered,
				// which is incoherent.
				Selector: opts.Selector,
				Stdout:   opts.Stdout,
			}
			if _, err := convergeSingle(ctx, depOpts, logger); err != nil {
				return nil, fmt.Errorf("dependency %s: %w", userPath(step), err)
			}
		}
	}

	// Final step: the target itself. Emit a "target" header only
	// when at least one dep ran -- a no-dep run doesn't need a
	// header for what the user explicitly passed via -k.
	logger.Debug("converge target", zap.String("dir", RelPath(cueRoot, absDir)))
	if hasDeps {
		fmt.Fprintf(opts.progressOut(), "yconverge target %s\n", userPath(absDir))
	}
	if _, err := convergeSingle(ctx, opts, logger); err != nil {
		return nil, err
	}

	return &Result{Steps: steps}, nil
}

// convergeSingle handles one apply+check cycle for a single kustomize base.
func convergeSingle(ctx context.Context, opts Options, logger *zap.Logger) (*Result, error) {
	absDir, err := filepath.Abs(opts.KustomizeDir)
	if err != nil {
		return nil, err
	}

	// Walk the kustomize tree to find yconverge.cue files and namespace
	tResult, err := traverse.Walk(absDir, func(format string, a ...any) {
		logger.Warn(fmt.Sprintf(format, a...))
	})
	if err != nil {
		return nil, fmt.Errorf("traverse %s: %w", opts.KustomizeDir, err)
	}

	namespace := tResult.Namespace

	// Find yconverge.cue files in the traversed directories
	cueDirs := FindCueFiles(tResult.Dirs)
	for _, d := range cueDirs {
		logger.Debug("found yconverge.cue", zap.String("dir", d))
	}

	// Apply (unless checks-only)
	if !opts.ChecksOnly {
		// Bail when a non-empty user selector matches nothing in
		// the kustomize tree. Without this the apply would silently
		// succeed via the "no objects passed" tolerance baked into
		// every group's invocation -- a user typo in -l or a stale
		// label would skip the entire apply with no visible signal.
		// The cost is one client-side dry-run per converge step
		// (~100 ms); the benefit is loud failure on intent
		// mismatches. CHECK_ONLY skips this for the same reason it
		// skips the apply: nothing to bail on if no apply happens.
		if opts.Selector != "" {
			n, err := preflightSelectorMatches(ctx, opts)
			if err != nil {
				return nil, fmt.Errorf("preflight selector: %w", err)
			}
			if n == 0 {
				return nil, fmt.Errorf(
					"selector %q matched no resources in %s; refusing to apply (a silent no-op is a likely user typo)",
					opts.Selector, userPath(absDir))
			}
		}
		if err := runApply(ctx, opts, logger); err != nil {
			return nil, fmt.Errorf("apply %s: %w", opts.KustomizeDir, err)
		}
	}

	// Run checks (unless skip-checks)
	if !opts.SkipChecks {
		runner := &CheckRunner{
			Context:   opts.Context,
			Namespace: namespace,
			Logger:    logger,
			Stdout:    opts.progressOut(),
		}
		for _, cueDir := range cueDirs {
			checks, err := ParseChecks(cueDir)
			if err != nil {
				return nil, fmt.Errorf("parse checks %s: %w", cueDir, err)
			}
			if len(checks) == 0 {
				continue
			}
			if err := runner.RunAll(ctx, checks); err != nil {
				return nil, fmt.Errorf("checks %s: %w", cueDir, err)
			}
		}
	}

	return &Result{Steps: []string{absDir}}, nil
}

// runApply wraps the shellout to `kubectl apply` in the package's
// kubectl.go helper, with one extra responsibility: emit a debug
// log of what's being applied so `-v` runs trace what each
// dependency-walk step is doing without the user having to
// correlate kubectl invocations to the dep graph manually.
func runApply(ctx context.Context, opts Options, logger *zap.Logger) error {
	logger.Debug("apply",
		zap.String("context", opts.Context),
		zap.String("kustomizeDir", opts.KustomizeDir),
		zap.String("dryRun", opts.DryRun),
	)
	if err := kubectlApply(ctx, opts); err != nil {
		return fmt.Errorf("apply %s: %w", opts.KustomizeDir, err)
	}
	return nil
}
