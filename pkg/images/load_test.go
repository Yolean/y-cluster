package images

import (
	"testing"
)

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
				"builds-registry.ystack.svc.cluster.local/yolean/keycloak-v3:local-dev    application/vnd.oci.image.index.v1+json    sha256:fafacfe13375f62fc0a8303c6c6b6186e755d44f479a161476f4129009eb730b    263.7 MiB    linux/amd64,linux/arm64    io.cri-containerd.image=managed\n",
			want: []importedRef{{
				ref:    "builds-registry.ystack.svc.cluster.local/yolean/keycloak-v3:local-dev",
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
		{"builds-registry.ystack.svc.cluster.local/yolean/keycloak-v3:local-dev", "builds-registry.ystack.svc.cluster.local/yolean/keycloak-v3"},
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
