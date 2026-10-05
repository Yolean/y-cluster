package dockerhost

import (
	"encoding/binary"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// grpcHandler answers like a gRPC server: one length-prefixed message
// and the status in a trailer.
func grpcHandler(t *testing.T, message []byte, status string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor != 2 || r.Header.Get("Content-Type") != "application/grpc" || r.URL.Path != "/moby.buildkit.v1.Control/ListWorkers" {
			t.Errorf("unexpected request %s %s %s", r.Proto, r.URL.Path, r.Header.Get("Content-Type"))
		}
		w.Header().Set("Content-Type", "application/grpc")
		w.Header().Set("Trailer", "Grpc-Status, Grpc-Message")
		w.WriteHeader(http.StatusOK)
		if message != nil {
			frame := make([]byte, 5+len(message))
			binary.BigEndian.PutUint32(frame[1:5], uint32(len(message)))
			copy(frame[5:], message)
			_, _ = w.Write(frame)
		}
		w.Header().Set("Grpc-Status", status)
		if status != "0" {
			w.Header().Set("Grpc-Message", "unknown service")
		}
	})
}

func TestListBuildkitWorkers(t *testing.T) {
	m, err := issueTLS(net.ParseIP("127.0.0.1"), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "client")
	if err := writeClientDir(dir, m); err != nil {
		t.Fatal(err)
	}
	cfg, err := clientTLS(dir, "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		message []byte
		status  string
		wantErr string
	}{
		{"a worker", []byte{0x0a, 0x03, 'a', 'b', 'c'}, "0", ""},
		{"no worker", []byte{}, "0", "no worker"},
		{"unimplemented", nil, "12", "grpc-status \"12\""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := mtlsServer(t, m, grpcHandler(t, tc.message, tc.status), true)
			n, err := listBuildkitWorkers(t.Context(), cfg, srv.Listener.Addr().String())
			if tc.wantErr == "" {
				if err != nil || n != len(tc.message) {
					t.Fatalf("got %d, %v", n, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("want %q, got %d, %v", tc.wantErr, n, err)
			}
		})
	}
}

func TestPingDocker_NotOK(t *testing.T) {
	m, err := issueTLS(net.ParseIP("127.0.0.1"), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	srv := mtlsServer(t, m, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "starting", http.StatusServiceUnavailable)
	}), false)
	dir := filepath.Join(t.TempDir(), "client")
	if err := writeClientDir(dir, m); err != nil {
		t.Fatal(err)
	}
	cfg, err := clientTLS(dir, "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pingDocker(t.Context(), cfg, srv.Listener.Addr().String()); err == nil || !strings.Contains(err.Error(), "503") {
		t.Fatalf("want the status in the error, got %v", err)
	}
}

func TestCheckHealth_NoClientDir(t *testing.T) {
	h := checkHealth(t.Context(), State{Address: "127.0.0.1", DockerPort: "1", BuildkitPort: "2"}, filepath.Join(t.TempDir(), "missing"))
	if h.OK() || h.DockerErr == nil || h.BuildkitErr == nil {
		t.Fatalf("health without client files must fail for both: %+v", h)
	}
	if !strings.Contains(h.Err().Error(), "dockerd:") || !strings.Contains(h.Err().Error(), "buildkitd:") {
		t.Errorf("error names both daemons: %v", h.Err())
	}
}
