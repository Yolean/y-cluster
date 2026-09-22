package yconverge

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestParseGatewayProbeOutput_HappyPath pins the curl -w shape we
// emit. The probe code reads HTTP_CODE and LOCATION lines; any
// surrounding noise (kubectl run banners, etc.) is ignored.
func TestParseGatewayProbeOutput_HappyPath(t *testing.T) {
	in := "HTTP_CODE:302\nLOCATION:http://dev.yolean.net/auth/realms/dev/openid?x=1\n"
	got, err := parseGatewayProbeOutput(in)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got.HTTPCode != 302 {
		t.Errorf("HTTPCode: %d, want 302", got.HTTPCode)
	}
	if got.Location != "http://dev.yolean.net/auth/realms/dev/openid?x=1" {
		t.Errorf("Location: %q", got.Location)
	}
}

// TestParseGatewayProbeOutput_NoLocation: a 200 has no Location.
// The probe must accept that without erroring.
func TestParseGatewayProbeOutput_NoLocation(t *testing.T) {
	got, err := parseGatewayProbeOutput("HTTP_CODE:200\nLOCATION:\n")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got.HTTPCode != 200 {
		t.Errorf("HTTPCode: %d", got.HTTPCode)
	}
	if got.Location != "" {
		t.Errorf("Location: %q (want empty)", got.Location)
	}
}

// TestParseGatewayProbeOutput_MissingCode: a probe that printed
// nothing (or no HTTP_CODE) fails the parse so the caller surfaces
// the failure as "probe didn't reach the server" rather than
// silently passing.
func TestParseGatewayProbeOutput_MissingCode(t *testing.T) {
	if _, err := parseGatewayProbeOutput("LOCATION:somewhere\n"); err == nil {
		t.Fatal("expected error for output without HTTP_CODE")
	}
	if _, err := parseGatewayProbeOutput(""); err == nil {
		t.Fatal("expected error for empty output")
	}
}

// TestParseGatewayProbeOutput_MalformedCode catches the case where
// curl printed something non-numeric where the http_code goes.
func TestParseGatewayProbeOutput_MalformedCode(t *testing.T) {
	_, err := parseGatewayProbeOutput("HTTP_CODE:not-a-number\nLOCATION:\n")
	if err == nil {
		t.Fatal("expected parse error")
	}
	if !strings.Contains(err.Error(), "HTTP_CODE") {
		t.Errorf("error should mention HTTP_CODE: %v", err)
	}
}

// TestValidateGatewayProbeResult_DefaultCode: empty ExpectCodes
// defaults to {200}.
func TestValidateGatewayProbeResult_DefaultCode(t *testing.T) {
	if err := validateGatewayProbeResult(gatewayProbeOpts{},
		&gatewayProbeResult{HTTPCode: 200}); err != nil {
		t.Errorf("default 200 should pass: %v", err)
	}
	if err := validateGatewayProbeResult(gatewayProbeOpts{},
		&gatewayProbeResult{HTTPCode: 500}); err == nil {
		t.Errorf("500 should fail default-200 validation")
	}
}

// TestValidateGatewayProbeResult_CodeList: any of the listed
// codes passes; outside the list fails.
func TestValidateGatewayProbeResult_CodeList(t *testing.T) {
	opts := gatewayProbeOpts{ExpectCodes: []int{200, 204, 302}}
	for _, code := range []int{200, 204, 302} {
		if err := validateGatewayProbeResult(opts,
			&gatewayProbeResult{HTTPCode: code}); err != nil {
			t.Errorf("code %d should pass: %v", code, err)
		}
	}
	if err := validateGatewayProbeResult(opts,
		&gatewayProbeResult{HTTPCode: 301}); err == nil {
		t.Error("301 should fail [200,204,302] validation")
	}
}

