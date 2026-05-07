package images

import (
	"bytes"
	"context"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"go.uber.org/zap"
)

// PresignTTL is the lifetime baked into pre-signed GET URLs the
// operator hands to the cluster node. Long enough to cover a slow
// pre-load over a saturated link; short enough that a leaked URL
// from a failed run is no worse than a temporary read grant.
const PresignTTL = 24 * time.Hour

// SSHRunner runs a shell snippet on the cluster node and returns
// stdout. The hetzner provisioner wraps sshexec.Exec; tests pass
// in a fake. Stdin lets callers stream a script body to `bash -s`
// without worrying about shell quoting.
type SSHRunner func(ctx context.Context, cmd string, stdin []byte) ([]byte, error)

// PreloadFromS3 reads the on-S3 cache index, generates presigned
// GETs for each layout file, and ships per-image bash scripts to
// the cluster node that download the layout into a tmpdir and
// pipe it through `tar | k3s ctr image import`.
//
// Idempotency is delegated to containerd: re-importing the same
// layout is a no-op on its side. We don't probe containerd's
// image store before each import because the network round-trip
// costs more than the redundant import.
//
// Caller is expected to gate this on cfg.ImageCache.Enabled() --
// PreloadFromS3 with an empty index is a no-op (logged).
func PreloadFromS3(ctx context.Context, c S3Config, run SSHRunner, logger *zap.Logger) error {
	if logger == nil {
		logger = zap.NewNop()
	}
	resolved, err := c.Resolve()
	if err != nil {
		return err
	}
	idx, err := ReadIndex(ctx, resolved)
	if err != nil {
		return fmt.Errorf("read index: %w", err)
	}
	if len(idx.Entries) == 0 {
		logger.Info("image cache index is empty; nothing to pre-load",
			zap.String("bucket", resolved.Bucket))
		return nil
	}
	mc, err := newS3Client(resolved)
	if err != nil {
		return fmt.Errorf("init S3 client: %w", err)
	}
	logger.Info("pre-loading images from S3",
		zap.String("bucket", resolved.Bucket),
		zap.Int("count", len(idx.Entries)))
	for _, entry := range idx.Entries {
		if err := preloadOne(ctx, mc, resolved.Bucket, entry, run, logger); err != nil {
			return fmt.Errorf("preload %s: %w", entry.Ref, err)
		}
	}
	logger.Info("pre-load complete")
	return nil
}

// preloadOne handles one IndexEntry: presigns every layout file
// (manifests under entry.Prefix; blobs at the shared bucket-level
// prefix for v2 entries, or under entry.Prefix for v1 entries),
// generates the per-image script, and ships it.
func preloadOne(ctx context.Context, mc *minio.Client, bucket string, entry IndexEntry, run SSHRunner, logger *zap.Logger) error {
	urls, err := presignEntry(ctx, mc, bucket, entry)
	if err != nil {
		return err
	}
	script := BuildPreloadScript(entry, urls)
	logger.Info("ssh-loading image",
		zap.String("ref", entry.Ref),
		zap.String("digest", entry.Digest),
		zap.Int("files", len(entry.Files)),
		zap.Int("blobs", len(entry.BlobDigests)))
	out, err := run(ctx, "bash -s", []byte(script))
	if err != nil {
		return fmt.Errorf("ssh: %w; stdout=%s", err, string(out))
	}
	if len(out) > 0 {
		logger.Info("ctr image import",
			zap.String("ref", entry.Ref),
			zap.String("output", strings.TrimSpace(string(out))))
	}
	return nil
}

// presignEntry generates a presigned GET URL for every file in
// the entry, keyed by the OCI-relative path the script materialises
// the file at on the node. Two cases:
//
//   - manifests (entry.Files): key = entry.Prefix + rel
//   - blobs (entry.BlobDigests, v2): key = SharedBlobsPrefix + tail
//     where tail is the digest path with the leading "blobs/"
//     stripped (so an OCI-relative "blobs/sha256/abc" maps to a
//     bucket key "blobs/sha256/abc" -- bucket-level shared).
//
// v1 entries have BlobDigests empty and blob paths in Files; the
// first branch handles them transparently.
func presignEntry(ctx context.Context, mc *minio.Client, bucket string, entry IndexEntry) (map[string]string, error) {
	out := make(map[string]string, len(entry.Files)+len(entry.BlobDigests))
	for _, rel := range entry.Files {
		key := entry.Prefix + rel
		u, err := mc.PresignedGetObject(ctx, bucket, key, PresignTTL, url.Values{})
		if err != nil {
			return nil, fmt.Errorf("presign %s: %w", key, err)
		}
		out[rel] = u.String()
	}
	for _, rel := range entry.BlobDigests {
		key := SharedBlobsPrefix + strings.TrimPrefix(rel, "blobs/")
		u, err := mc.PresignedGetObject(ctx, bucket, key, PresignTTL, url.Values{})
		if err != nil {
			return nil, fmt.Errorf("presign %s: %w", key, err)
		}
		out[rel] = u.String()
	}
	return out, nil
}

