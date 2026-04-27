package envoygateway

import (
	"strings"
	"testing"
)

// fakeInstallYAML is a tiny stand-in for the upstream install
// manifest: enough shape for pkg/images.ListYAML to find image
// refs, including the duplicates real install.yaml has so the
// dedup contract is exercised.
const fakeInstallYAML = `---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: envoy-gateway
  namespace: envoy-gateway-system
spec:
  template:
    spec:
      initContainers:
      - name: init
        image: envoyproxy/gateway:v1.7.2
      containers:
      - name: ctrl
        image: envoyproxy/gateway:v1.7.2
      - name: ratelimit
        image: docker.io/envoyproxy/ratelimit:05c08d03
`

func TestImages_HasController(t *testing.T) {
	got, err := Images(strings.NewReader(fakeInstallYAML))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 {
		t.Fatal("Images() returned no refs")
	}
	want := "envoyproxy/gateway:v1.7.2"
	found := false
	for _, ref := range got {
		if strings.Contains(ref, want) {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("controller image %q not found in %v", want, got)
	}
}

// TestImages_Deduplicated -- a real install.yaml lists the EG
// image on both initContainer and main container; our fake does
// the same. ListYAML guarantees deduplication; this pins that
// contract so a future refactor can't regress it.
func TestImages_Deduplicated(t *testing.T) {
	got, err := Images(strings.NewReader(fakeInstallYAML))
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, ref := range got {
		if seen[ref] {
			t.Fatalf("duplicate ref in Images(): %q", ref)
		}
		seen[ref] = true
	}
}