// TestValidateGatewayProbeResult_LocationRegex covers the canonical
// reproducer: 302 status + Location regex pinning the redirect
// target. This is the false-positive class kind: "exec" with
// `curl | grep 302` could not catch.
func TestValidateGatewayProbeResult_LocationRegex(t *testing.T) {
	opts := gatewayProbeOpts{
		ExpectCodes:    []int{302},
		ExpectLocation: `^http://dev\.yolean\.net/auth/realms/[^/]+/protocol/openid-connect/auth\?.*`,
	}
	good := &gatewayProbeResult{
		HTTPCode: 302,
		Location: "http://dev.yolean.net/auth/realms/dev/protocol/openid-connect/auth?response_type=code",
	}
	if err := validateGatewayProbeResult(opts, good); err != nil {
		t.Errorf("expected pass: %v", err)
	}
	wrongRealm := &gatewayProbeResult{
		HTTPCode: 302,
		Location: "https://login.example.com/oauth/authorize?...",
	}
	if err := validateGatewayProbeResult(opts, wrongRealm); err == nil {
		t.Error("expected Location regex failure for wrong-realm redirect")
	}
}

// TestValidateGatewayProbeResult_InvalidLocationRegex pins the
// "regex compile fails" failure mode -- author error in the cue
// file should surface a clear message.
func TestValidateGatewayProbeResult_InvalidLocationRegex(t *testing.T) {
	opts := gatewayProbeOpts{
		ExpectCodes:    []int{302},
		ExpectLocation: `[unclosed`,
	}
	err := validateGatewayProbeResult(opts, &gatewayProbeResult{HTTPCode: 302, Location: "x"})
	if err == nil {
		t.Fatal("expected error for malformed regex")
	}
	if !strings.Contains(err.Error(), "expectLocation") {
		t.Errorf("error should mention expectLocation: %v", err)
	}
}

// TestSplitURLHostPort covers the four cases the dial-target needs:
// scheme-defaulted ports, explicit ports, missing host, unsupported
// scheme.
func TestSplitURLHostPort(t *testing.T) {
	cases := []struct {
		in       string
		wantHost string
		wantPort string
		wantErr  bool
	}{
		{"http://dev.yolean.net/", "dev.yolean.net", "80", false},
		{"https://keycloak-admin/auth/", "keycloak-admin", "443", false},
		{"http://blobs:9000/", "blobs", "9000", false},
		{"http://[::1]:8080/x", "::1", "8080", false},
		// Errors:
		{"://no-scheme", "", "", true},
		{"ftp://example/", "", "", true}, // unsupported scheme without port
	}
	for _, c := range cases {
		host, port, err := splitURLHostPort(c.in)
		if (err != nil) != c.wantErr {
			t.Errorf("%q: err=%v wantErr=%v", c.in, err, c.wantErr)
			continue
		}
		if c.wantErr {
			continue
		}
		if host != c.wantHost || port != c.wantPort {
			t.Errorf("%q: got %s:%s, want %s:%s", c.in, host, port, c.wantHost, c.wantPort)
		}
	}
}

// TestPickGatewayAddress_ClassMatch: with className narrowed,
// only Gateways of that class contribute, and we pick the first
// programmed address.
func TestPickGatewayAddress_ClassMatch(t *testing.T) {
	items := []gatewayInfo{
		{}, // no spec class, no addresses -- skipped
	}
	items[0].Spec.GatewayClassName = "other-class"
	items[0].Status.Addresses = []struct {
		Type  string `json:"type"`
		Value string `json:"value"`
	}{{Type: "IPAddress", Value: "10.0.0.1"}}

	wanted := gatewayInfo{}
	wanted.Spec.GatewayClassName = "y-cluster"
	wanted.Status.Addresses = []struct {
		Type  string `json:"type"`
		Value string `json:"value"`
	}{{Type: "IPAddress", Value: "10.0.2.15"}}
	items = append(items, wanted)

	got := pickGatewayAddress(items, "y-cluster")
	if got != "10.0.2.15" {
		t.Errorf("got %q, want 10.0.2.15 (matched-class)", got)
	}
}

