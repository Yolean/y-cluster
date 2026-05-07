package yconverge

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// curlImage is the ephemeral probe Pod's image. Pinned by tag for
// reproducibility; can be overridden via opts.Image (used by tests
// to point at a local image and skip the pull).
const curlImage = "curlimages/curl:8.10.1"

// gatewayProbeOpts captures the runtime shape of a `kind: "gateway"`
// check. Pulled out as a value type so the validation half can be
// unit-tested without shelling out to kubectl.
type gatewayProbeOpts struct {
	// URL is the request target including scheme, host, optional
	// port, and path. Host header for the request is derived from
	// URL.Host so an HTTPRoute matching by Host actually fires.
	URL string
	// ExpectCodes, if non-empty, lists the response status codes
	// that pass the check. Empty defaults to {200}.
	ExpectCodes []int
	// ExpectLocation, if non-empty, is a Go regexp that must match
	// the Location response header. Pairs with 3xx ExpectCodes;
	// silently passes against responses with no Location.
	ExpectLocation string
	// Resolve, if non-empty, is the dial target IP for the URL's
	// host:port. Bypasses Gateway address discovery.
	Resolve string
	// GatewayClassName narrows discovery to Gateways of this class.
	// Empty means: pick from the only Gateway present (errors if
	// multiple distinct class names exist).
	GatewayClassName string
	// Image overrides curlImage; for tests.
	Image string
}

// gatewayProbeResult is the parsed curl response: just the bits we
// validate against. Body capture is deferred (cap + regex match
// would land here as a follow-up).
type gatewayProbeResult struct {
	HTTPCode int
	Location string
}

// parseGatewayProbeOutput parses the `-w` template our curl
// invocation emits:
//
//	HTTP_CODE:<int>
//	LOCATION:<url-or-empty>
//
// Returns an error if HTTP_CODE is missing -- a probe that didn't
// produce one is a probe that didn't reach the server, and the
// caller surfaces that as a probe failure.
func parseGatewayProbeOutput(s string) (*gatewayProbeResult, error) {
	r := &gatewayProbeResult{}
	seenCode := false
	for _, line := range strings.Split(strings.TrimSpace(s), "\n") {
		switch {
		case strings.HasPrefix(line, "HTTP_CODE:"):
			v := strings.TrimPrefix(line, "HTTP_CODE:")
			n, err := strconv.Atoi(strings.TrimSpace(v))
			if err != nil {
				return nil, fmt.Errorf("parse HTTP_CODE %q: %w", v, err)
			}
			r.HTTPCode = n
			seenCode = true
		case strings.HasPrefix(line, "LOCATION:"):
			r.Location = strings.TrimPrefix(line, "LOCATION:")
		}
	}
	if !seenCode {
		return nil, fmt.Errorf("no HTTP_CODE in probe output:\n%s", s)
	}
	return r, nil
}

// validateGatewayProbeResult applies opts' expectations to r.
// Returns nil on a fully-passing probe.
func validateGatewayProbeResult(opts gatewayProbeOpts, r *gatewayProbeResult) error {
	codes := opts.ExpectCodes
	if len(codes) == 0 {
		codes = []int{200}
	}
	matched := false
	for _, c := range codes {
		if r.HTTPCode == c {
			matched = true
			break
		}
	}
	if !matched {
		return fmt.Errorf("expected status %v, got %d", codes, r.HTTPCode)
	}
	if opts.ExpectLocation != "" {
		re, err := regexp.Compile(opts.ExpectLocation)
		if err != nil {
			return fmt.Errorf("invalid expectLocation regex %q: %w", opts.ExpectLocation, err)
		}
		if !re.MatchString(r.Location) {
			return fmt.Errorf("expected Location to match %q, got %q", opts.ExpectLocation, r.Location)
		}
	}
	return nil
}

// splitURLHostPort extracts host + dial port from a URL. http
// defaults to 80, https to 443. Other schemes are an error since
// curl --resolve needs an explicit port.
func splitURLHostPort(rawURL string) (host, port string, err error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", "", fmt.Errorf("parse url %q: %w", rawURL, err)
	}
	host = u.Hostname()
	if host == "" {
		return "", "", fmt.Errorf("url %q has no host", rawURL)
	}
	port = u.Port()
	if port == "" {
		switch u.Scheme {
		case "http":
			port = "80"
		case "https":
			port = "443"
		default:
			return "", "", fmt.Errorf("url %q has no port and unsupported scheme %q", rawURL, u.Scheme)
		}
	}
	return host, port, nil
}

