package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/Yolean/y-cluster/pkg/images"
)

// imagesPushCmd is `y-cluster images push <ref>`: ensure the ref is
// in the local OCI cache, then upload the layout + index entry to
// Hetzner Object Storage. The pre-load Provision-time consumer
// (phase 6.c) reads the index and OCI layouts from the same bucket.
//
// Credentials default to the H_S3_* env vars the operator's
// y-cluster-hetzner.env carries; flags override on a per-invocation
// basis. Hetzner endpoint defaults to <region>.your-objectstorage.com,
// overridable for local-MinIO testing.
//
// Idempotency: a digest already on S3 short-circuits the upload
// step; the index is still updated so a same-digest re-push from a
// fresh tag (e.g. `library/nginx:1.27` -> `nginx:1.27`) records the
// alias.
func imagesPushCmd() *cobra.Command {
	var (
		cacheDir  string
		bucket    string
		region    string
		endpoint  string
		indexKey  string
		accessKey string
		secretKey string
	)

	cmd := &cobra.Command{
		Use:   "push <ref>",
		Short: "Push an image to the Hetzner Object Storage cache (phase 6.b)",
		Long: `Pull <ref> into the local OCI cache (same path as ` + "`images cache`" + `),
then upload the resulting OCI v1 image layout to Hetzner Object
Storage under oci/<safe-ref>/<digest>/, and merge an entry into
the bucket's index.json. The index is the consumer of choice for
provision-time pre-load (HetznerConfig.imageCache).

Credentials default to the H_S3_ACCESS_KEY / H_S3_SECRET_KEY /
H_S3_REGION / H_S3_BUCKET env vars (the operator's
~/Yolean/.yolean-bots-device/y-cluster-hetzner.env shape). Flags
override on a per-invocation basis.

Idempotent on the resolved manifest digest: a digest already
present in the bucket skips the file uploads. The index is
updated either way so a tag-rename produces a fresh entry.

Examples:
  set -a; . ~/Yolean/.yolean-bots-device/y-cluster-hetzner.env; set +a
  y-cluster images push nginx:1.27
  y-cluster images push registry.k8s.io/pause:3.10
  y-cluster images push docker.io/library/redis@sha256:abc...

  y-cluster images push nginx:1.27 --bucket=alt-cluster-images --region=fsn1`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			logger := loggerFromContext(cmd.Context())
			ref := args[0]

			// Step 1: ensure the local OCI layout exists. Reuses
			// pkg/images.Cache so the layout shape stays the
			// single-sourced one.
			digestRef, err := images.Cache(cmd.Context(), ref, cacheDir, logger)
			if err != nil {
				return fmt.Errorf("local cache: %w", err)
			}
			// digestRef is the digest-pinned form; extract the
			// digest itself for the local layout dir lookup.
			//
			// Format guaranteed by digestReference(): "<repo>@<digest>".
			digest := digestRef
			if at := lastIndex(digestRef, "@"); at >= 0 {
				digest = digestRef[at+1:]
			}

			// Step 2: build S3 config from env + flag overrides.
			s3 := images.S3ConfigFromEnv()
			if bucket != "" {
				s3.Bucket = bucket
			}
			if region != "" {
				s3.Region = region
			}
			if endpoint != "" {
				s3.Endpoint = endpoint
			}
			if indexKey != "" {
				s3.IndexKey = indexKey
			}
			if accessKey != "" {
				s3.AccessKey = accessKey
			}
			if secretKey != "" {
				s3.SecretKey = secretKey
			}

			// Step 3: locate the local layout dir and push.
			localDir, err := images.LocalOCILayoutDir(cacheDir, digest)
			if err != nil {
				return fmt.Errorf("locate local layout: %w", err)
			}
			entry, err := images.PushOCILayout(cmd.Context(), s3, localDir, ref, digest, logger)
			if err != nil {
				return fmt.Errorf("push OCI layout: %w", err)
			}

			// Step 4: update the index. Read-modify-write; concurrent
			// pushes would race -- documented as v1 limitation.
			idx, err := images.ReadIndex(cmd.Context(), s3)
			if err != nil {
				return fmt.Errorf("read index: %w", err)
			}
			idx.Upsert(entry)
			if err := images.WriteIndex(cmd.Context(), s3, idx); err != nil {
				return fmt.Errorf("write index: %w", err)
			}

			fmt.Fprintln(cmd.OutOrStdout(), digestRef)
			return nil
		},
	}
	cmd.Flags().StringVar(&cacheDir, "cache-dir", "", "override local cache root (also: $Y_CLUSTER_CACHE_DIR)")
	cmd.Flags().StringVar(&bucket, "bucket", "", "S3 bucket (default: $H_S3_BUCKET)")
	cmd.Flags().StringVar(&region, "region", "", "S3 region (default: $H_S3_REGION)")
	cmd.Flags().StringVar(&endpoint, "endpoint", "", "S3 endpoint host (default: <region>.your-objectstorage.com)")
	cmd.Flags().StringVar(&indexKey, "index-key", "", "object key holding the index (default: index.json)")
	cmd.Flags().StringVar(&accessKey, "access-key", "", "S3 access key (default: $H_S3_ACCESS_KEY)")
	cmd.Flags().StringVar(&secretKey, "secret-key", "", "S3 secret key (default: $H_S3_SECRET_KEY)")
	return cmd
}

// lastIndex is strings.LastIndex inlined to keep this file's
// import list tight (the only other use is digest extraction).
func lastIndex(s, sub string) int {
	for i := len(s) - len(sub); i >= 0; i-- {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
