package images

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"

	"go.uber.org/zap"

	"github.com/Yolean/y-cluster/pkg/cluster"
)

// Load streams an OCI archive (or any tar containerd's image
// importer accepts) into the cluster's containerd via
// `ctr -n k8s.io image import -`. The archive's manifest carries
// the ref + tag — the same way `ctr image import` would behave
// if the operator ran it on the node directly. No cache is
// touched: callers driving from local build artefacts (e.g. a
// `contain` tarball under /tmp) can purge those independently.
//
// After import, Load creates a digest-keyed alias for every
// imported `:tag` ref via `ctr image tag <ref:tag>
// <ref>@sha256:<digest>`. Without this alias, deployments that
// pin images in `name:tag@sha256:digest` form (the shape
// y-release-image-list-local emits for reproducibility) fall
// through to a registry pull because kubelet looks the digest
// ref up in containerd's image store, which only knew about
// the tag ref. See
// specs/y-cluster/FEATURE_REQUEST_IMAGES_LOAD_DIGEST_ALIAS.md.
//
// Aliasing uses a snapshot diff (`ctr image list` before vs
// after the import) rather than parsing import stdout: ctr's
// import-progress output abbreviates / reformats refs in ways
// that don't round-trip back to the stored ref name. The
// post-import `ctr image list` row IS the authoritative
// (ref, digest) tuple containerd actually stored.
//
// Routing per backend matches the rest of pkg/cluster:
//   - docker: dockerexec.Exec into the k3s container
//   - qemu:   sshexec.ExecStream over SSH
//
// The k8s.io namespace is the one kubelet / containerd reads,
// not the default `default` namespace `ctr` uses without -n.
// Without the explicit namespace the loaded image is invisible
// to kubernetes.
func Load(ctx context.Context, lr *cluster.LookupResult, archive io.Reader, logger *zap.Logger) error {
	if logger == nil {
		logger = zap.NewNop()
	}
	if archive == nil {
		return fmt.Errorf("nil archive reader")
	}

	// Snapshot the existing image set so we can identify the
	// refs that landed in this import. listRefs returns nil
	// on any error -- if the snapshot fails we still run the
	// import; we just skip the alias step.
	before := listRefSet(ctx, lr)

	args := []string{"-n", "k8s.io", "image", "import", "-"}
	logger.Info("loading image archive",
		zap.String("backend", string(lr.Backend)),
		zap.String("cluster", lr.ClusterName),
	)
	var stdout, stderr bytes.Buffer
	if err := cluster.RunCtr(ctx, lr, args, archive, &stdout, &stderr); err != nil {
		return fmt.Errorf("ctr image import: %s%s: %w",
			stdout.String(), stderr.String(), err)
	}
	if stdout.Len() > 0 {
		logger.Info("ctr image import",
			zap.String("output", stdout.String()),
		)
	}

	if before == nil {
		// Pre-import snapshot failed; we can't reliably tell
		// what's new. Surface and bail.
		logger.Warn("pre-import snapshot failed; skipping digest-alias step")
		return nil
	}
	after := listRefsWithDigests(ctx, lr)
	if after == nil {
		logger.Warn("post-import snapshot failed; skipping digest-alias step")
		return nil
	}
	aliased := 0
	for _, p := range after {
		if before[p.ref] {
			continue // existed before this import
		}
		if strings.Contains(p.ref, "@") {
			continue // already digest-form; nothing to alias
		}
		nameOnly := stripTag(p.ref)
		alias := nameOnly + "@" + p.digest
		if before[alias] {
			continue // alias already exists somehow; skip
		}
		var tagOut, tagErr bytes.Buffer
		tagArgs := []string{"-n", "k8s.io", "image", "tag", p.ref, alias}
		if err := cluster.RunCtr(ctx, lr, tagArgs, nil, &tagOut, &tagErr); err != nil {
			// Don't fail the whole Load -- the import already
			// landed. Log so the operator sees what didn't
			// alias and can do it manually if needed.
			logger.Warn("ctr image tag (digest alias) failed",
				zap.String("ref", p.ref),
				zap.String("alias", alias),
				zap.String("stderr", tagErr.String()),
				zap.Error(err))
			continue
		}
		aliased++
		logger.Info("digest alias created",
			zap.String("ref", p.ref),
			zap.String("alias", alias))
	}
	if aliased == 0 {
		logger.Info("no new digest aliases needed (re-import or already aliased)")
	}
	return nil
}

// importedRef is one (ref, digest) row from `ctr image list`.
// "imported" is a slight misnomer here -- listRefsWithDigests
// returns the post-import snapshot of EVERY image ref, and the
// caller filters by the before-set diff to find the
// just-imported subset.
type importedRef struct {
	ref    string
	digest string
}

// listRefSet returns the set of all ref names containerd
// has indexed in the k8s.io namespace. Used as the "before"
// snapshot in Load. Returns nil on any error so the caller
// can distinguish "list ran but produced zero rows" (impossible
// in practice -- k3s always has system pause images) from
// "list failed and we have no snapshot".
func listRefSet(ctx context.Context, lr *cluster.LookupResult) map[string]bool {
	pairs := listRefsWithDigests(ctx, lr)
	if pairs == nil {
		return nil
	}
	set := make(map[string]bool, len(pairs))
	for _, p := range pairs {
		set[p.ref] = true
	}
	return set
}

// listRefsWithDigests runs `ctr -n k8s.io image list` on the
// cluster node and parses the (ref, digest) tuple per row.
// ctr's tabwriter output is:
//
//	REF  TYPE  DIGEST  SIZE  PLATFORMS  LABELS
//
// We accept any whitespace as a column separator and locate
// the digest by `sha256:`-prefix rather than column index, so
// the parse is resilient to ctr column-reorderings.
//
// Returns nil on any RunCtr failure -- the caller treats that
// as "skip the alias step" rather than failing the whole Load.
func listRefsWithDigests(ctx context.Context, lr *cluster.LookupResult) []importedRef {
	var stdout, stderr bytes.Buffer
	if err := cluster.RunCtr(ctx, lr, []string{"-n", "k8s.io", "image", "list"}, nil, &stdout, &stderr); err != nil {
		return nil
	}
	return parseImageList(stdout.String())
}

// parseImageList is the pure parser exposed for unit tests.
// Skips the header row, blank lines, and any line we can't
// pair to a sha256: digest token.
func parseImageList(output string) []importedRef {
	var pairs []importedRef
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || fields[0] == "REF" {
			continue
		}
		ref := fields[0]
		var digest string
		for _, f := range fields[1:] {
			if strings.HasPrefix(f, "sha256:") && len(f) == 7+64 {
				digest = f
				break
			}
		}
		if digest == "" {
			continue
		}
		pairs = append(pairs, importedRef{ref: ref, digest: digest})
	}
	return pairs
}

// stripTag returns the ref with its `:tag` suffix removed.
// Refs can include a hostport like `host:port/path:tag`, so
// the tag is the colon-separated segment after the LAST slash.
// Refs with no tag pass through unchanged.
func stripTag(ref string) string {
	slash := strings.LastIndex(ref, "/")
	tail := ref
	if slash >= 0 {
		tail = ref[slash:]
	}
	colon := strings.LastIndex(tail, ":")
	if colon < 0 {
		return ref
	}
	if slash >= 0 {
		return ref[:slash+colon]
	}
	return ref[:colon]
}
