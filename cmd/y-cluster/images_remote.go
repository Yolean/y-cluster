package main

import (
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/Yolean/y-cluster/pkg/images"
)

// imagesRemoteCmd is the parent of `y-cluster images remote *`,
// reflecting on the on-S3 image cache (the Hetzner Object Storage
// bucket configured via H_S3_*). Distinct from `images cache`
// (which is a verb operating on the LOCAL OCI cache); use
// `remote` when you want to know what the cluster's pre-load
// step would see.
func imagesRemoteCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "remote",
		Short: "Inspect the on-S3 image cache (the bucket pushed/pre-loaded by y-cluster)",
	}
	cmd.AddCommand(imagesRemoteListCmd())
	cmd.AddCommand(imagesRemoteStatsCmd())
	return cmd
}

func imagesRemoteListCmd() *cobra.Command {
	var bucket, region, endpoint, indexKey, accessKey, secretKey string

	cmd := &cobra.Command{
		Use:   "list",
		Short: "Print one row per pushed image (ref, digest, blob-count)",
		Long: `Read s3://<bucket>/<index-key> and print every entry: original ref,
resolved digest, blob count.

Output is a TSV-shaped table; pipe to ` + "`column -t`" + ` for nicer
human formatting, or to ` + "`awk`" + ` for scripting.

Credentials default to H_S3_ACCESS_KEY / H_S3_SECRET_KEY and
the H_S3_REGION / H_S3_BUCKET env vars (the operator's
y-cluster-hetzner.env shape).`,
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			cfg := buildRemoteCfg(bucket, region, endpoint, indexKey, accessKey, secretKey)
			idx, err := images.ReadIndex(c.Context(), cfg)
			if err != nil {
				return err
			}
			out := c.OutOrStdout()
			tw := tabwriter.NewWriter(out, 2, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "REF\tDIGEST\tBLOBS")
			for _, e := range idx.Entries {
				blobs := len(e.BlobDigests)
				if blobs == 0 {
					// v1 fallback: count blob paths embedded in Files.
					for _, f := range e.Files {
						if strings.HasPrefix(f, "blobs/") {
							blobs++
						}
					}
				}
				fmt.Fprintf(tw, "%s\t%s\t%d\n", e.Ref, e.Digest, blobs)
			}
			return tw.Flush()
		},
	}
	bindRemoteFlags(cmd, &bucket, &region, &endpoint, &indexKey, &accessKey, &secretKey)
	return cmd
}

func imagesRemoteStatsCmd() *cobra.Command {
	var bucket, region, endpoint, indexKey, accessKey, secretKey string

	cmd := &cobra.Command{
		Use:   "stats",
		Short: "Summarise the on-S3 image cache: image count, distinct vs referenced blobs, dedup factor, total bytes",
		Long: `Reads s3://<bucket>/<index-key> AND lists s3://<bucket>/blobs/ to
report dedup efficiency.

The dedup factor is references / distinct: 1.0 means every blob
is referenced once (no overlap across images), 2.0 means the
average blob is shared by two images, etc. Higher is better.

Use after a series of pushes to see how much bandwidth was saved
by sharing layers across images.`,
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			cfg := buildRemoteCfg(bucket, region, endpoint, indexKey, accessKey, secretKey)
			s, err := images.ReadRemoteStats(c.Context(), cfg)
			if err != nil {
				return err
			}
			out := c.OutOrStdout()
			fmt.Fprintf(out, "Index version:        %d\n", s.IndexVersion)
			fmt.Fprintf(out, "Images (entries):     %d\n", s.ImageCount)
			fmt.Fprintf(out, "Distinct blobs:       %d  (%.2f MB on S3)\n",
				s.DistinctBlobs, float64(s.SharedBlobBytes)/(1<<20))
			fmt.Fprintf(out, "Blob references:      %d\n", s.BlobReferences)
			fmt.Fprintf(out, "Dedup factor:         %.2fx\n", s.DedupFactor())
			fmt.Fprintf(out, "Manifest objects:     %d  (%.2f MB on S3)\n",
				s.ManifestObjects, float64(s.ManifestObjectBytes)/(1<<20))
			if s.OrphanBlobs > 0 {
				fmt.Fprintf(out, "Orphan v1 blobs:      %d  (%.2f MB on S3 -- candidates for `images gc`)\n",
					s.OrphanBlobs, float64(s.OrphanBlobBytes)/(1<<20))
			}
			savedRefs := s.BlobReferences - s.DistinctBlobs
			if savedRefs > 0 {
				fmt.Fprintf(out, "Pushes avoided:       %d (blob references that hit the dedup probe instead of upload)\n", savedRefs)
			}
			return nil
		},
	}
	bindRemoteFlags(cmd, &bucket, &region, &endpoint, &indexKey, &accessKey, &secretKey)
	return cmd
}

// buildRemoteCfg layers env defaults + flag overrides into one
// S3Config -- shared across every `images remote *` command so
// the credential surface is identical.
func buildRemoteCfg(bucket, region, endpoint, indexKey, accessKey, secretKey string) images.S3Config {
	c := images.S3ConfigFromEnv()
	if bucket != "" {
		c.Bucket = bucket
	}
	if region != "" {
		c.Region = region
	}
	if endpoint != "" {
		c.Endpoint = endpoint
	}
	if indexKey != "" {
		c.IndexKey = indexKey
	}
	if accessKey != "" {
		c.AccessKey = accessKey
	}
	if secretKey != "" {
		c.SecretKey = secretKey
	}
	return c
}

func bindRemoteFlags(cmd *cobra.Command, bucket, region, endpoint, indexKey, accessKey, secretKey *string) {
	cmd.Flags().StringVar(bucket, "bucket", "", "S3 bucket (default: $H_S3_BUCKET)")
	cmd.Flags().StringVar(region, "region", "", "S3 region (default: $H_S3_REGION)")
	cmd.Flags().StringVar(endpoint, "endpoint", "", "S3 endpoint host (default: <region>.your-objectstorage.com)")
	cmd.Flags().StringVar(indexKey, "index-key", "", "object key holding the index (default: index.json)")
	cmd.Flags().StringVar(accessKey, "access-key", "", "S3 access key (default: $H_S3_ACCESS_KEY)")
	cmd.Flags().StringVar(secretKey, "secret-key", "", "S3 secret key (default: $H_S3_SECRET_KEY)")
}
