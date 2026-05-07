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
//
// Schema versions: v1 stored every layout file (manifests +
// blobs) under Prefix and listed them all in Files. v2 splits
// blobs out to a content-addressed bucket-level prefix so two
// images sharing a layer share the storage and the upload
// bandwidth (the dedup phase). Entries written by v2 push have
// BlobDigests non-empty and Files restricted to non-blob layout
// files; v1 entries are still readable (BlobDigests empty;
// Files contains the full set under Prefix).
type IndexEntry struct {
	// Ref is the original registry reference the operator pushed
	// (e.g. "nginx:1.27" or "registry.k8s.io/pause:3.10"). Multiple
	// tags resolving to the same digest produce multiple entries
	// pointing at the same Prefix.
	Ref string `json:"ref"`
	// Digest is the resolved manifest digest, sha256:... form.
	Digest string `json:"digest"`
	// Prefix is the object-key prefix under which the per-image
	// OCI layout *manifests* live. Always has a trailing "/".
	// v1 entries had blobs under this prefix too; v2 entries do
	// not (blobs live at the bucket-level "blobs/" prefix).
	Prefix string `json:"prefix"`
	// Files lists relative paths for non-blob layout files
	// ("oci-layout", "index.json"). v1 entries also had blob
	// paths in this list; readers must tolerate both shapes.
	Files []string `json:"files"`
	// BlobDigests is the v2 dedup field: the OCI-relative blob
	// paths (e.g. "blobs/sha256/abc..."). At provision time the
	// pre-load step GETs each blob from <bucket>/<rel> -- the
	// same key shape that PushOCILayout writes to. Empty on v1
	// entries (those have blob paths in Files instead).
	BlobDigests []string `json:"blobDigests,omitempty"`
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
//
// Version history:
//   1: per-image OCI layout in S3; every blob duplicated per
//      pushed image (no dedup).
//   2: blobs hoisted to a content-addressed bucket-level prefix
//      ("blobs/sha256/<hex>"); two images sharing a layer share
//      the storage. IndexEntry.BlobDigests names the blobs;
//      IndexEntry.Files keeps only manifests (oci-layout +
//      index.json).
const IndexVersion = 2

// SharedBlobsPrefix is where v2 push uploads (and v2 pre-load
// reads) layer blobs. Bucket-level so any image's layer with a
// given sha256 lives at the same key, regardless of which image
// referenced it first.
const SharedBlobsPrefix = "blobs/"

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
// `<localCacheRoot>/images/<digest>/` to S3, deduping blobs at
// the content-addressed bucket-level prefix
// (`<bucket>/blobs/sha256/<hex>`). Returns the v2 IndexEntry
// callers merge into the on-S3 index.
//
// Two upload paths:
//
//  1. Blobs: HEAD the bucket-level blob key first. Existing blobs
//     are skipped (operator's bandwidth saved); missing ones get
//     uploaded to the shared key. HEADs run in parallel against a
//     small worker pool.
//  2. Manifests (oci-layout, index.json): per-image, written under
//     `oci/<safe-ref>/<digest>/` with index.json last so its
//     presence is the "fully written" sentinel.
//
// Idempotency: a HEAD on the per-image index.json short-circuits
// the whole push when the layout was already pushed by a prior run.
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

	manifests, blobs := partitionLayoutFiles(files)

	// Step 1: dedup blobs. HEAD each shared-blob key in parallel;
	// upload only the misses. This is the bandwidth-saving win for
	// re-pushes whose blobs already live at the shared prefix.
	//
	// We always run dedup -- never short-circuit -- because that
	// also handles v1->v2 migration: if a v1 layout was pushed
	// previously (blobs under the per-image prefix), the shared
	// keys are all missing, and we upload all blobs to the shared
	// location. The v1 per-image blobs become orphans (acceptable
	// trade for incremental migration; a later `images gc` could
	// sweep them).
	missingBlobs, err := blobsMissingOnS3(ctx, mc, resolved.Bucket, blobs, logger)
	if err != nil {
		return IndexEntry{}, fmt.Errorf("dedup probe: %w", err)
	}
	if len(blobs) > 0 {
		logger.Info("blob dedup result",
			zap.String("ref", ref),
			zap.Int("blobs_total", len(blobs)),
			zap.Int("blobs_to_upload", len(missingBlobs)),
			zap.Int("blobs_skipped", len(blobs)-len(missingBlobs)),
		)
	}
	for _, rel := range missingBlobs {
		key := SharedBlobsPrefix + strings.TrimPrefix(rel, "blobs/")
		logger.Info("uploading shared blob",
			zap.String("ref", ref),
			zap.String("key", key),
		)
		_, err := mc.FPutObject(ctx, resolved.Bucket, key, filepath.Join(localDir, rel), minio.PutObjectOptions{
			ContentType: "application/octet-stream",
		})
		if err != nil {
			return IndexEntry{}, fmt.Errorf("upload %s: %w", key, err)
		}
	}

	// Step 2: per-image manifests. Idempotent on the per-image
	// index.json: a present index.json means the manifests were
	// uploaded by a prior push (blobs may have changed location
	// between v1 and v2 but the manifest contents are identical).
	manifestKey := prefix + "index.json"
	if _, err := mc.StatObject(ctx, resolved.Bucket, manifestKey, minio.StatObjectOptions{}); err == nil {
		logger.Info("OCI manifests already at per-image prefix; skipping",
			zap.String("ref", ref),
			zap.String("prefix", prefix),
		)
	} else {
		// Order so index.json is LAST so its presence is the
		// idempotency sentinel for the next run.
		orderedManifests := orderForUpload(manifests)
		for _, rel := range orderedManifests {
			key := prefix + rel
			logger.Info("uploading OCI manifest",
				zap.String("ref", ref),
				zap.String("key", key),
			)
			_, err := mc.FPutObject(ctx, resolved.Bucket, key, filepath.Join(localDir, rel), minio.PutObjectOptions{
				ContentType: "application/octet-stream",
			})
			if err != nil {
				return IndexEntry{}, fmt.Errorf("upload %s: %w", key, err)
			}
		}
	}
	return IndexEntry{
		Ref: ref, Digest: digest, Prefix: prefix,
		Files: manifests, BlobDigests: blobs,
	}, nil
}