// gatewayInfo is the slice of `kubectl get gateway -A -o json`
// output we care about: namespace + name (for diagnostics), the
// configured class, and the controller-reported addresses.
type gatewayInfo struct {
	Metadata struct {
		Name      string `json:"name"`
		Namespace string `json:"namespace"`
	} `json:"metadata"`
	Spec struct {
		GatewayClassName string `json:"gatewayClassName"`
	} `json:"spec"`
	Status struct {
		Addresses []struct {
			Type  string `json:"type"`
			Value string `json:"value"`
		} `json:"addresses"`
	} `json:"status"`
}

// pickGatewayAddress applies the className filter to a list of
// Gateway objects and returns the first programmed address. Pulled
// out as a pure function so the discovery wrapper stays a thin
// kubectl-out shellout that can be mocked in tests.
//
// Behaviour:
//
//	className != ""        -> only Gateways with that class match
//	className == ""        -> all Gateways are candidates
//	no candidate has an
//	  address yet           -> ("", nil)   caller retries
//	one or more candidates
//	  with addresses        -> first non-empty Status.Addresses[i].Value
func pickGatewayAddress(items []gatewayInfo, className string) string {
	for _, g := range items {
		if className != "" && g.Spec.GatewayClassName != className {
			continue
		}
		for _, a := range g.Status.Addresses {
			if a.Value != "" {
				return a.Value
			}
		}
	}
	return ""
}

// discoverGatewayAddress walks the cluster's Gateways and returns
// the first programmed address matching opts.GatewayClassName (or
// the first programmed Gateway in any class when empty). Returns
// "" + nil error when no programmed Gateway exists yet -- the
// caller's retry-until-timeout loop catches that as transient.
func discoverGatewayAddress(ctx context.Context, contextName, className string) (string, error) {
	cmd := exec.CommandContext(ctx, "kubectl",
		"--context="+contextName,
		"get", "gateway", "-A",
		"-o", "json",
	)
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("kubectl get gateway: %w", err)
	}
	var list struct {
		Items []gatewayInfo `json:"items"`
	}
	if err := json.Unmarshal(out, &list); err != nil {
		return "", fmt.Errorf("parse gateway list: %w", err)
	}
	return pickGatewayAddress(list.Items, className), nil
}

// runGatewayProbe is a single probe attempt: discover + dial +
// parse + validate. The retry-until-timeout shape lives in the
// caller (CheckRunner.runGateway) so the unit-testable surface
// here stays one round-trip.
func runGatewayProbe(ctx context.Context, contextName string, opts gatewayProbeOpts) error {
	addr := opts.Resolve
	if addr == "" {
		var err error
		addr, err = discoverGatewayAddress(ctx, contextName, opts.GatewayClassName)
		if err != nil {
			return fmt.Errorf("discover Gateway address: %w", err)
		}
		if addr == "" {
			cn := opts.GatewayClassName
			if cn == "" {
				cn = "(any)"
			}
			return fmt.Errorf("no Gateway in class %s has a programmed address yet", cn)
		}
	}
	host, port, err := splitURLHostPort(opts.URL)
	if err != nil {
		return err
	}

	image := opts.Image
	if image == "" {
		image = curlImage
	}
	// Pod name uses ns-suffix random for collision-resistance under
	// a fast-retry loop; --restart=Never + --rm tears the Pod down
	// at exit. -- separates kubectl-run flags from the curl argv.
	podName := fmt.Sprintf("yconverge-probe-%d", time.Now().UnixNano())
	curlArgs := []string{
		"-sS", "--max-time", "10",
		"-o", "/dev/null",
		"-w", "HTTP_CODE:%{http_code}\nLOCATION:%{redirect_url}\n",
		"--resolve", host + ":" + port + ":" + addr,
		"-H", "Host: " + host,
		opts.URL,
	}
	args := append([]string{
		"--context=" + contextName,
		"run", podName,
		"--restart=Never",
		"--rm", "-i",
		"--image=" + image,
		"--quiet",
		"--command", "--",
		"curl",
	}, curlArgs...)
	cmd := exec.CommandContext(ctx, "kubectl", args...)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("probe pod %s: %w (stdout: %q stderr: %q)",
			podName, err, stdout.String(), stderr.String())
	}
	result, err := parseGatewayProbeOutput(stdout.String())
	if err != nil {
		return err
	}
	return validateGatewayProbeResult(opts, result)
}
