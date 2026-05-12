package images

import (
	"strings"
	"testing"
)

// TestParseClusterImageList_CollapsesByDigest pins the
// digest-collapse policy on a representative `ctr image list`
// capture: the same image manifest appears under tag, digest,
// and the alias the loader writes back. The parser must fold
// all three into one row with the most-informative ref form.
// The bare `sha256:<hex>` config-digest row must NOT show up.
func TestParseClusterImageList_CollapsesByDigest(t *testing.T) {
	manifest := "sha256:" + strings.Repeat("a", 64)
	config := "sha256:" + strings.Repeat("b", 64)
	in := strings.Join([]string{
		"REF\tTYPE\tDIGEST\tSIZE\tPLATFORMS\tLABELS",
		"ghcr.io/yolean/headless-chrome:abc123\tapplication/vnd.oci.image.index.v1+json\t" + manifest + "\t553.0 MiB\tlinux/amd64\t-",
		"ghcr.io/yolean/headless-chrome@" + manifest + "\tapplication/vnd.oci.image.index.v1+json\t" + manifest + "\t553.0 MiB\tlinux/amd64\t-",
		"ghcr.io/yolean/headless-chrome:abc123@" + manifest + "\tapplication/vnd.oci.image.index.v1+json\t" + manifest + "\t553.0 MiB\tlinux/amd64\t-",
		config + "\tapplication/vnd.docker.distribution.manifest.v2+json\t" + manifest + "\t553.0 MiB\tlinux/amd64\t-",
	}, "\n") + "\n"

	got := parseClusterImageList(in)
	if len(got) != 1 {
		t.Fatalf("expected 1 collapsed row, got %d: %+v", len(got), got)
	}
	want := "ghcr.io/yolean/headless-chrome:abc123@" + manifest
	if got[0].Ref != want {
		t.Errorf("canonical ref: got %q, want %q (most-informative form should win)", got[0].Ref, want)
	}
	if got[0].Digest != manifest {
		t.Errorf("digest: got %q, want %q", got[0].Digest, manifest)
	}
	if got[0].SizeBytes != 553*1024*1024 {
		t.Errorf("size: got %d bytes, want %d (553 MiB)", got[0].SizeBytes, 553*1024*1024)
	}
}

// TestParseClusterImageList_MultipleImages confirms the
// happy path where each image has its own digest.
func TestParseClusterImageList_MultipleImages(t *testing.T) {
	in := "REF\tTYPE\tDIGEST\tSIZE\tPLATFORMS\tLABELS\n" +
		"ghcr.io/foo/a:v1\tx\tsha256:1111111111111111111111111111111111111111111111111111111111111111\t10.0 MiB\tlinux/amd64\t-\n" +
		"ghcr.io/foo/b:v1\tx\tsha256:2222222222222222222222222222222222222222222222222222222222222222\t2.0 GiB\tlinux/amd64\t-\n"
	got := parseClusterImageList(in)
	if len(got) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(got))
	}
}

// TestParseClusterImageList_SkipsConfigDigestRows pins the
// regression guard from ISSUE_IMAGES_LOAD_MANGLES_CONFIG_DIGEST_REFS:
// the bare `sha256:<hex>` row must not surface in the listing.
func TestParseClusterImageList_SkipsConfigDigestRows(t *testing.T) {
	in := "REF\tTYPE\tDIGEST\tSIZE\tPLATFORMS\tLABELS\n" +
		"sha256:dc863b8391abb7c2d3e4f5a6b7c8d9e0f1a2b3c4d5e6f70819253647586978a9\tx\tsha256:1111111111111111111111111111111111111111111111111111111111111111\t10.0 MiB\tlinux/amd64\t-\n"
	got := parseClusterImageList(in)
	if len(got) != 0 {
		t.Fatalf("expected 0 rows (config-digest filtered), got %d: %+v", len(got), got)
	}
}

func TestRefRank(t *testing.T) {
	cases := []struct {
		ref      string
		wantHigh int
	}{
		// tag+digest beats digest beats tag beats neither
		{"ghcr.io/foo/bar:v1@sha256:abc", 3},
		{"ghcr.io/foo/bar@sha256:abc", 2},
		{"ghcr.io/foo/bar:v1", 1},
		{"ghcr.io/foo/bar", 0},
		// hostport ":port" before the last slash should NOT count as a tag
		{"registry.example:5000/foo/bar", 0},
		{"registry.example:5000/foo/bar:v1", 1},
		{"registry.example:5000/foo/bar:v1@sha256:abc", 3},
	}
	for _, c := range cases {
		if got := refRank(c.ref); got != c.wantHigh {
			t.Errorf("refRank(%q) = %d, want %d", c.ref, got, c.wantHigh)
		}
	}
}

func TestParseHumanSize(t *testing.T) {
	mib := func(f float64) int64 { return int64(f * float64(1024*1024)) }
	gib := func(f float64) int64 { return int64(f * float64(1024*1024*1024)) }
	cases := []struct {
		num, unit string
		want      int64
	}{
		{"0", "B", 0},
		{"1024", "B", 1024},
		{"1.0", "KiB", 1024},
		{"263.7", "MiB", mib(263.7)},
		{"1.5", "GiB", gib(1.5)},
		{"-", "", 0},
		{"", "", 0},
		{"nope", "MiB", 0},
		{"1", "??", 0},
	}
	for _, c := range cases {
		if got := parseHumanSize(c.num, c.unit); got != c.want {
			t.Errorf("parseHumanSize(%q, %q) = %d, want %d", c.num, c.unit, got, c.want)
		}
	}
}

func TestFormatHumanSize(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{0, "0 B"},
		{1023, "1023 B"},
		{1024, "1.0 KiB"},
		{1024 * 1024, "1.0 MiB"},
		{int64(553.0 * 1024 * 1024), "553.0 MiB"},
		{1024 * 1024 * 1024, "1.0 GiB"},
	}
	for _, c := range cases {
		if got := formatHumanSize(c.in); got != c.want {
			t.Errorf("formatHumanSize(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestSortClusterImagesBySizeDesc pins the deterministic
// ordering: size descending, ref ascending as the tiebreaker
// so an unchanged cluster always prints the same lines.
func TestSortClusterImagesBySizeDesc(t *testing.T) {
	rows := []ClusterImage{
		{Ref: "small", SizeBytes: 1},
		{Ref: "z-mid", SizeBytes: 100},
		{Ref: "a-mid", SizeBytes: 100},
		{Ref: "big", SizeBytes: 1000},
	}
	SortClusterImagesBySizeDesc(rows)
	want := []string{"big", "a-mid", "z-mid", "small"}
	for i, w := range want {
		if rows[i].Ref != w {
			t.Errorf("row %d: got %q, want %q", i, rows[i].Ref, w)
		}
	}
}