// BuildPreloadScript emits the bash snippet the cluster node runs
// for one IndexEntry. The script:
//
//   1. Creates a per-image tmpdir under /tmp/y-cluster-preload/.
//   2. curls each layout file (manifests + blobs) into its
//      relative position. The materialised tmpdir is a valid
//      OCI v1 image layout regardless of whether blobs came from
//      the per-image prefix (v1 entry) or the shared bucket-level
//      prefix (v2 entry) -- the OCI-relative paths are identical.
//   3. tars the layout and pipes through `sudo k3s ctr -n k8s.io
//      image import -`.
//   4. Cleans up via `trap`.
//
// `set -euo pipefail` makes the first failed curl abort the whole
// script with a non-zero exit, surfaced through SSH to the
// operator.
//
// Exposed (capitalised) so unit tests can pin the shape without
// running SSH. Order of curl invocations is sorted lexicographic
// across the union of Files + BlobDigests so consecutive runs
// emit byte-identical scripts.
func BuildPreloadScript(entry IndexEntry, urls map[string]string) string {
	var b bytes.Buffer
	b.WriteString("#!/bin/bash\n")
	b.WriteString("set -euo pipefail\n")
	b.WriteString("LAYOUT=$(mktemp -d /tmp/y-cluster-preload.XXXXXX)\n")
	b.WriteString("trap 'rm -rf \"$LAYOUT\"' EXIT\n")

	// Union of manifest + blob relative paths. Map iteration is
	// random in Go; sort the union explicitly so the script is
	// reproducible across runs of the same entry.
	all := make([]string, 0, len(entry.Files)+len(entry.BlobDigests))
	all = append(all, entry.Files...)
	all = append(all, entry.BlobDigests...)
	sort.Strings(all)

	for _, rel := range all {
		u, ok := urls[rel]
		if !ok {
			// presignEntry generated URLs from the same files +
			// blobs lists; a mismatch would be a programming
			// error.
			continue
		}
		// One mkdir per directory prefix is cheaper than -p on
		// each curl, but `mkdir -p $(dirname …)` is portable and
		// the overhead is irrelevant here. Quote the URL so any
		// signed-query-string special chars survive the shell.
		b.WriteString("mkdir -p \"$LAYOUT/")
		b.WriteString(shellEscape(dirOf(rel)))
		b.WriteString("\"\n")
		b.WriteString("curl -fsSL --retry 3 -o \"$LAYOUT/")
		b.WriteString(shellEscape(rel))
		b.WriteString("\" '")
		b.WriteString(strings.ReplaceAll(u, "'", "'\\''"))
		b.WriteString("'\n")
	}
	// tar | ctr import. -n k8s.io is the namespace kubelet reads.
	b.WriteString("tar -cf - -C \"$LAYOUT\" . | sudo k3s ctr -n k8s.io image import -\n")
	return b.String()
}

// dirOf returns the parent directory of a relative path. For
// "blobs/sha256/abc" -> "blobs/sha256". For "index.json" -> ""
// (the layout root); the script handles the empty case with
// `mkdir -p "$LAYOUT/"` which is a no-op against the already-
// created tmpdir.
func dirOf(rel string) string {
	i := strings.LastIndex(rel, "/")
	if i < 0 {
		return ""
	}
	return rel[:i]
}

// shellEscape protects path segments inside double-quoted bash
// strings. Layout files come from the operator's local cache --
// their names are sha256 hex / `index.json` / `oci-layout` /
// `blobs` / `sha256` -- so the only character we'd ever escape
// is `$` (none in practice). We handle it anyway so a future
// layout shape with a non-trivial path doesn't bite.
func shellEscape(s string) string {
	r := strings.NewReplacer(
		`\`, `\\`,
		`"`, `\"`,
		"$", `\$`,
		"`", "\\`",
	)
	return r.Replace(s)
}
