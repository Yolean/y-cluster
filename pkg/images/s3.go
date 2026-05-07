package images

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"go.uber.org/zap"

	"github.com/Yolean/y-cluster/pkg/cache"
)

// S3Config carries the credentials + bucket addressing the
// operator-side `images push` and the operator-side
// presigner (used by Provision-time pre-load) need.
//
// Endpoint is the S3-compat host for Hetzner Object Storage:
// `<region>.your-objectstorage.com`. Built from Region rather than
// asked for separately so the operator can't drift the two apart.
type S3Config struct {
	AccessKey string
	SecretKey string
	Region    string
	Bucket    string
	// Endpoint is optional; if empty, derived from Region as
	// `<region>.your-objectstorage.com`. Tests against a local
	// MinIO override this.
	Endpoint string
	// IndexKey is the object holding the cache index. Defaults
	// to "index.json" if empty.
	IndexKey string
}

// HetznerS3Endpoint maps a Hetzner Object Storage region to its
// host. Single source of truth so push, pre-load, and tests
// don't drift.
func HetznerS3Endpoint(region string) string {
	return region + ".your-objectstorage.com"
}

// Resolve fills in Endpoint + IndexKey defaults and returns a
// validated copy. Empty AccessKey / SecretKey / Region / Bucket
// are caller errors -- the credential surface is too dangerous
// to silently default.
func (c S3Config) Resolve() (S3Config, error) {
	if c.AccessKey == "" || c.SecretKey == "" {
		return c, errors.New("S3 access key + secret key are required")
	}
	if c.Region == "" {
		return c, errors.New("S3 region is required")
	}
	if c.Bucket == "" {
		return c, errors.New("S3 bucket is required")
	}
	if c.Endpoint == "" {
		c.Endpoint = HetznerS3Endpoint(c.Region)
	}
	if c.IndexKey == "" {
		c.IndexKey = "index.json"
	}
	return c, nil
}

// S3ConfigFromEnv reads the H_S3_* variables the operator's
// y-cluster-hetzner.env file ships with. Empty variables become
// empty fields; callers that allow flag overrides apply those
// before calling Resolve.
func S3ConfigFromEnv() S3Config {
	return S3Config{
		AccessKey: os.Getenv("H_S3_ACCESS_KEY"),
		SecretKey: os.Getenv("H_S3_SECRET_KEY"),
		Region:    os.Getenv("H_S3_REGION"),
		Bucket:    os.Getenv("H_S3_BUCKET"),
	}
}

// newS3Client opens a minio client targeting Hetzner Object
// Storage. Always Secure=true; Hetzner's S3 API only speaks HTTPS.
func newS3Client(c S3Config) (*minio.Client, error) {
	return minio.New(c.Endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(c.AccessKey, c.SecretKey, ""),
		Secure: true,
		Region: c.Region,
	})
}

// IndexEntry is one row in the cache index. The file list is
// kept on the entry (not synthesized at pre-load time by
// LISTing the bucket) so a future pre-loader works off a single
// JSON GET rather than the slower LIST API.
type IndexEntry struct {
	// Ref is the original registry reference the operator pushed
	// (e.g. "nginx:1.27" or "registry.k8s.io/pause:3.10"). Multiple
	// tags resolving to the same digest produce multiple entries
	// pointing at the same Prefix.
	Ref string `json:"ref"`
	// Digest is the resolved manifest digest, sha256:... form.
	Digest string `json:"digest"`
	// Prefix is the object-key prefix under which the OCI v1 image
	// layout lives, e.g. "oci/nginx-1.27/sha256-abc.../". Always
	// has a trailing "/".
	Prefix string `json:"prefix"`
	// Files lists every relative path inside the OCI layout, sorted.
	// Pre-load uses this to issue one presigned GET per file.
	Files []string `json:"files"`
}

// Index is the on-S3 cache index: an array of entries plus a
// trivial Version stamp so future readers can detect a layout
// they don't understand and bail loud.
type Index struct {
	Version int          `json:"version"`
	Entries []IndexEntry `json:"entries"`
}

// IndexVersion is the on-disk schema version. Bump when the
// shape changes incompatibly; loaders that read a higher version
// than they know fail loud.
const IndexVersion = 1

// Find returns the entry whose Ref matches, or zero-IndexEntry +
// false. Multiple entries with the same Ref keep the first match
// (insertion order); push de-dupes on (Ref, Digest) before write.
func (i Index) Find(ref string) (IndexEntry, bool) {
	for _, e := range i.Entries {
		if e.Ref == ref {
			return e, true
		}
	}
	return IndexEntry{}, false
}

