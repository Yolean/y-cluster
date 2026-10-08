package dockerhost

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// healthTimeout bounds each daemon's check.
const healthTimeout = 10 * time.Second

// Health is the answer of both daemons, over TLS with the client
// certificate, as a client sees them.
type Health struct {
	// Docker is dockerd's Server header, such as "Docker/29.8.2 (linux)".
	Docker    string
	DockerErr error
	// BuildkitAnswer is the length in bytes of buildkitd's
	// ListWorkers message; more than zero means it has a worker.
	BuildkitAnswer int
	BuildkitErr    error
}

// OK reports whether both daemons answered.
func (h Health) OK() bool { return h.DockerErr == nil && h.BuildkitErr == nil }

func (h Health) Err() error {
	var parts []string
	if h.DockerErr != nil {
		parts = append(parts, "dockerd: "+h.DockerErr.Error())
	}
	if h.BuildkitErr != nil {
		parts = append(parts, "buildkitd: "+h.BuildkitErr.Error())
	}
	if len(parts) == 0 {
		return nil
	}
	return fmt.Errorf("%s", strings.Join(parts, "; "))
}

// clientTLS is the client side of the contract: the CA from the client
// directory, its certificate and key, and the address the server
// certificate must carry.
func clientTLS(clientDir, address string) (*tls.Config, error) {
	caPEM, err := os.ReadFile(filepath.Join(clientDir, "ca.pem"))
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("%s/ca.pem holds no certificate", clientDir)
	}
	cert, err := tls.LoadX509KeyPair(filepath.Join(clientDir, "cert.pem"), filepath.Join(clientDir, "key.pem"))
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		RootCAs:      pool,
		Certificates: []tls.Certificate{cert},
		ServerName:   address,
		MinVersion:   tls.VersionTLS12,
	}, nil
}

// checkHealth asks both daemons, the way their clients would.
func checkHealth(ctx context.Context, s State, clientDir string) Health {
	var h Health
	cfg, err := clientTLS(clientDir, s.Address)
	if err != nil {
		h.DockerErr, h.BuildkitErr = err, err
		return h
	}
	h.Docker, h.DockerErr = pingDocker(ctx, cfg, hostPort(s.Address, s.DockerPort))
	h.BuildkitAnswer, h.BuildkitErr = listBuildkitWorkers(ctx, cfg, hostPort(s.Address, s.BuildkitPort))
	return h
}

// newHTTPClient never keeps a connection: an idle connection to a
// daemon port would read as activity to the guest's idle reaper.
func newHTTPClient(cfg *tls.Config, h2 bool) *http.Client {
	return &http.Client{
		Timeout: healthTimeout,
		Transport: &http.Transport{
			TLSClientConfig:   cfg.Clone(),
			ForceAttemptHTTP2: h2,
			DisableKeepAlives: true,
		},
	}
}

// pingDocker is GET /_ping, Docker's own liveness endpoint.
func pingDocker(ctx context.Context, cfg *tls.Config, addr string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, healthTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+addr+"/_ping", nil)
	if err != nil {
		return "", err
	}
	resp, err := newHTTPClient(cfg, false).Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	if resp.StatusCode != http.StatusOK || string(body) != "OK" {
		return "", fmt.Errorf("GET /_ping: %s %q", resp.Status, body)
	}
	return resp.Header.Get("Server"), nil
}

// listBuildkitWorkers calls buildkitd's Control.ListWorkers as gRPC
// over HTTP/2 without a gRPC library: an empty request message in one
// length-prefixed frame. An answer with grpc-status 0 proves that the
// TLS session (client certificate included) was accepted and that the
// control API serves; a non-empty message means it has a worker.
func listBuildkitWorkers(ctx context.Context, cfg *tls.Config, addr string) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, healthTimeout)
	defer cancel()
	frame := []byte{0, 0, 0, 0, 0} // uncompressed, length 0
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://"+addr+"/moby.buildkit.v1.Control/ListWorkers", bytes.NewReader(frame))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/grpc")
	req.Header.Set("TE", "trailers")
	resp, err := newHTTPClient(cfg, true).Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return 0, err
	}
	if resp.ProtoMajor != 2 {
		return 0, fmt.Errorf("ListWorkers: answered over %s, not HTTP/2", resp.Proto)
	}
	status := resp.Trailer.Get("Grpc-Status")
	if status == "" {
		status = resp.Header.Get("Grpc-Status") // trailers-only answer
	}
	if resp.StatusCode != http.StatusOK || status != "0" {
		msg := resp.Trailer.Get("Grpc-Message")
		if msg == "" {
			msg = resp.Header.Get("Grpc-Message")
		}
		return 0, fmt.Errorf("ListWorkers: HTTP %d, grpc-status %q %s", resp.StatusCode, status, msg)
	}
	if len(body) < 5 {
		return 0, fmt.Errorf("ListWorkers: no message in the answer")
	}
	n := int(binary.BigEndian.Uint32(body[1:5]))
	if n == 0 {
		return 0, fmt.Errorf("ListWorkers: buildkitd has no worker")
	}
	return n, nil
}
