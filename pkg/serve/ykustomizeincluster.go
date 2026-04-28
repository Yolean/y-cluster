package serve

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
)

const (
	// ykInClusterSecretPrefix is the naming convention inherited
	// from ystack: Secret name `y-kustomize.{group}.{name}`.
	ykInClusterSecretPrefix = "y-kustomize."

	// ykInClusterDefaultLabel is the Secret label selector ystack
	// applies to every y-kustomize Secret. Configurable via
	// inCluster.labelSelector.
	ykInClusterDefaultLabel = "yolean.se/module-part=y-kustomize"

	// ykInClusterDefaultPoll is the interval between Secret list
	// refreshes. The previous design used a client-go informer
	// (long-running watch with apiserver-side timeouts and resume
	// semantics); polling is dramatically simpler and acceptable
	// here because the served set is small and the consumer
	// tolerates a few seconds of staleness.
	//
	// Configurable via inCluster.pollInterval.
	ykInClusterDefaultPoll = 5 * time.Second
)

// ykInClusterRoute is an in-memory served path. Unlike the local
// backend which reads from disk on every request, the in-cluster
// backend keeps bodies in memory because the authoritative source
// is the last poll's response.
type ykInClusterRoute struct {
	Path        string
	ContentType string
	Body        []byte
}

// secretLister is the minimal apiserver shape this backend needs.
// pkg/serve/kubeclient.go's k8sClient is the production
// implementation; tests inject an httptest-backed fake.
type secretLister interface {
	listSecrets(ctx context.Context, labelSelector string) ([]byte, error)
}

// ykInClusterBackend polls the apiserver for matching Secrets and
// serves each Secret's data keys at `/v1/{group}/{name}/{key}`.
type ykInClusterBackend struct {
	namespace    string
	selector     string
	pollInterval time.Duration
	logger       *zap.Logger

	client secretLister
	stopCh chan struct{}

	mu     sync.RWMutex
	routes map[string]ykInClusterRoute
}

// clientFactory is an injection point so tests can substitute an
// httptest-backed fake without dragging real kubeconfig loading.
type clientFactory func(cfg YKustomizeInClusterConfig) (secretLister, string, error)

var defaultClientFactory clientFactory = func(cfg YKustomizeInClusterConfig) (secretLister, string, error) {
	c, err := newK8sClient(cfg)
	if err != nil {
		return nil, "", err
	}
	return c, c.namespace, nil
}

// newYKustomizeInClusterBackend builds a backend, runs an initial
// refresh, and starts the polling goroutine. The goroutine stops
// when ctx is cancelled or Close() is called.
func newYKustomizeInClusterBackend(ctx context.Context, cfg *Config, logger *zap.Logger) (*ykInClusterBackend, error) {
	return newYKustomizeInClusterBackendWith(ctx, cfg, logger, defaultClientFactory)
}

func newYKustomizeInClusterBackendWith(ctx context.Context, cfg *Config, logger *zap.Logger, cf clientFactory) (*ykInClusterBackend, error) {
	if cfg.Type != TypeYKustomizeInCluster {
		return nil, fmt.Errorf("not a y-kustomize-incluster config: %s", cfg.Type)
	}
	ic := cfg.InCluster
	if ic == nil {
		return nil, fmt.Errorf("inCluster config missing after validate") // defensive
	}

	client, namespace, err := cf(*ic)
	if err != nil {
		return nil, err
	}

	selector := ic.LabelSelector
	if selector == "" {
		selector = ykInClusterDefaultLabel
	}

	poll := ic.PollInterval
	if poll <= 0 {
		poll = ykInClusterDefaultPoll
	}

	b := &ykInClusterBackend{
		namespace:    namespace,
		selector:     selector,
		pollInterval: poll,
		logger:       logger,
		client:       client,
		stopCh:       make(chan struct{}),
		routes:       make(map[string]ykInClusterRoute),
	}

	// Initial refresh: must succeed so the first /v1 request after
	// `serve ensure` returns serves the current state, not an
	// empty 404. Same semantics as the previous WaitForCacheSync.
	if err := b.refresh(ctx); err != nil {
		return nil, fmt.Errorf("initial poll: %w", err)
	}

	go b.run(ctx)

	logger.Info("in-cluster backend ready",
		zap.Int("port", cfg.Port),
		zap.String("namespace", namespace),
		zap.String("labelSelector", selector),
		zap.Duration("pollInterval", poll),
		zap.Int("routes", len(b.routes)),
	)
	return b, nil
}

// Close stops the polling goroutine. Safe to call multiple times.
func (b *ykInClusterBackend) Close() {
	select {
	case <-b.stopCh:
		// already closed
	default:
		close(b.stopCh)
	}
}