// Upsert replaces (or appends) the entry for entry.Ref. Sorts
// by Ref on the way out so diffs stay legible.
func (i *Index) Upsert(entry IndexEntry) {
	out := make([]IndexEntry, 0, len(i.Entries)+1)
	replaced := false
	for _, e := range i.Entries {
		if e.Ref == entry.Ref {
			if !replaced {
				out = append(out, entry)
				replaced = true
			}
			continue
		}
		out = append(out, e)
	}
	if !replaced {
		out = append(out, entry)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Ref < out[b].Ref })
	i.Entries = out
}

// safeRefSegment turns an image reference into a single S3 key
// segment that's both human-recognisable and free of characters
// S3 SDKs / CLIs trip over. The shape (`<repo>--<tag>`) is
// deterministic and reversible-by-eye; the per-digest sub-prefix
// underneath disambiguates same-tag-different-digest cases.
//
// Rules:
//   - "/" -> "_"  (preserves the registry path's intent)
//   - ":" -> "--" (separates tag/repo cleanly)
//   - "@" -> "@"  (digest-form refs keep the @ for clarity)
func safeRefSegment(ref string) string {
	r := strings.NewReplacer("/", "_", ":", "--").Replace(ref)
	return r
}

// safeDigestSegment turns "sha256:abc" into "sha256-abc" so the
// digest can serve as a key segment on its own (S3 keys handle
// `:` but humans navigating the bucket don't).
func safeDigestSegment(digest string) string {
	return strings.ReplaceAll(digest, ":", "-")
}

// ociLayoutPrefix is the canonical object-key prefix for one
// (ref, digest) pair under <bucketRoot>/oci/. Always ends with
// "/" so callers can concatenate relative file paths directly.
func ociLayoutPrefix(ref, digest string) string {
	return path.Join("oci", safeRefSegment(ref), safeDigestSegment(digest)) + "/"
}

// PushOCILayout uploads the local OCI v1 image layout under
// `<localCacheRoot>/images/<digest>/` to S3 under the canonical
// per-(ref, digest) prefix, then returns the IndexEntry callers
// merge into the on-S3 index.
//
// Idempotency: a HEAD on the manifest object short-circuits the
// upload when the object already exists (the manifest is the
// last file written, so its presence implies a complete prior
// upload).
func PushOCILayout(ctx context.Context, c S3Config, localDir, ref, digest string, logger *zap.Logger) (IndexEntry, error) {
	if logger == nil {
		logger = zap.NewNop()
	}
	resolved, err := c.Resolve()
	if err != nil {
		return IndexEntry{}, err
	}
	mc, err := newS3Client(resolved)
	if err != nil {
		return IndexEntry{}, fmt.Errorf("init S3 client: %w", err)
	}

	prefix := ociLayoutPrefix(ref, digest)
	files, err := walkOCILayout(localDir)
	if err != nil {
		return IndexEntry{}, fmt.Errorf("walk %s: %w", localDir, err)
	}
	if len(files) == 0 {
		return IndexEntry{}, fmt.Errorf("no OCI layout files under %s; run `y-cluster images cache %s` first", localDir, ref)
	}

	// Idempotency probe: if the manifest object (index.json) is
	// already present at the canonical key, assume the layout was
	// pushed by a prior run and short-circuit. We probe index.json
	// because PushOCILayout writes it last.
	manifestKey := prefix + "index.json"
	if _, err := mc.StatObject(ctx, resolved.Bucket, manifestKey, minio.StatObjectOptions{}); err == nil {
		logger.Info("OCI layout already on S3; skipping upload",
			zap.String("ref", ref),
			zap.String("digest", digest),
			zap.String("prefix", prefix),
		)
		return IndexEntry{Ref: ref, Digest: digest, Prefix: prefix, Files: files}, nil
	}

	// Order the upload so the layout's `index.json` is LAST, then
	// the idempotency probe above can rely on it as the
	// "fully-written" sentinel. blobs first, oci-layout next, then
	// index.json.
	ordered := orderForUpload(files)
	for _, rel := range ordered {
		key := prefix + rel
		logger.Info("uploading OCI layout file",
			zap.String("ref", ref),
			zap.String("key", key),
		)
		_, err := mc.FPutObject(ctx, resolved.Bucket, key, filepath.Join(localDir, rel), minio.PutObjectOptions{
			// blobs are content-addressed -- octet-stream is fine
			// for everything in an OCI layout.
			ContentType: "application/octet-stream",
		})
		if err != nil {
			return IndexEntry{}, fmt.Errorf("upload %s: %w", key, err)
		}
	}
	return IndexEntry{Ref: ref, Digest: digest, Prefix: prefix, Files: files}, nil
}

