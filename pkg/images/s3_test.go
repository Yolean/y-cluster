package images

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// TestS3Config_Resolve_Defaults: the operator-side flow leaves
// Endpoint + IndexKey blank and expects Resolve to fill them in
// from Region. Region->endpoint is the contract `images push` and
// the future Provision-time pre-load both lean on; pin it.
func TestS3Config_Resolve_Defaults(t *testing.T) {
	c := S3Config{
		AccessKey: "AK",
		SecretKey: "SK",
		Region:    "hel1",
		Bucket:    "y-cluster-examples",
	}
	got, err := c.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	if got.Endpoint != "hel1.your-objectstorage.com" {
		t.Errorf("Endpoint: %q (want hel1.your-objectstorage.com)", got.Endpoint)
	}
	if got.IndexKey != "index.json" {
		t.Errorf("IndexKey: %q (want index.json)", got.IndexKey)
	}
}

// TestS3Config_Resolve_PreservesExplicitOverride: the e2e path
// (later) points at a local MinIO via Endpoint override. Resolve
// must not clobber explicit values.
func TestS3Config_Resolve_PreservesExplicitOverride(t *testing.T) {
	c := S3Config{
		AccessKey: "AK", SecretKey: "SK", Region: "hel1", Bucket: "x",
		Endpoint: "127.0.0.1:9000",
		IndexKey: "alt-index.json",
	}
	got, _ := c.Resolve()
	if got.Endpoint != "127.0.0.1:9000" {
		t.Errorf("Endpoint clobbered: %q", got.Endpoint)
	}
	if got.IndexKey != "alt-index.json" {
		t.Errorf("IndexKey clobbered: %q", got.IndexKey)
	}
}

// TestS3Config_Resolve_RejectsMissing: the credential surface is
// dangerous to silently default; missing fields are caller errors.
func TestS3Config_Resolve_RejectsMissing(t *testing.T) {
	cases := []struct {
		name string
		c    S3Config
		want string
	}{
		{"missing-access", S3Config{SecretKey: "s", Region: "r", Bucket: "b"}, "access key"},
		{"missing-secret", S3Config{AccessKey: "a", Region: "r", Bucket: "b"}, "access key"},
		{"missing-region", S3Config{AccessKey: "a", SecretKey: "s", Bucket: "b"}, "region"},
		{"missing-bucket", S3Config{AccessKey: "a", SecretKey: "s", Region: "r"}, "bucket"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.c.Resolve()
			if err == nil || !contains(err.Error(), tc.want) {
				t.Errorf("want error containing %q, got %v", tc.want, err)
			}
		})
	}
}

// TestS3ConfigFromEnv pins the H_S3_* env mapping the operator's
// y-cluster-hetzner.env file ships with.
func TestS3ConfigFromEnv(t *testing.T) {
	t.Setenv("H_S3_ACCESS_KEY", "AK")
	t.Setenv("H_S3_SECRET_KEY", "SK")
	t.Setenv("H_S3_REGION", "hel1")
	t.Setenv("H_S3_BUCKET", "y-cluster-examples")
	c := S3ConfigFromEnv()
	if c.AccessKey != "AK" || c.SecretKey != "SK" || c.Region != "hel1" || c.Bucket != "y-cluster-examples" {
		t.Errorf("S3ConfigFromEnv mismatch: %+v", c)
	}
}

