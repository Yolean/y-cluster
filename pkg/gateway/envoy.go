package gateway

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// Envoy is a sample of dataplane state from one envoy-gateway
// proxy pod. Captured live: we pick any one Running pod
// matching the envoy-gateway managed-by label, port-forward to
// its admin port (19000), and pull /server_info + /config_dump.
//
// "Any one pod" is deliberate. envoy-gateway runs the same
// rendered config on every replica, so a single sample
// represents the dataplane truth. Sampling N pods would
// multiply the JSON size without adding signal in the common
// case (a divergence between replicas is itself a bug, not a
// shape consumers should design around).
type Envoy struct {
	// Source identifies the proxy pod sampled, in
	// "<namespace>/<pod-name>" form. Documents WHERE the
	// snapshot came from so a future maintainer can correlate
	// against `kubectl describe pod` output.
	Source string `json:"source,omitempty"`

	// Version is the envoy build version from /server_info
	// (e.g. "1.34.1/Distribution/RELEASE/BoringSSL"). Empty
	// when /server_info isn't reachable or doesn't carry a
	// version field.
	Version string `json:"version,omitempty"`

	// Config is the verbatim /config_dump JSON (an envoy
	// ConfigDump message rendered as JSON). Loose-typed
	// (object) so envoy admin schema drift across versions
	// doesn't break our schema; consumers parse it as JSON.
	Config json.RawMessage `json:"config,omitempty" jsonschema:"type=object"`
}

// FetchEnvoy samples one envoy-gateway proxy pod's admin
// endpoints. Best-effort: returns (nil, nil) when no proxy pod
// is running yet (e.g. the cluster is pre-Gateway), and
// returns (nil, err) for transient kubectl / port-forward
// failures so the caller can decide whether to surface them.
//
// Implementation: kubectl port-forward to the pod's admin
// port. envoy admin binds 127.0.0.1:19000 inside the
// container, which means kubelet's pod-proxy (apiserver
// /pods/<n>:19000/proxy) can't reach it (localhost in the
// pod is not reachable from the host network namespace).
// port-forward streams through kubelet's port-forward
// mechanism, which executes inside the pod's network
// namespace, so it CAN reach localhost-bound admin.
//
// envoy-gateway proxy images are distroless: kubectl exec
// curl is not an option.
func FetchEnvoy(ctx context.Context, kubectlContext string) (*Envoy, error) {
	pod, err := pickEnvoyProxyPod(ctx, kubectlContext)
	if err != nil {
		return nil, fmt.Errorf("find envoy proxy pod: %w", err)
	}
	if pod == nil {
		// Pre-install or pre-reconcile cluster -- not an error.
		return nil, nil
	}

	port, cancel, err := startEnvoyAdminPortForward(ctx, kubectlContext, pod.Namespace, pod.Name)
	if err != nil {
		return nil, fmt.Errorf("port-forward to %s/%s: %w", pod.Namespace, pod.Name, err)
	}
	defer cancel()

	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	serverInfo, err := httpGetBody(ctx, base+"/server_info")
	if err != nil {
		return nil, fmt.Errorf("GET /server_info: %w", err)
	}
	configDump, err := httpGetBody(ctx, base+"/config_dump")
	if err != nil {
		return nil, fmt.Errorf("GET /config_dump: %w", err)
	}

	return &Envoy{
		Source:  pod.Namespace + "/" + pod.Name,
		Version: extractEnvoyVersion(serverInfo),
		Config:  json.RawMessage(configDump),
	}, nil
}

// envoyPod is the minimal pod identifier we need.
type envoyPod struct {
	Namespace string
	Name      string
}

