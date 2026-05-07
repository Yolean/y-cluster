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

// preloadOne handles one IndexEntry: presigns every blob URL,
// generates a script, ships it.
func preloadOne(ctx context.Context, mc *minio.Client, bucket string, entry IndexEntry, run SSHRunner, logger *zap.Logger) error {
	urls, err := presignEntry(ctx, mc, bucket, entry)
	if err != nil {
		return err
	}
	script := BuildPreloadScript(entry, urls)
	logger.Info("ssh-loading image",
		zap.String("ref", entry.Ref),
		zap.String("digest", entry.Digest),
		zap.Int("files", len(entry.Files)))
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
// the entry. Returns map keyed by relative path so the script
// generator can emit `curl ... -o <rel>` rows.
func presignEntry(ctx context.Context, mc *minio.Client, bucket string, entry IndexEntry) (map[string]string, error) {
	out := make(map[string]string, len(entry.Files))
	for _, rel := range entry.Files {
		key := entry.Prefix + rel
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
//   2. curls each layout file into its relative position.
//   3. tars the layout and pipes through `sudo k3s ctr -n k8s.io
//      image import -`.
//   4. Cleans up via `trap`.
//
// `set -euo pipefail` makes the first failed curl abort the whole
// script with a non-zero exit, surfaced through SSH to the
// operator.
//
// Exposed (capitalised) so unit tests can pin the shape without
// running SSH. Order of curl invocations follows the entry's
// Files list (sorted lexicographically by walkOCILayout), which
// keeps the script reproducible across runs of the same entry.
func BuildPreloadScript(entry IndexEntry, urls map[string]string) string {
	var b bytes.Buffer
	b.WriteString("#!/bin/bash\n")
	b.WriteString("set -euo pipefail\n")
	b.WriteString("LAYOUT=$(mktemp -d /tmp/y-cluster-preload.XXXXXX)\n")
	b.WriteString("trap 'rm -rf \"$LAYOUT\"' EXIT\n")

	// Walk Files in sorted order. Ranging map[string]string would
	// randomise; iterate the entry slice so consecutive runs emit
	// byte-identical scripts.
	files := make([]string, 0, len(entry.Files))
	files = append(files, entry.Files...)
	sort.Strings(files)

	for _, rel := range files {
		u, ok := urls[rel]
		if !ok {
			// presignEntry generated URLs from the same files
			// slice; a mismatch would be a programming error.
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
