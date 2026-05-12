package images

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
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
	logger.Debug("loading image archive",
		zap.String("backend", string(lr.Backend)),
		zap.String("cluster", lr.ClusterName),
	)
	var stdout, stderr bytes.Buffer
	if err := cluster.RunCtr(ctx, lr, args, archive, &stdout, &stderr); err != nil {
		return fmt.Errorf("ctr image import: %s%s: %w",
			stdout.String(), stderr.String(), err)
	}
	if stdout.Len() > 0 {
		logger.Debug("ctr image import",
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
	created := map[string]string{}
	for _, p := range after {
		if before[p.ref] {
			continue // existed before this import
		}
		alias := aliasFor(p.ref, p.digest)
		if alias == "" {
			continue
		}
		if before[alias] {
			continue // alias already exists somehow; skip
		}
		var tagOut, tagErr bytes.Buffer
		tagArgs := []string{"-n", "k8s.io", "image", "tag", p.ref, alias}
		if err := cluster.RunCtr(ctx, lr, tagArgs, nil, &tagOut, &tagErr); err != nil {
			// Don't fail the whole Load -- the import already
			// landed. Log so the operator sees what didn't
			// alias and can do it manually if needed.
			logger.Warn("ctr image tag (alias) failed",
				zap.String("ref", p.ref),
				zap.String("alias", alias),
				zap.String("stderr", tagErr.String()),
				zap.Error(err))
			continue
		}
		created[p.ref] = alias
		logger.Debug("alias created",
			zap.String("ref", p.ref),
			zap.String("alias", alias))
	}

	// Happy-path summary: one INFO per new ref that names a real
	// repo. ctr writes a bare config-digest row ("sha256:<hex>")
	// alongside the canonical ref when importing; we filter it
	// here so the operator only sees lines for things they pinned.
	any := false
	for _, p := range after {
		if before[p.ref] {
			continue
		}
		if strings.HasPrefix(p.ref, "sha256:") {
			continue
		}
		any = true
		var line string
		if strings.Contains(p.ref, "@") {
			// Digest-form ref already carries the digest; don't
			// repeat it in the log line.
			line = "imported " + p.ref
		} else {
			short := p.digest
			if strings.HasPrefix(short, "sha256:") && len(short) > 19 {
				short = short[:19]
			}
			line = fmt.Sprintf("imported %s @%s", p.ref, short)
		}
		if _, ok := created[p.ref]; ok {
			if strings.Contains(p.ref, "@") {
				line += " (+1 :latest alias)"
			} else {
				line += " (+1 digest alias)"
			}
		}
		logger.Info(line)
	}
	if !any {
		logger.Info("no new image refs (already loaded)")
	}
	return nil
}

// aliasFor decides what alias (if any) to create for a new
// post-import ref. Three cases produce a useful alias; one (the
// bare config-digest row containerd writes alongside the
// canonical ref) produces "" so the caller skips.
//
//   - Tag-only ref ("<repo>:<tag>") -> "<repo>@<digest>".
//     Required by deployments pinning images in name:tag@sha256:
//     digest form; without it kubelet's digest lookup misses the
//     tag-only row in containerd's image store and falls back to
//     a registry pull.
//
//   - Digest-only ref ("<repo>@<digest>") -> "<repo>:latest@
//     <digest>". Required by kubelet's checkpoint-image check on
//     containerd v2: it resolves images by config-digest and
//     normalizes the bare config-digest row to "docker.io/library/
//     sha256@..." (not found). A tag-form alias gives the lookup
//     a parseable repo to land on.
//
//   - Tag+digest ref ("<repo>:<tag>@<digest>") -> "<repo>@
//     <digest>". The import already stored under the full ref
//     name (kubelet resolves either form), but legacy consumers
//     expecting the digest-only form (checkit's appliance-init.sh
//     post-load retag) need that row too. Without it, a partial
//     `ctr image tag --force <repo>@<digest> <new>` from the
//     consumer side fails "image not found".
//
//   - Bare "sha256:<hex>" (the config-digest row ctr emits as a
//     side effect) -> "". Treating it as a tagged ref would
//     mangle it through stripTag into the literal "sha256" and
//     synthesize "sha256@sha256:..." -- a garbage entry that
//     poisons the image store.
func aliasFor(ref, digest string) string {
	if strings.HasPrefix(ref, "sha256:") {
		return ""
	}
	at := strings.Index(ref, "@")
	if at < 0 {
		// Tag-only input. Synthesize <repo>@<digest> so
		// deployments pinning by digest resolve from the
		// tag-only row.
		return stripTag(ref) + "@" + digest
	}
	repoPart := ref[:at]
	if hasTag(repoPart) {
		// Tag+digest input ("<repo>:<tag>@<digest>"). The
		// import already stored under the full ref name --
		// kubelet resolves either form. Also create a
		// digest-only alias so legacy consumers expecting
		// "<repo>@<digest>" (checkit's appliance-init.sh
		// post-load retag for minio-deduplication, kubelet's
		// checkpoint-image lookup on containerd v2) can
		// still find it.
		return stripTag(repoPart) + ref[at:]
	}
	// Digest-only input ("<repo>@<digest>"). Synthesize
	// "<repo>:latest@<digest>" alias so crictl + kubelet's
	// checkpoint-image lookup resolve it.
	return repoPart + ":latest" + ref[at:]
}

// hasTag reports whether ref (already trimmed of any "@digest"
// suffix) carries a "<...>:<tag>" tail. The tag colon must be
// after the last "/" so a "host:port/path" prefix doesn't
// false-positive.
func hasTag(ref string) bool {
	slash := strings.LastIndex(ref, "/")
	tail := ref
	if slash >= 0 {
		tail = ref[slash+1:]
	}
	return strings.Contains(tail, ":")
}

// TarOCIDir streams a USTAR archive of an OCI v1 layout rooted
// at dir into w. The entries are dir-relative (oci-layout,
// index.json, blobs/sha256/*) -- the same shape `tar -cf - -C
// <dir> .` produces, which is what `ctr image import` accepts
// as the "OCI image layout (as tar)" import format.
//
// Used by the load path when the caller supplies a directory:
// stream a tar of the layout straight into the cluster node's
// containerd without making an intermediate file on disk.
// Streaming (not building in memory) keeps memory bounded.
func TarOCIDir(dir string, w io.Writer) error {
	tw := tar.NewWriter(w)
	err := filepath.Walk(dir, func(path string, info fs.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		hdr.Name = filepath.ToSlash(rel)
		if info.IsDir() && !strings.HasSuffix(hdr.Name, "/") {
			hdr.Name += "/"
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(tw, f)
		_ = f.Close()
		return copyErr
	})
	if err != nil {
		_ = tw.Close()
		return err
	}
	return tw.Close()
}

// TarOCIDirReader is the streaming variant of TarOCIDir: returns
// an io.ReadCloser the caller pipes into Load. Internally runs
// TarOCIDir in a goroutine through an io.Pipe; the caller MUST
// Close() to release the goroutine even on aborted reads.
func TarOCIDirReader(dir string) io.ReadCloser {
	pr, pw := io.Pipe()
	go func() {
		err := TarOCIDir(dir, pw)
		_ = pw.CloseWithError(err)
	}()
	return pr
}

// PresentInCluster reports whether digestRef (e.g.
// "docker.io/library/nginx@sha256:abc...") is already indexed in
// the cluster's k8s.io containerd namespace. Returns false on any
// lookup failure -- the caller treats that as "import anyway",
// which is safe because `ctr image import` is itself idempotent
// at the content-store level.
//
// Match policy: succeeds when SOME row in `ctr image list` has
// the same digest as digestRef, regardless of the row's ref
// name. That covers the cases that matter:
//
//   - a prior load brought in the same digest under a different
//     tag (mirrors, retag);
//   - the digest alias the regular Load() step writes back into
//     containerd (host/name@sha256:...) lines up exactly with
//     digestRef.
//
// Skipping a re-import on a digest hit is what saves the
// SSH/docker-exec byte transfer the layered-registry-store 2.0
// would eliminate at the protocol level.
func PresentInCluster(ctx context.Context, lr *cluster.LookupResult, digestRef string) bool {
	wantDigest := digestRef
	if at := strings.LastIndex(digestRef, "@"); at >= 0 {
		wantDigest = digestRef[at+1:]
	}
	pairs := listRefsWithDigests(ctx, lr)
	if pairs == nil {
		return false
	}
	for _, p := range pairs {
		if p.digest == wantDigest {
			return true
		}
	}
	return false
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