// pickEnvoyProxyPod returns the first Running pod managed by
// envoy-gateway, or nil when none exists. We don't try to be
// clever about which pod: any replica's config is the same
// reconciled output, and a divergence between replicas would
// indicate a separate bug consumers can investigate via
// /pkg/gateway State.
func pickEnvoyProxyPod(ctx context.Context, kubectlContext string) (*envoyPod, error) {
	out, err := runKubectl(ctx, kubectlContext,
		"get", "pods", "-A",
		"-l", "app.kubernetes.io/managed-by=envoy-gateway",
		"--field-selector=status.phase=Running",
		"-o", "json",
	)
	if err != nil {
		return nil, err
	}
	var list struct {
		Items []struct {
			Metadata struct {
				Namespace string `json:"namespace"`
				Name      string `json:"name"`
			} `json:"metadata"`
		} `json:"items"`
	}
	if err := json.Unmarshal(out, &list); err != nil {
		return nil, fmt.Errorf("unmarshal pods: %w", err)
	}
	if len(list.Items) == 0 {
		return nil, nil
	}
	return &envoyPod{
		Namespace: list.Items[0].Metadata.Namespace,
		Name:      list.Items[0].Metadata.Name,
	}, nil
}

// startEnvoyAdminPortForward starts a `kubectl port-forward
// pod/<name> :19000` and waits for the "Forwarding from
// 127.0.0.1:<port> -> 19000" line. Returns the chosen local
// port plus a cancel func the caller MUST defer to stop the
// background process.
//
// We let kubectl pick the local port (`:19000` syntax) so
// concurrent invocations don't collide.
func startEnvoyAdminPortForward(ctx context.Context, kubectlContext, namespace, podName string) (int, func(), error) {
	pfCtx, cancel := context.WithCancel(ctx)
	cmd := exec.CommandContext(pfCtx, "kubectl",
		"--context="+kubectlContext,
		"port-forward",
		"-n", namespace,
		"pod/"+podName,
		":19000",
	)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return 0, nil, err
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		cancel()
		return 0, nil, err
	}

	cleanup := func() {
		cancel()
		_ = cmd.Wait()
	}

	portCh := make(chan int, 1)
	errCh := make(chan error, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			line := scanner.Text()
			const prefix = "Forwarding from 127.0.0.1:"
			idx := strings.Index(line, prefix)
			if idx < 0 {
				continue
			}
			rest := line[idx+len(prefix):]
			portStr := strings.SplitN(rest, " ", 2)[0]
			p, err := strconv.Atoi(portStr)
			if err != nil {
				errCh <- fmt.Errorf("parse port from %q: %w", line, err)
				return
			}
			portCh <- p
			// Keep draining stdout so the pipe doesn't fill up
			// and block the kubectl process.
			go func() { _, _ = io.Copy(io.Discard, stdout) }()
			return
		}
		if err := scanner.Err(); err != nil {
			errCh <- err
			return
		}
		errCh <- errors.New("kubectl port-forward exited without printing forwarding line")
	}()

	select {
	case <-time.After(15 * time.Second):
		cleanup()
		return 0, nil, errors.New("timed out waiting for kubectl port-forward to bind")
	case err := <-errCh:
		cleanup()
		return 0, nil, err
	case p := <-portCh:
		return p, cleanup, nil
	}
}

// httpGetBody is a context-aware GET that returns the body
// bytes or an error. Short timeout: envoy admin is local
// (loopback through port-forward) and should respond
// instantly; anything slower is a bug.
func httpGetBody(ctx context.Context, url string) ([]byte, error) {
	reqCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("status %d: %s", resp.StatusCode, body)
	}
	return io.ReadAll(resp.Body)
}

// extractEnvoyVersion pulls the version string out of an envoy
// /server_info response. The doc has a top-level `version`
// field plus a nested build identifier; we surface the top-level
// one (e.g. "1.34.1/Distribution/RELEASE/BoringSSL") which is
// what `envoy --version` prints.
func extractEnvoyVersion(serverInfo []byte) string {
	var doc struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(serverInfo, &doc); err != nil {
		return ""
	}
	return doc.Version
}