// TestPickGatewayAddress_AnyClass: empty className -> first
// programmed address wins regardless of class.
func TestPickGatewayAddress_AnyClass(t *testing.T) {
	items := []gatewayInfo{{}, {}}
	items[0].Spec.GatewayClassName = "first-class"
	items[0].Status.Addresses = []struct {
		Type  string `json:"type"`
		Value string `json:"value"`
	}{{Type: "IPAddress", Value: "1.1.1.1"}}
	items[1].Spec.GatewayClassName = "second-class"
	items[1].Status.Addresses = []struct {
		Type  string `json:"type"`
		Value string `json:"value"`
	}{{Type: "IPAddress", Value: "2.2.2.2"}}

	got := pickGatewayAddress(items, "")
	if got != "1.1.1.1" {
		t.Errorf("got %q, want 1.1.1.1 (first programmed)", got)
	}
}

// TestPickGatewayAddress_NoneProgrammed: no Gateway has a
// non-empty status.addresses[].value -> "" so the caller's retry
// loop knows to wait.
func TestPickGatewayAddress_NoneProgrammed(t *testing.T) {
	items := []gatewayInfo{{}}
	items[0].Spec.GatewayClassName = "y-cluster"
	if got := pickGatewayAddress(items, "y-cluster"); got != "" {
		t.Errorf("got %q, want empty (none programmed)", got)
	}
	if got := pickGatewayAddress(items, ""); got != "" {
		t.Errorf("got %q, want empty (none programmed, any class)", got)
	}
}

// TestPickGatewayAddress_ClassNoMatch: className narrows past
// every Gateway -> "" (no match). Distinguishable from
// "none programmed" only via the kubectl get's exit code, which
// the discoverGatewayAddress wrapper handles.
func TestPickGatewayAddress_ClassNoMatch(t *testing.T) {
	items := []gatewayInfo{{}}
	items[0].Spec.GatewayClassName = "actual-class"
	items[0].Status.Addresses = []struct {
		Type  string `json:"type"`
		Value string `json:"value"`
	}{{Type: "IPAddress", Value: "10.0.0.1"}}
	if got := pickGatewayAddress(items, "wrong-class"); got != "" {
		t.Errorf("got %q, want empty (no class match)", got)
	}
}

// kubectl run --rm only cleans up when kubectl gets to the end. A
// probe whose kubectl fails (or is killed at the deadline) has to be
// deleted by us, or a retry loop leaves one pod per attempt.
func TestRunGatewayProbe_DeletesPodWhenRunFails(t *testing.T) {
	log := filepath.Join(t.TempDir(), "kubectl.log")
	// One log line per invocation; curl's -w template has newlines.
	fakeKubectlOnPATH(t, `echo "$*" | tr '\n' ' ' >> `+log+`; echo >> `+log+`
case "$*" in
  *" run "*) echo "error: timed out waiting for the condition" >&2; exit 1 ;;
esac`)

	err := runGatewayProbe(context.Background(), "ctx", gatewayProbeOpts{
		URL:     "http://app.example.test/",
		Resolve: "10.0.0.1",
	})
	if err == nil {
		t.Fatal("expected the probe to fail")
	}
	data, readErr := os.ReadFile(log)
	if readErr != nil {
		t.Fatal(readErr)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatalf("want a run followed by a delete, got:\n%s\n(probe error: %v)", data, err)
	}
	runFields := strings.Fields(lines[0])
	podName := ""
	for i, f := range runFields {
		if f == "run" && i+1 < len(runFields) {
			podName = runFields[i+1]
		}
	}
	if !strings.HasPrefix(podName, "yconverge-probe-") {
		t.Fatalf("could not find the probe pod name in: %s", lines[0])
	}
	for _, want := range []string{"--context=ctx", "delete pod " + podName, "--ignore-not-found"} {
		if !strings.Contains(lines[1], want) {
			t.Errorf("cleanup call lacks %q: %s", want, lines[1])
		}
	}
}

// A redirect check against a response without a Location header
// fails; the header is matched as the empty string.
func TestValidateGatewayProbeResult_MissingLocationFails(t *testing.T) {
	err := validateGatewayProbeResult(
		gatewayProbeOpts{ExpectCodes: []int{302}, ExpectLocation: `^https://login\.`},
		&gatewayProbeResult{HTTPCode: 302, Location: ""},
	)
	if err == nil {
		t.Fatal("a 302 without Location must not satisfy expectLocation")
	}
}
