package yconverge

import (
	"context"
	"fmt"
	"os/exec"
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

	// Resolve dependency order from all CUE files in the kustomize tree.
	// An overlay (e.g. backend/qa) inherits dependencies from its base
	// (e.g. backend/base/yconverge.cue imports db).
	var steps []string
	if cueRoot != "" {
		// Walk the kustomize tree to find all dirs with yconverge.cue
		tResult, walkErr := traverse.Walk(absDir, nil)
		if walkErr == nil {
			cueDirs := FindCueFiles(tResult.Dirs)
			// Resolve deps from each CUE file, collecting all unique steps
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
		}
		// Always include the target itself as the final step
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
		logger.Info("converge plan",
			zap.String("context", opts.Context),
			zap.Int("steps", len(steps)),
		)
		for _, step := range steps[:len(steps)-1] {
			logger.Info("converge dependency",
				zap.String("dir", RelPath(cueRoot, step)),
			)
			depOpts := Options{
				Context:      opts.Context,
				KustomizeDir: step,
				DryRun:       opts.DryRun,
				SkipChecks:   opts.SkipChecks,
			}
			if _, err := convergeSingle(ctx, depOpts, logger); err != nil {
				return nil, fmt.Errorf("dependency %s: %w", RelPath(cueRoot, step), err)
			}
		}
	}

	// Final step: the target itself
	logger.Info("converge target",
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

// kubectlApply runs kubectl apply --server-side on a kustomize base.
func kubectlApply(ctx context.Context, opts Options, logger *zap.Logger) error {
	args := []string{
		"--context=" + opts.Context,
		"apply",
		"--server-side=true",
		"--force-conflicts",
		"-k", opts.KustomizeDir,
	}
	if opts.DryRun != "" {
		args = append(args, "--dry-run="+opts.DryRun)
	}

	logger.Debug("kubectl apply", zap.Strings("args", args))

	cmd := exec.CommandContext(ctx, "kubectl", args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("kubectl apply: %s: %w", string(output), err)
	}
	if len(output) > 0 {
		logger.Info("applied", zap.String("output", string(output)))
	}
	return nil
}
