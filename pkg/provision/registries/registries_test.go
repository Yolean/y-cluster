package registries

import (
	"archive/tar"
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/Yolean/y-cluster/pkg/provision/config"
)

func TestMarshal_EmptyReturnsNil(t *testing.T) {
	out, err := Marshal(config.Registries{})
	if err != nil {
		t.Fatal(err)
	}
	if out != nil {
		t.Fatalf("empty Registries should marshal to nil, got %q", out)
	}
}

// TestMarshal_K3sShape ensures the byte layout matches what k3s
// reads at /etc/rancher/k3s/registries.yaml. Two-space indent,
// lowercase keys, no document marker.
func TestMarshal_K3sShape(t *testing.T) {
	r := config.Registries{
		Mirrors: map[string]config.RegistryMirror{
			"prod-registry.ystack.svc.cluster.local": {
				Endpoint: []string{"http://10.43.0.51"},
			},
			"europe-docker.pkg.dev": {
				Endpoint: []string{"https://europe-docker.pkg.dev"},
				Rewrite:  map[string]string{"^artifactrepo/(.*)": "yolean-prod/artifactrepo/$1"},
			},
		},
		Configs: map[string]config.RegistryConfig{
			"europe-docker.pkg.dev": {
				Auth: &config.RegistryAuth{
					Username: "oauth2accesstoken",
					Password: "ya29.test-token",
				},
			},
		},
	}
	out, err := Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	body := string(out)

	for _, want := range []string{
		"mirrors:",
		"prod-registry.ystack.svc.cluster.local:",
		"endpoint:\n    - http://10.43.0.51",
		"europe-docker.pkg.dev:",
		"rewrite:",
		"^artifactrepo/(.*): yolean-prod/artifactrepo/$1",
		"configs:",
		"auth:",
		"username: oauth2accesstoken",
		"password: ya29.test-token",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("rendered yaml missing %q.\nGot:\n%s", want, body)
		}
	}
	if strings.Contains(body, "---") {
		t.Errorf("k3s registries.yaml should not include a document marker")
	}
}

// TestTar_HasDirsAndFile parses the tar Tar() returns and checks
// that intermediate directories exist (CopyToContainer doesn't
// auto-mkdir) and that the file body matches.
func TestTar_HasDirsAndFile(t *testing.T) {
	body := []byte("mirrors: {}\n")
	archive, err := Tar(body)
	if err != nil {
		t.Fatal(err)
	}

	want := map[string]struct {
		typ  byte
		body string
	}{
		"etc/":                            {typ: tar.TypeDir},
		"etc/rancher/":                    {typ: tar.TypeDir},
		"etc/rancher/k3s/":                {typ: tar.TypeDir},
		"etc/rancher/k3s/registries.yaml": {typ: tar.TypeReg, body: string(body)},
	}
	got := map[string]struct {
		typ  byte
		body string
	}{}
	tr := tar.NewReader(bytes.NewReader(archive))
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		var b strings.Builder
		if h.Typeflag == tar.TypeReg {
			if _, err := io.Copy(&b, tr); err != nil {
				t.Fatal(err)
			}
		}
		got[h.Name] = struct {
			typ  byte
			body string
		}{h.Typeflag, b.String()}
	}
	if len(got) != len(want) {
		t.Fatalf("tar entries: got %d (%v), want %d (%v)", len(got), got, len(want), want)
	}
	for name, w := range want {
		g, ok := got[name]
		if !ok {
			t.Errorf("missing tar entry %q", name)
			continue
		}
		if g.typ != w.typ {
			t.Errorf("tar %q: typeflag %v want %v", name, g.typ, w.typ)
		}
		if g.body != w.body {
			t.Errorf("tar %q: body %q want %q", name, g.body, w.body)
		}
	}
}

func TestPath(t *testing.T) {
	if Path != "/etc/rancher/k3s/registries.yaml" {
		t.Fatalf("k3s reads from a fixed path; do not change without coordinating with both providers: %q", Path)
	}
}