// TestSafeRefSegment_RoundTrip pins the ref-to-key transformation.
// `/` -> `_` and `:` -> `--` are the only changes; nothing else
// gets rewritten so the human reading the bucket can tell what's
// in there.
func TestSafeRefSegment(t *testing.T) {
	cases := map[string]string{
		"nginx:1.27":                       "nginx--1.27",
		"library/nginx:1.27":               "library_nginx--1.27",
		"registry.k8s.io/pause:3.10":       "registry.k8s.io_pause--3.10",
		"hetznercloud/cli:v1.64.1":         "hetznercloud_cli--v1.64.1",
		"localhost:5000/foo:bar":           "localhost--5000_foo--bar",
	}
	for in, want := range cases {
		if got := safeRefSegment(in); got != want {
			t.Errorf("safeRefSegment(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestSafeDigestSegment: `:` -> `-` so the digest is a clean
// directory name in S3 console listings.
func TestSafeDigestSegment(t *testing.T) {
	got := safeDigestSegment("sha256:abc123")
	if got != "sha256-abc123" {
		t.Errorf("safeDigestSegment: %q (want sha256-abc123)", got)
	}
}

// TestOCILayoutPrefix is the contract push and pre-load both rely
// on. Pin it explicitly so renaming either end is loud.
func TestOCILayoutPrefix(t *testing.T) {
	got := ociLayoutPrefix("nginx:1.27", "sha256:abc")
	want := "oci/nginx--1.27/sha256-abc/"
	if got != want {
		t.Errorf("ociLayoutPrefix: %q (want %q)", got, want)
	}
}

// TestWalkOCILayout: a fake layout produces a sorted list of
// forward-slash-relative paths. Mirrors the shape pkg/images.Cache
// writes -- if Cache changes its layout, this test catches the
// drift before push goes out the door.
func TestWalkOCILayout(t *testing.T) {
	dir := t.TempDir()
	for _, f := range []string{
		"oci-layout",
		"index.json",
		"blobs/sha256/abc",
		"blobs/sha256/def",
	} {
		full := filepath.Join(dir, f)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := walkOCILayout(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"blobs/sha256/abc",
		"blobs/sha256/def",
		"index.json",
		"oci-layout",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("walkOCILayout:\n got:  %v\n want: %v", got, want)
	}
}

// TestOrderForUpload: index.json must be LAST so its presence is
// the "fully written" sentinel for the idempotency probe.
func TestOrderForUpload(t *testing.T) {
	in := []string{
		"blobs/sha256/abc",
		"blobs/sha256/def",
		"index.json",
		"oci-layout",
	}
	got := orderForUpload(in)
	if got[len(got)-1] != "index.json" {
		t.Errorf("index.json should be last; got %v", got)
	}
	// oci-layout next-to-last is convention but not load-bearing;
	// just confirm both terminals land in the right slots.
	if got[len(got)-2] != "oci-layout" {
		t.Errorf("oci-layout should be next-to-last; got %v", got)
	}
}

// TestIndex_Upsert_AddsAndReplaces: covers the two cases that
// matter -- first push of a ref appends, repeat push replaces in
// place, and the result is sorted.
func TestIndex_Upsert_AddsAndReplaces(t *testing.T) {
	idx := Index{Version: IndexVersion}
	idx.Upsert(IndexEntry{Ref: "nginx:1.27", Digest: "sha256:a", Prefix: "oci/nginx--1.27/sha256-a/"})
	idx.Upsert(IndexEntry{Ref: "alpine:3.20", Digest: "sha256:b", Prefix: "oci/alpine--3.20/sha256-b/"})
	if len(idx.Entries) != 2 {
		t.Fatalf("want 2 entries, got %d", len(idx.Entries))
	}
	// Sorted by ref ascending.
	if idx.Entries[0].Ref != "alpine:3.20" {
		t.Errorf("not sorted: %+v", idx.Entries)
	}
	// Replacing nginx:1.27 with a fresh digest leaves count unchanged.
	idx.Upsert(IndexEntry{Ref: "nginx:1.27", Digest: "sha256:c", Prefix: "oci/nginx--1.27/sha256-c/"})
	if len(idx.Entries) != 2 {
		t.Errorf("upsert should replace, not append; got %d entries", len(idx.Entries))
	}
	got, ok := idx.Find("nginx:1.27")
	if !ok || got.Digest != "sha256:c" {
		t.Errorf("Find after re-Upsert: %+v ok=%v", got, ok)
	}
}

func contains(s, substr string) bool {
	return len(substr) == 0 || (len(s) >= len(substr) && (s == substr || hasSubstr(s, substr)))
}

func hasSubstr(s, substr string) bool {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