// run is the polling loop. Sleeps pollInterval between refreshes;
// returns on context cancel or Close. Errors are logged but don't
// stop the loop -- a transient apiserver hiccup shouldn't take the
// route table offline; readers keep serving the last known state.
func (b *ykInClusterBackend) run(ctx context.Context) {
	t := time.NewTicker(b.pollInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-b.stopCh:
			return
		case <-t.C:
		}
		if err := b.refresh(ctx); err != nil {
			b.logger.Warn("poll failed; keeping last known routes",
				zap.Error(err),
			)
		}
	}
}

// refresh issues one list and replaces the route table on success.
func (b *ykInClusterBackend) refresh(ctx context.Context) error {
	body, err := b.client.listSecrets(ctx, b.selector)
	if err != nil {
		return err
	}
	var list secretList
	if err := json.Unmarshal(body, &list); err != nil {
		return fmt.Errorf("decode SecretList: %w", err)
	}

	routes := make(map[string]ykInClusterRoute)
	for i := range list.Items {
		b.addSecretRoutes(&list.Items[i], routes)
	}

	b.mu.Lock()
	prev := len(b.routes)
	b.routes = routes
	b.mu.Unlock()

	if prev != len(routes) {
		b.logger.Info("in-cluster routes changed",
			zap.Int("before", prev),
			zap.Int("after", len(routes)),
		)
	}
	return nil
}

// secretList is the minimal subset of corev1.SecretList the route
// builder needs. Unmarshals the apiserver's standard JSON encoding
// of `kubectl get secrets -o json`.
type secretList struct {
	Items []secretItem `json:"items"`
}

type secretItem struct {
	Metadata secretMeta        `json:"metadata"`
	Data     map[string]string `json:"data,omitempty"` // base64-encoded
}

type secretMeta struct {
	Name string `json:"name"`
}

// addSecretRoutes adds every data key of a matching Secret to the
// route map. Mirrors the previous informer-driven version: ignores
// Secrets whose name doesn't start with `y-kustomize.`, skips data
// keys named ForbiddenSecretKey ("kustomization.yaml") with a warn.
func (b *ykInClusterBackend) addSecretRoutes(sec *secretItem, routes map[string]ykInClusterRoute) {
	if !strings.HasPrefix(sec.Metadata.Name, ykInClusterSecretPrefix) {
		return
	}
	suffix := strings.TrimPrefix(sec.Metadata.Name, ykInClusterSecretPrefix)
	pathBase := "/v1/" + strings.Replace(suffix, ".", "/", 1)
	for key, b64 := range sec.Data {
		if key == ForbiddenSecretKey {
			b.logger.Warn("skipping reserved data key",
				zap.String("secret", sec.Metadata.Name),
				zap.String("key", key),
				zap.String("reason", "kustomization.yaml is reserved; rename the key"),
			)
			continue
		}
		body, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			b.logger.Warn("skipping un-decodable data value",
				zap.String("secret", sec.Metadata.Name),
				zap.String("key", key),
				zap.Error(err),
			)
			continue
		}
		route := pathBase + "/" + key
		routes[route] = ykInClusterRoute{
			Path:        route,
			ContentType: DetectContentType(key),
			Body:        body,
		}
	}
}

// ServeHTTP implements http.Handler. Only `/v1/**` paths are handled;
// the parent mux routes `/health` and `/openapi.yaml` separately.
func (b *ykInClusterBackend) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		MethodNotAllowed(w, http.MethodGet, http.MethodHead)
		return
	}
	if !strings.HasPrefix(r.URL.Path, "/v1/") {
		http.NotFound(w, r)
		return
	}
	b.mu.RLock()
	route, ok := b.routes[r.URL.Path]
	b.mu.RUnlock()
	if !ok {
		http.NotFound(w, r)
		return
	}
	WriteAsset(w, r, route.Path, route.Body)
}

// Routes returns the sorted list of served paths (stable order).
func (b *ykInClusterBackend) Routes() []string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	out := make([]string, 0, len(b.routes))
	for p := range b.routes {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// RouteContentType returns the content type a route will be served
// with. Returns empty string for unknown routes.
func (b *ykInClusterBackend) RouteContentType(path string) string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.routes[path].ContentType
}

// Health returns a map of extra fields to include in /health.
// Computed on each call so it reflects the current poll state.
func (b *ykInClusterBackend) Health() map[string]any {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return map[string]any{
		"namespace":     b.namespace,
		"labelSelector": b.selector,
		"pollInterval":  b.pollInterval.String(),
		"routes":        len(b.routes),
	}
}
