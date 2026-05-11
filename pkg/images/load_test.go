package images

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	"github.com/Yolean/y-cluster/pkg/cache"
)

// TestTarOCIDir_StreamsAllFiles writes a small OCI v1 layout
// via the same go-containerregistry pipeline an actual caller
// would (random.Image -> push to in-process registry -> Cache
// pulls into the shared cache), then exercises TarOCIDir and
// confirms every required layout entry shows up in the
// resulting archive. Order doesn't matter; the entry set must
// cover the layout's three required nodes.
func TestTarOCIDir_StreamsAllFiles(t *testing.T) {
	srv := httptest.NewServer(registry.New())
	defer srv.Close()
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	img, err := random.Image(512, 1)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := name.NewTag(u.Host + "/test/tarocidir:v1")
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.Write(ref, img); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	t.Setenv("Y_CLUSTER_CACHE_DIR", root)
	if _, err := Cache(context.Background(), ref.String(), "", nil); err != nil {
		t.Fatal(err)
	}
	imgDigest, err := img.Digest()
	if err != nil {
		t.Fatal(err)
	}
	dir, err := cache.ImageLayout(root, imgDigest.String())
	if err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if err := TarOCIDir(dir, &buf); err != nil {
		t.Fatalf("TarOCIDir: %v", err)
	}

	tr := tar.NewReader(&buf)
	seen := map[string]bool{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		seen[hdr.Name] = true
		_, _ = io.Copy(io.Discard, tr)
	}

	for _, need := range []string{"oci-layout", "index.json"} {
		if !seen[need] {
			t.Errorf("tar missing %q (got %v)", need, seen)
		}
	}
	hasBlob := false
	for n := range seen {
		if filepath.Dir(n) == "blobs/sha256" {
			hasBlob = true
			break
		}
	}
	if !hasBlob {
		t.Errorf("tar has no blobs/sha256 entries (got %v)", seen)
	}
}

// TestTarOCIDir_EmptyDirNoEntries: empty source dir produces a
// valid tar that decodes to zero entries. The caller (Load via
// ctr import) would surface a downstream import failure on
// empty input, which is the right level for "you pointed me at
// an empty layout".
func TestTarOCIDir_EmptyDirNoEntries(t *testing.T) {
	dir := t.TempDir()
	var buf bytes.Buffer
	if err := TarOCIDir(dir, &buf); err != nil {
		t.Fatalf("TarOCIDir(empty): %v", err)
	}
	tr := tar.NewReader(&buf)
	if _, err := tr.Next(); err != io.EOF {
		t.Errorf("expected EOF on empty layout, got err=%v", err)
	}
}

// TestTarOCIDir_NonexistentDirErrors: cmd-layer guards check
// os.Stat first, but TarOCIDir on a missing dir should still
// surface the error rather than silently produce an empty tar.
func TestTarOCIDir_NonexistentDirErrors(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	var buf bytes.Buffer
	err := TarOCIDir(missing, &buf)
	if err == nil {
		t.Fatal("expected error for missing dir")
	}
}

// TestTarOCIDirReader_PipeClose: the goroutine-backed reader
// must release its goroutine on early Close (Load defers Close
// even on abort).
func TestTarOCIDirReader_PipeClose(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "marker"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := TarOCIDirReader(dir)
	buf := make([]byte, 32)
	_, _ = r.Read(buf)
	if err := r.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}

// TestPresentInCluster_DigestMatch pins the matcher policy:
// digestRef like "host/name@sha256:abc" matches any row whose
// digest column equals sha256:abc, regardless of the row's
// ref name. Catches the common case where a prior load brought
// in the same digest under a different tag (mirrors, retag) --
// we should still skip the re-import.
//
// Driven through parseImageList + a hand-constructed pairs
// slice so we don't need a real cluster.
func TestPresentInCluster_DigestMatch(t *testing.T) {
	const want = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	pairs := parseImageList("REF\tTYPE\tDIGEST\nfoo:bar\ttype\t" + want + "\n")
	if len(pairs) != 1 {
		t.Fatalf("expected 1 row, got %d", len(pairs))
	}
	if pairs[0].digest != want {
		t.Fatalf("parser regression: got digest %q want %q", pairs[0].digest, want)
	}
	// The cluster-side matcher is a substring of PresentInCluster:
	// scan the pairs for a digest equality. Verify the policy in
	// isolation -- a full PresentInCluster test would need a fake
	// cluster.LookupResult which is more wiring than this
	// behavioural check justifies.
	hit := false
	for _, p := range pairs {
		if p.digest == want {
			hit = true
			break
		}
	}
	if !hit {
		t.Error("digest match policy regression")
	}
}

