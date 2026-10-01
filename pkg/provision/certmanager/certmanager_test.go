package certmanager

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"sigs.k8s.io/yaml"
)

func TestVersionIsARelease(t *testing.T) {
	if !strings.HasPrefix(Version, "v1.") {
		t.Errorf("Version = %q, want a v1.x release tag", Version)
	}
}

// The first Ensure downloads into <cache>/certmanager/<version>/, the
// second is a cache hit with no request, and a failed download leaves
// nothing a later run would take for a hit.
func TestEnsure_DownloadsOnceThenCacheHit(t *testing.T) {
	var requests atomic.Int32
	failing := atomic.Bool{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if failing.Load() {
			http.Error(w, "down", http.StatusInternalServerError)
			return
		}
		if r.URL.Path != "/v9.9.9/cert-manager.yaml" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte("kind: Namespace\n"))
	}))
	t.Cleanup(srv.Close)
	prev := installURL
	installURL = srv.URL + "/%s/cert-manager.yaml"
	t.Cleanup(func() { installURL = prev })
	root := t.TempDir()

	failing.Store(true)
	if _, err := Ensure(context.Background(), EnsureOptions{Version: "v9.9.9", CacheOverride: root}); err == nil {
		t.Fatal("want an error from a failing upstream")
	}
	if _, err := os.Stat(filepath.Join(root, "certmanager", "v9.9.9", "cert-manager.yaml")); err == nil {
		t.Fatal("failed download left a cache file")
	}

	failing.Store(false)
	path, err := Ensure(context.Background(), EnsureOptions{Version: "v9.9.9", CacheOverride: root})
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(root, "certmanager", "v9.9.9", "cert-manager.yaml"); path != want {
		t.Errorf("path = %s, want %s", path, want)
	}
	before := requests.Load()
	if _, err := Ensure(context.Background(), EnsureOptions{Version: "v9.9.9", CacheOverride: root}); err != nil {
		t.Fatal(err)
	}
	if requests.Load() != before {
		t.Error("second Ensure downloaded again")
	}
}

// The issuers are the self-signed bootstrap, the CA it signs in the
// cert-manager namespace, and the CA ClusterIssuer over that secret.
func TestIssuersYAML(t *testing.T) {
	var got []string
	var caIssuerSecret, certSecret, certIssuer string
	for _, raw := range strings.Split(string(IssuersYAML()), "\n---\n") {
		var doc map[string]any
		if err := yaml.Unmarshal([]byte(raw), &doc); err != nil {
			t.Fatal(err)
		}
		md := doc["metadata"].(map[string]any)
		ns, _ := md["namespace"].(string)
		got = append(got, doc["kind"].(string)+"/"+ns+"/"+md["name"].(string))
		spec := doc["spec"].(map[string]any)
		if ca, ok := spec["ca"].(map[string]any); ok {
			caIssuerSecret = ca["secretName"].(string)
		}
		if doc["kind"] == "Certificate" {
			certSecret = spec["secretName"].(string)
			certIssuer = spec["issuerRef"].(map[string]any)["name"].(string)
			if spec["isCA"] != true {
				t.Error("the CA Certificate must set isCA")
			}
		}
	}
	want := []string{
		"ClusterIssuer//" + SelfSignedIssuer,
		"Certificate/" + Namespace + "/" + CACertificate,
		"ClusterIssuer//" + CAIssuer,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("objects = %q, want %q", got, want)
	}
	if certSecret != CASecret || caIssuerSecret != CASecret || certIssuer != SelfSignedIssuer {
		t.Errorf("wiring: certificate secret %q issuer %q, CA issuer secret %q", certSecret, certIssuer, caIssuerSecret)
	}
}

func TestImages(t *testing.T) {
	manifest := `apiVersion: apps/v1
kind: Deployment
metadata: {name: cert-manager}
spec:
  template:
    spec:
      containers:
      - name: c
        image: quay.io/jetstack/cert-manager-controller:v1.21.2
---
apiVersion: apps/v1
kind: Deployment
metadata: {name: cert-manager-webhook}
spec:
  template:
    spec:
      containers:
      - name: c
        image: quay.io/jetstack/cert-manager-webhook:v1.21.2
`
	refs, err := Images(strings.NewReader(manifest))
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 2 || !strings.Contains(refs[0], "cert-manager-controller") {
		t.Errorf("refs = %q", refs)
	}
}
