package serve

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
)

const (
	// ykInClusterSecretPrefix is the naming convention inherited
	// from ystack: Secret name `y-kustomize.{group}.{name}`.
	ykInClusterSecretPrefix = "y-kustomize."

	// ykInClusterDefaultLabel is the Secret label selector ystack
	// applies to every y-kustomize Secret. Configurable via
	// inCluster.labelSelector.
	ykInClusterDefaultLabel = "yolean.se/module-part=y-kustomize"

	// ykInClusterResyncPeriod controls how often the informer
	// re-lists Secrets even without events. Ten minutes matches the
	// k8s.io/client-go examples and is cheap on small object counts.
	ykInClusterResyncPeriod = 10 * time.Minute
)

// ykInClusterRoute is an in-memory served path. Unlike the local
// backend which reads from disk on every request, the in-cluster
// backend keeps bodies in memory because the authoritative source is
// the informer's store, which is itself in-memory.
type ykInClusterRoute struct {
	Path        string
	ContentType string
	Body        []byte
}

// ykInClusterBackend watches Kubernetes Secrets with a matching label
// and serves each Secret's data keys at `/v1/{group}/{name}/{key}`.
type ykInClusterBackend struct {
	namespace string
	selector  string
	logger    *zap.Logger

	informer cache.SharedIndexInformer
	factory  informers.SharedInformerFactory
	stopCh   chan struct{}

	mu     sync.RWMutex
	routes map[string]ykInClusterRoute
}

// clientFactory is an injection point so tests can substitute a
// fake.Clientset without touching kubeconfig loading.
type clientFactory func(cfg YKustomizeInClusterConfig) (kubernetes.Interface, string, error)

var defaultClientFactory clientFactory = func(cfg YKustomizeInClusterConfig) (kubernetes.Interface, string, error) {
	restCfg, ns, err := loadK8sConfig(cfg)
	if err != nil {
		return nil, "", fmt.Errorf("kube config: %w", err)
	}
	cs, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return nil, "", fmt.Errorf("kube client: %w", err)
	}
	return cs, ns, nil
}

// newYKustomizeInClusterBackend builds a backend, starts its informer,
// waits for initial cache sync, and populates the initial route table.
// The informer keeps running until ctx is cancelled or Close() is called.
func newYKustomizeInClusterBackend(ctx context.Context, cfg *Config, logger *zap.Logger) (*ykInClusterBackend, error) {
	return newYKustomizeInClusterBackendWith(ctx, cfg, logger, defaultClientFactory)
}

func newYKustomizeInClusterBackendWith(ctx context.Context, cfg *Config, logger *zap.Logger, cf clientFactory) (*ykInClusterBackend, error) {
	if cfg.Type != TypeYKustomizeInCluster {
		return nil, fmt.Errorf("not a y-kustomize-in-cluster config: %s", cfg.Type)
	}
	ic := cfg.InCluster
	if ic == nil {
		return nil, fmt.Errorf("inCluster config missing after validate") // defensive
	}

	clientset, namespace, err := cf(*ic)
	if err != nil {
		return nil, err
	}

	selector := ic.LabelSelector
	if selector == "" {
		selector = ykInClusterDefaultLabel
	}

	b := &ykInClusterBackend{
		namespace: namespace,
		selector:  selector,
		logger:    logger,
		stopCh:    make(chan struct{}),
		routes:    make(map[string]ykInClusterRoute),
	}

	factory := informers.NewSharedInformerFactoryWithOptions(
		clientset,
		ykInClusterResyncPeriod,
		informers.WithNamespace(namespace),
		informers.WithTweakListOptions(func(opts *metav1.ListOptions) {
			opts.LabelSelector = selector
		}),
	)
	b.factory = factory
	b.informer = factory.Core().V1().Secrets().Informer()

	if _, err := b.informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj any) { b.onChange() },
		UpdateFunc: func(_, obj any) { b.onChange() },
		DeleteFunc: func(obj any) { b.onChange() },
	}); err != nil {
		return nil, fmt.Errorf("register handler: %w", err)
	}

	// Plumb ctx cancellation through to the informer's stopCh so a
	// SIGTERM to the daemon cleanly stops the watch goroutines.
	go func() {
		<-ctx.Done()
		b.Close()
	}()

	factory.Start(b.stopCh)

	if !cache.WaitForCacheSync(b.stopCh, b.informer.HasSynced) {
		return nil, fmt.Errorf("initial cache sync cancelled")
	}
	b.rebuild()

	logger.Info("in-cluster backend ready",
		zap.Int("port", cfg.Port),
		zap.String("namespace", namespace),
		zap.String("labelSelector", selector),
		zap.Int("routes", len(b.routes)),
	)
	return b, nil
}

// Close stops the informer. Safe to call multiple times.
func (b *ykInClusterBackend) Close() {
	select {
	case <-b.stopCh:
		// already closed
	default:
		close(b.stopCh)
	}
}

// onChange rebuilds the route table under write lock. The informer
// fires events in a single worker goroutine so we won't race with
// ourselves; readers take the read lock and see a coherent snapshot.
func (b *ykInClusterBackend) onChange() {
	b.rebuild()
}

func (b *ykInClusterBackend) rebuild() {
	routes := make(map[string]ykInClusterRoute)
	for _, obj := range b.informer.GetStore().List() {
		sec, ok := obj.(*corev1.Secret)
		if !ok {
			continue
		}
		addSecretRoutes(sec, routes)
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
}

// addSecretRoutes adds every data key of a matching Secret to the
// route map. Ignores Secrets whose name doesn't start with the
// `y-kustomize.` prefix (possible if a user applies the label to
// other Secrets). Preserves the `first dot only` behavior inherited
// from ystack's y-kustomize so renames behave identically there.
func addSecretRoutes(sec *corev1.Secret, routes map[string]ykInClusterRoute) {
	if !strings.HasPrefix(sec.Name, ykInClusterSecretPrefix) {
		return
	}
	suffix := strings.TrimPrefix(sec.Name, ykInClusterSecretPrefix)
	pathBase := "/v1/" + strings.Replace(suffix, ".", "/", 1)
	for key, val := range sec.Data {
		route := pathBase + "/" + key
		body := make([]byte, len(val))
		copy(body, val)
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

// Routes returns the sorted list of served paths (stable order). The
// openapi handler queries this on every request so the spec reflects
// the current watch state, per SERVE_FEATURE.md ("the openapi spec
// adapts to the watch").
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

// Health returns a map of extra fields to include in /health alongside
// the standard ok/type fields. Computed on each call so it reflects
// the current watch state.
func (b *ykInClusterBackend) Health() map[string]any {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return map[string]any{
		"namespace":     b.namespace,
		"labelSelector": b.selector,
		"routes":        len(b.routes),
	}
}