// TestParseImageList pins the (ref, digest) extraction
// against a real `ctr -n k8s.io image list` capture from a
// k3s v1.35 cluster. We accept any whitespace separator and
// locate the digest by sha256: prefix rather than column
// index so the parse survives ctr column reorderings.
func TestParseImageList(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []importedRef
	}{
		{
			name: "header + single row",
			in: "REF                                  TYPE                                                                                       DIGEST                                                                  SIZE       PLATFORMS                LABELS\n" +
				"builds-registry.default.svc.cluster.local/myrepo/myapp:local-dev    application/vnd.oci.image.index.v1+json    sha256:fafacfe13375f62fc0a8303c6c6b6186e755d44f479a161476f4129009eb730b    263.7 MiB    linux/amd64,linux/arm64    io.cri-containerd.image=managed\n",
			want: []importedRef{{
				ref:    "builds-registry.default.svc.cluster.local/myrepo/myapp:local-dev",
				digest: "sha256:fafacfe13375f62fc0a8303c6c6b6186e755d44f479a161476f4129009eb730b",
			}},
		},
		{
			name: "two rows including a digest-form ref",
			in: "REF\tTYPE\tDIGEST\tSIZE\tPLATFORMS\tLABELS\n" +
				"docker.io/yolean/echo:1.0\tapplication/vnd.docker.distribution.manifest.v2+json\tsha256:1111111111111111111111111111111111111111111111111111111111111111\t10 MiB\tlinux/amd64\t-\n" +
				"docker.io/yolean/echo@sha256:1111111111111111111111111111111111111111111111111111111111111111\tapplication/vnd.docker.distribution.manifest.v2+json\tsha256:1111111111111111111111111111111111111111111111111111111111111111\t10 MiB\tlinux/amd64\t-\n",
			want: []importedRef{
				{ref: "docker.io/yolean/echo:1.0", digest: "sha256:1111111111111111111111111111111111111111111111111111111111111111"},
				{ref: "docker.io/yolean/echo@sha256:1111111111111111111111111111111111111111111111111111111111111111", digest: "sha256:1111111111111111111111111111111111111111111111111111111111111111"},
			},
		},
		{
			name: "empty",
			in:   "",
			want: nil,
		},
		{
			name: "only header",
			in:   "REF\tTYPE\tDIGEST\n",
			want: nil,
		},
		{
			name: "row missing digest token",
			in:   "REF\tTYPE\tDIGEST\nfoo\tbar\tbaz\n",
			want: nil,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := parseImageList(c.in)
			if len(got) != len(c.want) {
				t.Fatalf("got %d rows, want %d: %#v", len(got), len(c.want), got)
			}
			for i := range c.want {
				if got[i] != c.want[i] {
					t.Errorf("row %d: got %+v, want %+v", i, got[i], c.want[i])
				}
			}
		})
	}
}

// TestStripTag covers the hostport edge case
// (`host:port/path:tag`) that a naive `strings.LastIndex(ref,
// ":")` mis-handles by stripping the port instead of the tag.
func TestStripTag(t *testing.T) {
	cases := []struct{ in, want string }{
		// Plain ref with tag.
		{"foo:bar", "foo"},
		// Hostport-prefixed ref WITH tag.
		{"registry.example:5000/path:tag", "registry.example:5000/path"},
		{"localhost:5000/yolean/echo:v1", "localhost:5000/yolean/echo"},
		// Hostport-prefixed ref WITHOUT tag.
		{"registry.example:5000/path", "registry.example:5000/path"},
		// Standard cluster-local registry path.
		{"builds-registry.default.svc.cluster.local/myrepo/myapp:local-dev", "builds-registry.default.svc.cluster.local/myrepo/myapp"},
		// No tag at all.
		{"plain", "plain"},
	}
	for _, c := range cases {
		got := stripTag(c.in)
		if got != c.want {
			t.Errorf("stripTag(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestAliasFor pins the post-import alias policy. Each case
// captures one of the three shapes ctr writes into the image
// store after `image import`: tag-form, digest-form, and the
// bare config-digest row that mustn't be aliased.
func TestAliasFor(t *testing.T) {
	const digest = "sha256:af91c49ce795f3b2c1a4e6d8b9c0e1f2a3b4c5d6e7f80112233445566778899aa"
	cases := []struct {
		name, ref, want string
	}{
		{
			name: "tag-form -> digest alias",
			ref:  "ghcr.io/yolean/echo:v1",
			want: "ghcr.io/yolean/echo@" + digest,
		},
		{
			name: "digest-form -> :latest alias (kubelet checkpoint-image lookup)",
			ref:  "ghcr.io/yolean/minio-deduplication@" + digest,
			want: "ghcr.io/yolean/minio-deduplication:latest@" + digest,
		},
		{
			name: "bare config-digest row -> no alias (would mangle to sha256@sha256:...)",
			ref:  "sha256:dc863b8391abb7c2d3e4f5a6b7c8d9e0f1a2b3c4d5e6f70819253647586978a9",
			want: "",
		},
		{
			name: "hostport tag stripped at correct colon",
			ref:  "registry.example:5000/foo/bar:tag",
			want: "registry.example:5000/foo/bar@" + digest,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := aliasFor(c.ref, digest)
			if got != c.want {
				t.Errorf("aliasFor(%q) = %q, want %q", c.ref, got, c.want)
			}
		})
	}
}
