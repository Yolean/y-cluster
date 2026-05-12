package images

import (
	"bytes"
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/Yolean/y-cluster/pkg/cluster"
)

// ClusterImage is one stored manifest in a cluster's containerd
// k8s.io namespace, after digest-alias collapse. Multiple
// (ref, digest) rows in `ctr image list` that share the same
// manifest digest fold into one ClusterImage with the most
// informative ref form selected.
type ClusterImage struct {
	Ref       string `json:"ref"`
	Digest    string `json:"digest"`
	SizeBytes int64  `json:"size_bytes"`
	SizeHuman string `json:"size_human"`
}

// ListFromCluster queries the cluster's k8s.io containerd
// namespace and returns one row per stored manifest. ctr's
// tabular output is used over `--format json` because it's
// stable across the containerd versions y-cluster targets;
// the size column is parsed back from its IEC-formatted form.
func ListFromCluster(ctx context.Context, lr *cluster.LookupResult) ([]ClusterImage, error) {
	var stdout, stderr bytes.Buffer
	if err := cluster.RunCtr(ctx, lr, []string{"-n", "k8s.io", "image", "list"}, nil, &stdout, &stderr); err != nil {
		return nil, fmt.Errorf("ctr image list: %s: %w", stderr.String(), err)
	}
	return parseClusterImageList(stdout.String()), nil
}

// parseClusterImageList is the pure parser exposed for tests.
// Skips header, blank lines, and the bare `sha256:<hex>`
// config-digest rows ctr writes alongside the canonical rows.
// Collapses by digest: many refs sharing the same manifest
// digest fold into one ClusterImage, with the most informative
// ref form winning (preferred order in betterRef).
func parseClusterImageList(output string) []ClusterImage {
	byDigest := map[string]*ClusterImage{}
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || fields[0] == "REF" {
			continue
		}
		ref := fields[0]
		if strings.HasPrefix(ref, "sha256:") {
			continue
		}
		// Locate the digest by the sha256: prefix; the SIZE
		// column lives two whitespace tokens after it (number,
		// unit). Anchoring on the digest is robust to ctr column
		// reorderings.
		var digest string
		sizeNumIdx := -1
		for i, f := range fields[1:] {
			if strings.HasPrefix(f, "sha256:") && len(f) == 7+64 {
				digest = f
				sizeNumIdx = i + 1 + 1
				break
			}
		}
		if digest == "" {
			continue
		}
		var size int64
		if sizeNumIdx >= 0 && sizeNumIdx+1 < len(fields) {
			size = parseHumanSize(fields[sizeNumIdx], fields[sizeNumIdx+1])
		}
		if existing, ok := byDigest[digest]; ok {
			if refRank(ref) > refRank(existing.Ref) {
				existing.Ref = ref
			}
			if size > existing.SizeBytes {
				existing.SizeBytes = size
				existing.SizeHuman = formatHumanSize(size)
			}
			continue
		}
		byDigest[digest] = &ClusterImage{
			Ref:       ref,
			Digest:    digest,
			SizeBytes: size,
			SizeHuman: formatHumanSize(size),
		}
	}
	out := make([]ClusterImage, 0, len(byDigest))
	for _, v := range byDigest {
		out = append(out, *v)
	}
	return out
}

// refRank prefers refs that name more identifying detail:
// name:tag@digest > name@digest > name:tag > anything else.
// Used to pick the canonical ref when many rows fold into one.
func refRank(ref string) int {
	hasAt := strings.Contains(ref, "@")
	tail := ref
	if slash := strings.LastIndex(ref, "/"); slash >= 0 {
		tail = ref[slash+1:]
	}
	if at := strings.Index(tail, "@"); at >= 0 {
		tail = tail[:at]
	}
	hasTag := strings.Contains(tail, ":")
	rank := 0
	if hasAt {
		rank += 2
	}
	if hasTag {
		rank++
	}
	return rank
}

// parseHumanSize converts ctr's IEC-formatted size column
// ("263.7 MiB", "1.5 GiB") to bytes. Tolerates "-" / "0" for
// unknown size and a small set of metric units in case
// containerd ever switches.
func parseHumanSize(num, unit string) int64 {
	if num == "" || num == "-" {
		return 0
	}
	f, err := strconv.ParseFloat(num, 64)
	if err != nil {
		return 0
	}
	var mul float64
	switch unit {
	case "B", "":
		mul = 1
	case "KiB":
		mul = 1024
	case "MiB":
		mul = 1024 * 1024
	case "GiB":
		mul = 1024 * 1024 * 1024
	case "TiB":
		mul = 1024 * 1024 * 1024 * 1024
	case "kB":
		mul = 1000
	case "MB":
		mul = 1000 * 1000
	case "GB":
		mul = 1000 * 1000 * 1000
	case "TB":
		mul = 1000 * 1000 * 1000 * 1000
	default:
		return 0
	}
	return int64(f * mul)
}

// formatHumanSize renders a byte count in the same IEC shape
// ctr uses ("263.7 MiB"). Bytes pass through as "<n> B".
func formatHumanSize(b int64) string {
	if b < 1024 {
		return fmt.Sprintf("%d B", b)
	}
	f := float64(b)
	for _, u := range []string{"KiB", "MiB", "GiB", "TiB"} {
		f /= 1024
		if f < 1024 {
			return fmt.Sprintf("%.1f %s", f, u)
		}
	}
	return fmt.Sprintf("%.1f PiB", f/1024)
}

// SortClusterImagesBySizeDesc sorts in place by SizeBytes
// descending, ties broken by Ref ascending so the output is
// deterministic across runs of an unchanged cluster.
func SortClusterImagesBySizeDesc(rows []ClusterImage) {
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].SizeBytes != rows[j].SizeBytes {
			return rows[i].SizeBytes > rows[j].SizeBytes
		}
		return rows[i].Ref < rows[j].Ref
	})
}

// SortClusterImagesByName sorts in place by Ref ascending.
func SortClusterImagesByName(rows []ClusterImage) {
	sort.SliceStable(rows, func(i, j int) bool {
		return rows[i].Ref < rows[j].Ref
	})
}
