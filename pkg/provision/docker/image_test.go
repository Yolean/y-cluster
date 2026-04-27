package docker

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/Yolean/y-cluster/pkg/provision/config"
)

type stubExister struct {
	exists bool
	err    error
	calls  []string
}

func (s *stubExister) Exists(_ context.Context, ref string) (bool, error) {
	s.calls = append(s.calls, ref)
	return s.exists, s.err
}

func TestResolveImage_PrefersMirrorWhenPresent(t *testing.T) {
	stub := &stubExister{exists: true}
	got, fellBack, err := ResolveImage(context.Background(), "v1.32.4+k3s1", stub, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	if fellBack {
		t.Fatal("expected mirror hit, not fallback")
	}
	if got != config.MirrorImage("v1.32.4+k3s1") {
		t.Fatalf("got %q want mirror", got)
	}
	if len(stub.calls) != 1 || !strings.HasPrefix(stub.calls[0], config.K3sMirrorTarget()+":") {
		t.Fatalf("did not probe mirror: %v", stub.calls)
	}
}

func TestResolveImage_FallsBackWhenMirrorMissing(t *testing.T) {
	stub := &stubExister{exists: false}
	got, fellBack, err := ResolveImage(context.Background(), "v1.99.0+k3s1", stub, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	if !fellBack {
		t.Fatal("expected fallback")
	}
	if got != config.UpstreamImage("v1.99.0+k3s1") {
		t.Fatalf("got %q want upstream", got)
	}
}

func TestResolveImage_PropagatesProbeError(t *testing.T) {
	stub := &stubExister{err: errors.New("boom")}
	_, _, err := ResolveImage(context.Background(), "v1.32.4+k3s1", stub, zap.NewNop())
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("want probe error, got %v", err)
	}
}

func TestResolveImage_RejectsEmptyVersion(t *testing.T) {
	_, _, err := ResolveImage(context.Background(), "", &stubExister{}, zap.NewNop())
	if err == nil || !strings.Contains(err.Error(), "version") {
		t.Fatalf("want version error, got %v", err)
	}
}

// fakeRegistry serves the minimum of the OCI distribution v2 API
// that go-containerregistry's remote.Head needs:
//   - GET /v2/ -> 200 (signals "no auth required")
//   - HEAD /v2/<repo>/manifests/<tag> -> configurable status
//
// We don't bother with a token endpoint; remote.Head only escalates
// to the token flow when /v2/ returns 401 with a Bearer challenge.
func fakeRegistry(t *testing.T, manifestStatus int) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v2/" || r.URL.Path == "/v2":
			w.WriteHeader(http.StatusOK)
		case strings.Contains(r.URL.Path, "/manifests/"):
			if manifestStatus == http.StatusOK {
				body := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":"sha256:0000000000000000000000000000000000000000000000000000000000000000","size":0},"layers":[]}`)
				w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
				w.Header().Set("Docker-Content-Digest", "sha256:0000000000000000000000000000000000000000000000000000000000000000")
				w.Header().Set("Content-Length", fmt.Sprintf("%d", len(body)))
				w.WriteHeader(http.StatusOK)
				if r.Method != http.MethodHead {
					_, _ = w.Write(body)
				}
				return
			}
			w.WriteHeader(manifestStatus)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	return u.Host
}

// TestResolveImage_GHCRDeniedFallsBack wires the real
// DefaultManifestExister to a fake registry that returns 403
// DENIED for every manifest -- the GHCR-anonymous-on-private-repo
// shape that motivated the fix. We don't have a way to point
// ResolveImage at an arbitrary host (it derives the mirror from
// config), so we call DefaultManifestExister with the fake host
// directly and assert the bool/error contract ResolveImage relies
// on -- if Exists returns (false, nil) here, ResolveImage's
// fallback branch fires.
func TestResolveImage_GHCRDeniedFallsBack(t *testing.T) {
	host := fakeRegistry(t, http.StatusForbidden)
	ref := fmt.Sprintf("%s/yolean/k3s:v1.99.0-k3s1", host)
	exists, err := (DefaultManifestExister{}).Exists(context.Background(), ref)
	if err != nil {
		t.Fatalf("403 should classify as missing, got error: %v", err)
	}
	if exists {
		t.Fatal("403 should classify as missing, got exists=true")
	}
}

// TestDefaultManifestExister classifies HTTP responses the way
// ResolveImage's fallback decision needs. The 401/403 cases are
// the regression: GHCR returns 403 DENIED for a missing/private
// repo to anonymous probes; treating that as a hard error blocked
// the upstream fallback for fresh k3s versions.
func TestDefaultManifestExister(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		wantExists bool
		wantErr    bool
	}{
		{"200 -> exists", http.StatusOK, true, false},
		{"401 -> missing", http.StatusUnauthorized, false, false},
		{"403 -> missing", http.StatusForbidden, false, false},
		{"404 -> missing", http.StatusNotFound, false, false},
		{"500 -> error (don't silently fall back on outage)", http.StatusInternalServerError, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			host := fakeRegistry(t, tc.status)
			ref := fmt.Sprintf("%s/yolean/k3s:v1.99.0-k3s1", host)
			exists, err := (DefaultManifestExister{}).Exists(context.Background(), ref)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("status %d: want error, got exists=%v", tc.status, exists)
				}
				return
			}
			if err != nil {
				t.Fatalf("status %d: unexpected error: %v", tc.status, err)
			}
			if exists != tc.wantExists {
				t.Fatalf("status %d: exists=%v want %v", tc.status, exists, tc.wantExists)
			}
		})
	}
}