// partitionLayoutFiles splits the walkOCILayout output into
// non-blob manifests vs blobs. The OCI image-layout spec has
// `oci-layout` and `index.json` at the root; everything under
// `blobs/sha256/` is a content-addressed blob. Anything else
// (out of spec) currently treated as a manifest -- callers can
// look at the result to decide whether to log a warning.
func partitionLayoutFiles(files []string) (manifests, blobs []string) {
	for _, f := range files {
		if strings.HasPrefix(f, "blobs/") {
			blobs = append(blobs, f)
		} else {
			manifests = append(manifests, f)
		}
	}
	return manifests, blobs
}

// blobDedupConcurrency caps the number of in-flight HEADs during
// the dedup probe. 8 is enough to saturate a typical home/office
// link without thrashing the bucket; small enough that a slow
// bucket doesn't queue up thousands of pending requests.
const blobDedupConcurrency = 8

// blobsMissingOnS3 returns the subset of relPaths that are NOT
// already present at <bucket>/<SharedBlobsPrefix><tail>. relPaths
// are expected in OCI form ("blobs/sha256/<hex>"); the bucket key
// rewrites the leading "blobs/" to SharedBlobsPrefix.
//
// Concurrency: parallel HEADs via a fixed worker pool. The
// returned slice preserves the input order so logs / re-runs are
// reproducible.
func blobsMissingOnS3(ctx context.Context, mc *minio.Client, bucket string, relPaths []string, logger *zap.Logger) ([]string, error) {
	if len(relPaths) == 0 {
		return nil, nil
	}
	type result struct {
		idx   int
		rel   string
		exist bool
		err   error
	}
	results := make([]result, len(relPaths))
	for i := range results {
		results[i] = result{idx: i, rel: relPaths[i]}
	}
	jobs := make(chan int)
	done := make(chan struct{}, blobDedupConcurrency)
	for w := 0; w < blobDedupConcurrency; w++ {
		go func() {
			for i := range jobs {
				rel := results[i].rel
				key := SharedBlobsPrefix + strings.TrimPrefix(rel, "blobs/")
				_, err := mc.StatObject(ctx, bucket, key, minio.StatObjectOptions{})
				if err == nil {
					results[i].exist = true
				} else if isNotFound(err) {
					results[i].exist = false
				} else {
					results[i].err = err
				}
			}
			done <- struct{}{}
		}()
	}
	for i := range relPaths {
		jobs <- i
	}
	close(jobs)
	for w := 0; w < blobDedupConcurrency; w++ {
		<-done
	}
	missing := make([]string, 0, len(relPaths))
	for _, r := range results {
		if r.err != nil {
			return nil, fmt.Errorf("HEAD blob %s: %w", r.rel, r.err)
		}
		if !r.exist {
			missing = append(missing, r.rel)
		}
	}
	return missing, nil
}

// isNotFound recognises the minio-go shape of an S3 NoSuchKey /
// 404 response. We can't use errors.As against a value type that
// minio-go returns by value (ErrorResponse), so unwrap the
// concrete type explicitly.
func isNotFound(err error) bool {
	var resp minio.ErrorResponse
	if errors.As(err, &resp) {
		return resp.Code == "NoSuchKey" || resp.StatusCode == 404
	}
	return false
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
	// Always stamp the writing binary's version. We never write a
	// version older than this binary understands, and a future
	// reader can rely on `index.version` to match the highest
	// schema field used in any entry.
	idx.Version = IndexVersion
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