// walkOCILayout returns every regular file under root, as relative
// paths sorted lexicographically. Blob paths use forward slashes
// regardless of host OS so the same key list works on darwin and
// linux operators.
func walkOCILayout(root string) ([]string, error) {
	var files []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		files = append(files, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(files)
	return files, nil
}

// orderForUpload puts oci-layout and index.json LAST (in that
// order) so the index.json idempotency probe has a meaningful
// "fully written" signal. All blobs go first.
func orderForUpload(files []string) []string {
	var blobs, ociLayoutOnly, indexOnly []string
	for _, f := range files {
		switch f {
		case "oci-layout":
			ociLayoutOnly = append(ociLayoutOnly, f)
		case "index.json":
			indexOnly = append(indexOnly, f)
		default:
			blobs = append(blobs, f)
		}
	}
	// Blobs already come in sorted; preserve that for legibility.
	out := make([]string, 0, len(files))
	out = append(out, blobs...)
	out = append(out, ociLayoutOnly...)
	out = append(out, indexOnly...)
	return out
}

// ReadIndex GETs s3://<bucket>/<indexKey> and decodes it. A 404
// (NoSuchKey) returns an empty Index{Version: IndexVersion}, NOT
// an error -- the bucket may be brand-new.
func ReadIndex(ctx context.Context, c S3Config) (Index, error) {
	resolved, err := c.Resolve()
	if err != nil {
		return Index{}, err
	}
	mc, err := newS3Client(resolved)
	if err != nil {
		return Index{}, fmt.Errorf("init S3 client: %w", err)
	}
	obj, err := mc.GetObject(ctx, resolved.Bucket, resolved.IndexKey, minio.GetObjectOptions{})
	if err != nil {
		return Index{}, fmt.Errorf("get index: %w", err)
	}
	defer obj.Close()
	stat, err := obj.Stat()
	if err != nil {
		// minio-go reports NoSuchKey through Stat()'s ErrorResponse.
		var resp minio.ErrorResponse
		if errors.As(err, &resp) && resp.Code == "NoSuchKey" {
			return Index{Version: IndexVersion}, nil
		}
		return Index{}, fmt.Errorf("stat index: %w", err)
	}
	_ = stat
	var idx Index
	if err := json.NewDecoder(obj).Decode(&idx); err != nil {
		return Index{}, fmt.Errorf("decode index: %w", err)
	}
	if idx.Version > IndexVersion {
		return Index{}, fmt.Errorf("index version %d is newer than this binary understands (%d); upgrade y-cluster", idx.Version, IndexVersion)
	}
	if idx.Version == 0 {
		idx.Version = IndexVersion
	}
	return idx, nil
}

// WriteIndex PUTs the index JSON at s3://<bucket>/<indexKey>.
// Single-writer; concurrent push runs would race. CAS-via-etag
// is a follow-up if multi-operator workflows show up.
func WriteIndex(ctx context.Context, c S3Config, idx Index) error {
	resolved, err := c.Resolve()
	if err != nil {
		return err
	}
	mc, err := newS3Client(resolved)
	if err != nil {
		return fmt.Errorf("init S3 client: %w", err)
	}
	if idx.Version == 0 {
		idx.Version = IndexVersion
	}
	body, err := json.MarshalIndent(idx, "", "  ")
	if err != nil {
		return err
	}
	_, err = mc.PutObject(ctx, resolved.Bucket, resolved.IndexKey,
		strings.NewReader(string(body)), int64(len(body)),
		minio.PutObjectOptions{ContentType: "application/json"})
	if err != nil {
		return fmt.Errorf("put index: %w", err)
	}
	return nil
}

// LocalOCILayoutDir returns the canonical local OCI layout
// directory for digest under cacheRoot, mirroring pkg/images.Cache's
// layout. Used by `images push` to find the bytes Cache wrote.
func LocalOCILayoutDir(cacheRoot, digest string) (string, error) {
	imagesDir, err := cache.Images(cacheRoot)
	if err != nil {
		return "", err
	}
	return filepath.Join(imagesDir, digest), nil
}
