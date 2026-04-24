package serve

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"go.uber.org/zap"
)

// daemonEnv is set in the re-execed child to signal it should enter the
// daemon loop directly instead of re-exec'ing again.
const daemonEnv = "Y_CLUSTER_SERVE_DAEMON"

// daemonMode reports whether the current process is the re-execed child.
func daemonMode() bool { return os.Getenv(daemonEnv) == "1" }

// server is one listening port.
type server struct {
	port int
	mux  *http.ServeMux
	srv  *http.Server
}

// buildServers constructs the per-port handlers. Returns a startable
// slice and the list of health URLs Ensure probes after start.
func buildServers(cfgs []*Config, logger *zap.Logger) ([]*server, []string, error) {
	out := make([]*server, 0, len(cfgs))
	healthURLs := make([]string, 0, len(cfgs))
	for _, c := range cfgs {
		mux := http.NewServeMux()
		switch c.Type {
		case TypeYKustomizeLocal:
			b, err := newYKustomizeLocalBackend(c)
			if err != nil {
				return nil, nil, fmt.Errorf("port %d: %w", c.Port, err)
			}
			routes := make([]specRoute, 0, len(b.Routes()))
			for _, p := range b.Routes() {
				routes = append(routes, specRoute{Path: p, ContentType: b.RouteContentType(p)})
			}
			spec := newOpenAPISpec(
				fmt.Sprintf("y-cluster serve :%d", c.Port),
				TypeYKustomizeLocal,
				"dev",
				routes,
			).Render()

			mux.Handle("/health", HealthHandler(TypeYKustomizeLocal, map[string]any{"routes": len(b.Routes())}))
			mux.Handle("/openapi.yaml", OpenAPIHandler(spec))
			mux.Handle("/v1/", b)
			logger.Info("backend ready",
				zap.Int("port", c.Port),
				zap.String("type", string(c.Type)),
				zap.Int("routes", len(b.Routes())),
			)
		case TypeStatic:
			return nil, nil, fmt.Errorf("port %d: type %s is declared in the schema but not implemented in this release", c.Port, c.Type)
		default:
			return nil, nil, fmt.Errorf("port %d: unknown type %s", c.Port, c.Type)
		}
		s := &server{
			port: c.Port,
			mux:  mux,
			srv: &http.Server{
				Addr:              fmt.Sprintf(":%d", c.Port),
				Handler:           mux,
				ReadHeaderTimeout: 5 * time.Second,
			},
		}
		out = append(out, s)
		healthURLs = append(healthURLs, fmt.Sprintf("http://127.0.0.1:%d/health", c.Port))
	}
	return out, healthURLs, nil
}

// runDaemon blocks running every server until ctx is done or a server
// exits with an unrecoverable error. Respects SIGTERM/SIGINT via the
// ctx the caller passes in.
func runDaemon(ctx context.Context, servers []*server, logger *zap.Logger) error {
	var wg sync.WaitGroup
	errs := make(chan error, len(servers))
	for _, s := range servers {
		s := s
		ln, err := net.Listen("tcp", s.srv.Addr)
		if err != nil {
			return fmt.Errorf("listen :%d: %w", s.port, err)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			logger.Info("listening", zap.Int("port", s.port))
			if err := s.srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errs <- fmt.Errorf("serve :%d: %w", s.port, err)
			}
		}()
	}

	// Wait for context cancellation or a fatal listener error.
	select {
	case <-ctx.Done():
		logger.Info("shutdown signal received")
	case err := <-errs:
		logger.Error("server exited", zap.Error(err))
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, s := range servers {
		if err := s.srv.Shutdown(shutdownCtx); err != nil {
			logger.Warn("shutdown", zap.Int("port", s.port), zap.Error(err))
		}
	}
	wg.Wait()
	return nil
}

// stopByPidfile reads the pidfile, SIGTERMs the process, and waits for
// it to exit. Idempotent: zero-error if the pidfile is missing or the
// process is already gone.
func stopByPidfile(paths StatePaths, timeout time.Duration) error {
	pid, err := ReadPidfile(paths.Pid)
	if err != nil {
		// Corrupt pidfile — remove it and treat as stopped.
		_ = os.Remove(paths.Pid)
		return nil
	}
	if pid == 0 {
		// No pidfile — nothing to do.
		return nil
	}
	if !PidAlive(pid) {
		_ = os.Remove(paths.Pid)
		_ = os.Remove(paths.Config)
		return nil
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return fmt.Errorf("find process %d: %w", pid, err)
	}
	if err := proc.Signal(syscall.SIGTERM); err != nil {
		return fmt.Errorf("signal %d: %w", pid, err)
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !PidAlive(pid) {
			_ = os.Remove(paths.Pid)
			_ = os.Remove(paths.Config)
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	// Escalate.
	_ = proc.Signal(syscall.SIGKILL)
	time.Sleep(200 * time.Millisecond)
	if PidAlive(pid) {
		return fmt.Errorf("pid %d did not exit after SIGKILL", pid)
	}
	_ = os.Remove(paths.Pid)
	_ = os.Remove(paths.Config)
	return nil
}

// waitHealthy probes every /health URL until it returns 200 or timeout.
// Honors Q-S2: Ensure must not return before ports are accepting
// requests, otherwise scripts race the listener.
func waitHealthy(ctx context.Context, urls []string, timeout time.Duration) error {
	client := &http.Client{Timeout: 1 * time.Second}
	deadline := time.Now().Add(timeout)
	for _, u := range urls {
		for {
			if err := ctx.Err(); err != nil {
				return err
			}
			resp, err := client.Get(u)
			if err == nil {
				_ = resp.Body.Close()
				if resp.StatusCode == 200 {
					break
				}
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("health %s not ready after %s", u, timeout)
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	return nil
}

// withSignals wraps ctx with SIGTERM/SIGINT cancellation.
func withSignals(parent context.Context) (context.Context, context.CancelFunc) {
	return signal.NotifyContext(parent, syscall.SIGINT, syscall.SIGTERM)
}
