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

// TestBuildPreloadScript_V2_SharedBlobs: a v2 entry has Files for
// manifests only, BlobDigests for the blob paths. The script
// must include curl rows for both -- the materialised tmpdir is
// still a complete OCI layout despite the bucket-level blob
// indirection.
func TestBuildPreloadScript_V2_SharedBlobs(t *testing.T) {
	entry := IndexEntry{
		Ref:    "nginx:1.27",
		Digest: "sha256:abc",
		Prefix: "oci/nginx--1.27/sha256-abc/",
		Files: []string{
			"index.json",
			"oci-layout",
		},
		BlobDigests: []string{
			"blobs/sha256/aaa",
			"blobs/sha256/bbb",
		},
	}
	urls := map[string]string{
		"index.json":       "https://example.test/idx",
		"oci-layout":       "https://example.test/oci",
		"blobs/sha256/aaa": "https://example.test/aaa",
		"blobs/sha256/bbb": "https://example.test/bbb",
	}
	got := BuildPreloadScript(entry, urls)

	for _, want := range []string{
		"-o \"$LAYOUT/blobs/sha256/aaa\" 'https://example.test/aaa'",
		"-o \"$LAYOUT/blobs/sha256/bbb\" 'https://example.test/bbb'",
		"-o \"$LAYOUT/index.json\" 'https://example.test/idx'",
		"-o \"$LAYOUT/oci-layout\" 'https://example.test/oci'",
		"tar -cf - -C \"$LAYOUT\" . | sudo k3s ctr -n k8s.io image import -",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("script missing %q:\n%s", want, got)
		}
	}
}

// TestCanonicalAliasesForKubelet pins the kubelet-canonicalisation
// rules. The shipping bug this guards against: pre-loaded image
// is in containerd's k8s.io namespace under "hashicorp/http-echo:1.0.0",
// kubelet schedules a Pod with image "hashicorp/http-echo:1.0.0",
// canonicalises to "docker.io/hashicorp/http-echo:1.0.0" before
// the containerd lookup, finds nothing, and pulls. Adding the
// "docker.io/..." alias closes that gap.
func TestCanonicalAliasesForKubelet(t *testing.T) {
	cases := map[string][]string{
		// Docker Hub two-segment: aliases under both forms.
		"hashicorp/http-echo:1.0.0": {
			"docker.io/hashicorp/http-echo:1.0.0",
			"index.docker.io/hashicorp/http-echo:1.0.0",
		},
		// Docker Hub one-segment (library/...): same shape.
		"hello-world:latest": {
			"docker.io/library/hello-world:latest",
			"index.docker.io/library/hello-world:latest",
		},
		// Already-canonical (non-Docker Hub registry): no
		// alias needed (the ref form matches kubelet's lookup
		// already; tagging X to X would fail).
		"registry.k8s.io/pause:3.10": nil,
		"ghcr.io/yolean/foo:v1":      nil,
		// Already prefixed with docker.io: produce an
		// index.docker.io/... alias (and skip the docker.io/
		// alias since it equals input).
		"docker.io/library/redis:7.2": {
			"index.docker.io/library/redis:7.2",
		},
		// Digest form: separator becomes `@` not `:`.
		"hashicorp/http-echo@sha256:abcdef0000000000000000000000000000000000000000000000000000000000": {
			"docker.io/hashicorp/http-echo@sha256:abcdef0000000000000000000000000000000000000000000000000000000000",
			"index.docker.io/hashicorp/http-echo@sha256:abcdef0000000000000000000000000000000000000000000000000000000000",
		},
	}
	for ref, want := range cases {
		got := canonicalAliasesForKubelet(ref)
		if len(got) != len(want) {
			t.Errorf("%q: got %v aliases (%d), want %v (%d)", ref, got, len(got), want, len(want))
			continue
		}
		for i := range got {
			if got[i] != want[i] {
				t.Errorf("%q[%d]: got %q, want %q", ref, i, got[i], want[i])
			}
		}
	}
}

// TestBuildPreloadScript_TagsCanonicalAliases: the script the
// node runs must include a `ctr image tag` step for each
// canonical alias. Verifies the tag command is in scope.
func TestBuildPreloadScript_TagsCanonicalAliases(t *testing.T) {
	entry := IndexEntry{
		Ref:    "hashicorp/http-echo:1.0.0",
		Digest: "sha256:abc",
		Prefix: "oci/hashicorp_http-echo--1.0.0/sha256-abc/",
		Files:  []string{"index.json"},
	}
	got := BuildPreloadScript(entry, map[string]string{
		"index.json": "https://example.test/index.json",
	})
	for _, want := range []string{
		"ctr -n k8s.io image tag --force 'hashicorp/http-echo:1.0.0' 'docker.io/hashicorp/http-echo:1.0.0'",
		"ctr -n k8s.io image tag --force 'hashicorp/http-echo:1.0.0' 'index.docker.io/hashicorp/http-echo:1.0.0'",
		"|| true", // swallow tag-when-already-equal failures
	} {
		if !strings.Contains(got, want) {
			t.Errorf("preload script missing %q:\n%s", want, got)
		}
	}
}

// TestBuildPreloadScript_V1_BackCompat: a v1 entry (BlobDigests
// empty, blob paths in Files) still produces a working script.
// Pre-load must remain backward-compatible so an operator who
// pushed under v1 and then upgraded the binary doesn't have to
// re-push every cached image.
func TestBuildPreloadScript_V1_BackCompat(t *testing.T) {
	entry := IndexEntry{
		Ref:    "hello-world:latest",
		Digest: "sha256:abc",
		Prefix: "oci/hello-world--latest/sha256-abc/",
		Files: []string{
			"blobs/sha256/aaa",
			"index.json",
			"oci-layout",
		},
		// BlobDigests intentionally empty
	}
	urls := map[string]string{
		"blobs/sha256/aaa": "https://example.test/aaa-v1",
		"index.json":       "https://example.test/idx-v1",
		"oci-layout":       "https://example.test/oci-v1",
	}
	got := BuildPreloadScript(entry, urls)
	if !strings.Contains(got, "-o \"$LAYOUT/blobs/sha256/aaa\" 'https://example.test/aaa-v1'") {
		t.Errorf("v1 entry blob curl missing:\n%s", got)
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
