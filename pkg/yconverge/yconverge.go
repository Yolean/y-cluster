package yconverge

import (
	"context"
	"fmt"
	"path/filepath"

	"go.uber.org/zap"

	"github.com/Yolean/y-cluster/pkg/k8sapply"
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
	if len(steps) > 1 {
		logger.Debug("converge plan",
			zap.String("context", opts.Context),
			zap.Int("steps", len(steps)),
		)
		for _, step := range steps[:len(steps)-1] {
			logger.Debug("converge dependency",
				zap.String("dir", RelPath(cueRoot, step)),
			)
			depOpts := Options{
				Context:      opts.Context,
				KustomizeDir: step,
				DryRun:       opts.DryRun,
				SkipChecks:   opts.SkipChecks,
				// Q14: --checks-only must propagate so callers can
				// verify a whole chain without applying anywhere.
				// Earlier this field was dropped, so deps re-applied
				// even when the user only wanted a health check.
				ChecksOnly:   opts.ChecksOnly,
			}
			if _, err := convergeSingle(ctx, depOpts, logger); err != nil {
				return nil, fmt.Errorf("dependency %s: %w", RelPath(cueRoot, step), err)
			}
		}
	}

	// Final step: the target itself
	logger.Debug("converge target",
		zap.String("dir", RelPath(cueRoot, absDir)),
	)
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
		if err := kubectlApply(ctx, opts, logger); err != nil {
			return nil, fmt.Errorf("apply %s: %w", opts.KustomizeDir, err)
		}
	}

	// Run checks (unless skip-checks)
	if !opts.SkipChecks {
		runner := &CheckRunner{
			Context:   opts.Context,
			Namespace: namespace,
			Logger:    logger,
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

// kubectlApply runs server-side apply against the named context's
// cluster, equivalent to:
//
//	kubectl --context=<...> apply --server-side --force-conflicts \
//	  --field-manager=y-cluster -k <KustomizeDir>
//
// Implemented in pkg/k8sapply via client-go directly so callers
// get typed errors (apierrors.IsConflict, IsForbidden, etc.)
// instead of "exit status 1, see stderr".
func kubectlApply(ctx context.Context, opts Options, logger *zap.Logger) error {
	dryRun := k8sapply.DryRunNone
	if opts.DryRun == "server" {
		dryRun = k8sapply.DryRunServer
	}
	logger.Debug("apply",
		zap.String("context", opts.Context),
		zap.String("kustomizeDir", opts.KustomizeDir),
		zap.String("dryRun", string(dryRun)),
	)
	if err := k8sapply.Apply(ctx, opts.Context, opts.KustomizeDir, dryRun, logger); err != nil {
		return fmt.Errorf("apply %s: %w", opts.KustomizeDir, err)
	}
	return nil
}
