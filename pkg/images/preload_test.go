package images

import (
	"strings"
	"testing"
)

// TestBuildPreloadScript_ShapeAndSafety pins the four properties
// of the per-image bash script the cluster node runs:
//
//   1. set -euo pipefail (a failed curl aborts the whole import)
//   2. mktemp under /tmp/y-cluster-preload.* (matches the trap)
//   3. one curl per file, in lexicographic order (reproducible)
//   4. ends in `tar | sudo k3s ctr -n k8s.io image import -`
//
// File-order reproducibility matters for diffing across runs; the
// test pins it explicitly.
func TestBuildPreloadScript_ShapeAndSafety(t *testing.T) {
	entry := IndexEntry{
		Ref:    "nginx:1.27",
		Digest: "sha256:abc",
		Prefix: "oci/nginx--1.27/sha256-abc/",
		Files: []string{
			"blobs/sha256/aaa",
			"blobs/sha256/bbb",
			"index.json",
			"oci-layout",
		},
	}
	urls := map[string]string{
		"blobs/sha256/aaa": "https://example.test/aaa?sig=1",
		"blobs/sha256/bbb": "https://example.test/bbb?sig=2",
		"index.json":       "https://example.test/index.json?sig=3",
		"oci-layout":       "https://example.test/oci-layout?sig=4",
	}
	got := BuildPreloadScript(entry, urls)

	for _, want := range []string{
		"set -euo pipefail",
		"LAYOUT=$(mktemp -d /tmp/y-cluster-preload.XXXXXX)",
		"trap 'rm -rf \"$LAYOUT\"' EXIT",
		"tar -cf - -C \"$LAYOUT\" . | sudo k3s ctr -n k8s.io image import -",
		// Per-file curl with the URL inside single-quotes.
		"curl -fsSL --retry 3 -o \"$LAYOUT/blobs/sha256/aaa\" 'https://example.test/aaa?sig=1'",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("script missing %q:\n%s", want, got)
		}
	}

	// File-order: aaa before bbb, both before index.json, both
	// before oci-layout. Sorted lexicographic.
	posAaa := strings.Index(got, "blobs/sha256/aaa\"")
	posBbb := strings.Index(got, "blobs/sha256/bbb\"")
	posIdx := strings.Index(got, "index.json\"")
	posOci := strings.Index(got, "oci-layout\"")
	if !(posAaa < posBbb && posBbb < posIdx && posIdx < posOci) {
		t.Errorf("file order not lexicographic:\naaa@%d  bbb@%d  index@%d  oci@%d",
			posAaa, posBbb, posIdx, posOci)
	}
}

// TestBuildPreloadScript_QuoteSafety: a presigned URL with a
// single-quote (rare but possible in arbitrary query-string
// values) must be escaped so it doesn't terminate the bash
// single-quoted string.
func TestBuildPreloadScript_QuoteSafety(t *testing.T) {
	entry := IndexEntry{
		Ref:    "x",
		Digest: "sha256:y",
		Prefix: "oci/x/sha256-y/",
		Files:  []string{"index.json"},
	}
	tricky := "https://example.test/blob?sig=a'b"
	got := BuildPreloadScript(entry, map[string]string{"index.json": tricky})
	// Bash single-quote-escape pattern: '\''
	if !strings.Contains(got, `'https://example.test/blob?sig=a'\''b'`) {
		t.Errorf("quote escape missing for tricky URL:\n%s", got)
	}
}

// TestDirOf: the parent-dir helper that drives the per-curl
// `mkdir -p` in the script.
func TestDirOf(t *testing.T) {
	cases := map[string]string{
		"blobs/sha256/abc": "blobs/sha256",
		"index.json":       "",
		"oci-layout":       "",
		"a/b/c/d":          "a/b/c",
	}
	for in, want := range cases {
		if got := dirOf(in); got != want {
			t.Errorf("dirOf(%q) = %q, want %q", in, got, want)
		}
	}
}
