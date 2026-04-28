package serve

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeSecretStore mocks the apiserver's GET /api/v1/.../secrets
// endpoint. The backend talks to it via secretLister; tests
// mutate the in-memory secret map (Set / Delete) and the next
// poll picks up the change. testPoll keeps poll latency bounded
// at ~50ms so the existing wait helpers (deadline ~2s) still
// work.
const testPoll = 50 * time.Millisecond

type fakeSecret struct {
	name   string
	labels map[string]string
	data   map[string]string // string values; the lister base64-encodes
}

type fakeSecretStore struct {
	mu      sync.RWMutex
	secrets map[string]fakeSecret
}

func newFakeSecretStore(seed ...fakeSecret) *fakeSecretStore {
	s := &fakeSecretStore{secrets: map[string]fakeSecret{}}
	for _, sec := range seed {
		s.secrets[sec.name] = sec
	}
	return s
}

func (s *fakeSecretStore) Set(sec fakeSecret) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.secrets[sec.name] = sec
}

func (s *fakeSecretStore) Delete(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.secrets, name)
}

// listSecrets implements secretLister against the in-memory store.
// Filters by labelSelector in the same `key=value,key2=value2` form
// the apiserver accepts (only the equality cases this test corpus
// uses; we don't reimplement the full label selector grammar).
func (s *fakeSecretStore) listSecrets(_ context.Context, labelSelector string) ([]byte, error) {
	wants := parseEqualityLabelSelector(labelSelector)
	s.mu.RLock()
	defer s.mu.RUnlock()
	list := secretList{}
	for _, sec := range s.secrets {
		if !matchesLabels(sec.labels, wants) {
			continue
		}
		encoded := map[string]string{}
		for k, v := range sec.data {
			encoded[k] = base64.StdEncoding.EncodeToString([]byte(v))
		}
		list.Items = append(list.Items, secretItem{
			Metadata: secretMeta{Name: sec.name},
			Data:     encoded,
		})
	}
	return json.Marshal(list)
}

func parseEqualityLabelSelector(s string) map[string]string {
	out := map[string]string{}
	if s == "" {
		return out
	}
	for _, part := range strings.Split(s, ",") {
		kv := strings.SplitN(part, "=", 2)
		if len(kv) != 2 {
			continue
		}
		out[kv[0]] = kv[1]
	}
	return out
}

func matchesLabels(have, want map[string]string) bool {
	for k, v := range want {
		if have[k] != v {
			return false
		}
	}
	return true
}

// newTestInClusterBackend builds a backend whose secretLister is
// a fakeSecretStore. The store is returned so tests can mutate
// it (Set / Delete) post-construction. testPoll keeps the
// polling loop fast so wait helpers don't time out.
func newTestInClusterBackend(t *testing.T, ctx context.Context, cfg *Config, seed ...fakeSecret) (*ykInClusterBackend, *fakeSecretStore) {
	t.Helper()
	store := newFakeSecretStore(seed...)
	if cfg.InCluster == nil {
		cfg.InCluster = &YKustomizeInClusterConfig{}
	}
	if cfg.InCluster.PollInterval <= 0 {
		cfg.InCluster.PollInterval = testPoll
	}
	factory := func(ic YKustomizeInClusterConfig) (secretLister, string, error) {
		ns := ic.Namespace
		if ns == "" {
			ns = "default"
		}
		return store, ns, nil
	}
	b, err := newYKustomizeInClusterBackendWith(ctx, cfg, newConsoleLogger(), factory)
	if err != nil {
		t.Fatal(err)
	}
	return b, store
}

// secretWithDefault builds a fakeSecret with the y-kustomize label
// set so the default selector matches it.
func secretWithDefault(name string, data map[string]string) fakeSecret {
	return fakeSecret{
		name:   name,
		labels: map[string]string{"yolean.se/module-part": "y-kustomize"},
		data:   data,
	}
}

// secretBare builds a fakeSecret without the default label, so the
// default selector filters it out.
func secretBare(name string, data map[string]string) fakeSecret {
	return fakeSecret{name: name, data: data}
}

func cfgInCluster() *Config {
	return &Config{
		Port:      1,
		Type:      TypeYKustomizeInCluster,
		InCluster: &YKustomizeInClusterConfig{Namespace: "default"},
	}
}

// waitForRouteCount polls the backend until it reports exactly `want`
// routes, or the deadline fires. Polls are asynchronous so tests
// that mutate secrets after backend construction have to wait.
func waitForRouteCount(t *testing.T, b *ykInClusterBackend, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(b.Routes()) == want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("route count: got %d, want %d (routes=%v)", len(b.Routes()), want, b.Routes())
}

func TestInCluster_InitialSync(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cfg := cfgInCluster()
	b, _ := newTestInClusterBackend(t, ctx, cfg,
		secretWithDefault("y-kustomize.blobs.setup-bucket-job", map[string]string{
			"base-for-annotations.yaml": "kind: Job\n",
		}),
		secretWithDefault("y-kustomize.kafka.setup-topic-job", map[string]string{
			"base-for-annotations.yaml": "kind: Job\n",
		}),
	)

	got := b.Routes()
	want := []string{
		"/v1/blobs/setup-bucket-job/base-for-annotations.yaml",
		"/v1/kafka/setup-topic-job/base-for-annotations.yaml",
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("routes: got %v, want %v", got, want)
	}
}

func TestInCluster_SecretWithoutPrefixIgnored(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cfg := cfgInCluster()
	b, _ := newTestInClusterBackend(t, ctx, cfg,
		// Has the label but wrong name prefix.
		secretWithDefault("unrelated-secret", map[string]string{"key": "value"}),
		secretWithDefault("y-kustomize.blobs.setup-bucket-job", map[string]string{
			"base-for-annotations.yaml": "kind: Job\n",
		}),
	)
	if len(b.Routes()) != 1 {
		t.Fatalf("routes: %v (unrelated secret leaked)", b.Routes())
	}
}

func TestInCluster_LabelSelectorFilters(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cfg := cfgInCluster()
	b, _ := newTestInClusterBackend(t, ctx, cfg,
		// Would match the name pattern but lacks the label.
		secretBare("y-kustomize.blobs.setup-bucket-job", map[string]string{
			"base-for-annotations.yaml": "kind: Job\n",
		}),
	)
	if len(b.Routes()) != 0 {
		t.Fatalf("unlabeled secret leaked into routes: %v", b.Routes())
	}
}

func TestInCluster_Serve200AndCached304(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cfg := cfgInCluster()
	b, _ := newTestInClusterBackend(t, ctx, cfg,
		secretWithDefault("y-kustomize.blobs.setup-bucket-job", map[string]string{
			"values.yaml": "bucket: builds\n",
		}),
	)
	srv := httptest.NewServer(b)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v1/blobs/setup-bucket-job/values.yaml")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "bucket: builds\n" {
		t.Fatalf("body: %q", body)
	}
	if resp.Header.Get("Content-Type") != yamlMIME {
		t.Fatalf("content-type: %s", resp.Header.Get("Content-Type"))
	}
	etag := resp.Header.Get("ETag")

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/blobs/setup-bucket-job/values.yaml", nil)
	req.Header.Set("If-None-Match", etag)
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusNotModified {
		t.Fatalf("conditional: %d", resp2.StatusCode)
	}
}

func TestInCluster_AddAfterStart(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cfg := cfgInCluster()
	b, store := newTestInClusterBackend(t, ctx, cfg) // no seed

	if len(b.Routes()) != 0 {
		t.Fatalf("routes before add: %v", b.Routes())
	}

	store.Set(secretWithDefault("y-kustomize.blobs.setup-bucket-job", map[string]string{
		"base-for-annotations.yaml": "kind: Job\n",
	}))
	waitForRouteCount(t, b, 1)
}

func TestInCluster_UpdateChangesBody(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cfg := cfgInCluster()
	b, store := newTestInClusterBackend(t, ctx, cfg,
		secretWithDefault("y-kustomize.blobs.setup-bucket-job", map[string]string{
			"values.yaml": "bucket: builds\n",
		}),
	)
	srv := httptest.NewServer(b)
	defer srv.Close()

	store.Set(secretWithDefault("y-kustomize.blobs.setup-bucket-job", map[string]string{
		"values.yaml": "bucket: builds-v2\n",
	}))

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(srv.URL + "/v1/blobs/setup-bucket-job/values.yaml")
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if strings.Contains(string(body), "builds-v2") {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("update never propagated to served body")
}

func TestInCluster_DeleteRemovesRoute(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cfg := cfgInCluster()
	b, store := newTestInClusterBackend(t, ctx, cfg,
		secretWithDefault("y-kustomize.blobs.setup-bucket-job", map[string]string{
			"values.yaml": "x\n",
		}),
	)
	if len(b.Routes()) != 1 {
		t.Fatalf("initial routes: %v", b.Routes())
	}

	store.Delete("y-kustomize.blobs.setup-bucket-job")
	waitForRouteCount(t, b, 0)
}

func TestInCluster_Health(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cfg := cfgInCluster()
	b, _ := newTestInClusterBackend(t, ctx, cfg,
		secretWithDefault("y-kustomize.a.b", map[string]string{"k.yaml": "x"}),
	)
	h := b.Health()
	if h["namespace"] != "default" {
		t.Fatalf("namespace: %v", h["namespace"])
	}
	if h["routes"] != 1 {
		t.Fatalf("routes count: %v", h["routes"])
	}
}

func TestInCluster_OpenAPIReflectsWatch(t *testing.T) {
	// End-to-end through buildServers-style wiring: the openapi
	// handler should re-render the spec on each request so a
	// secret added after start appears in the response.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cfg := cfgInCluster()
	b, store := newTestInClusterBackend(t, ctx, cfg)

	handler := OpenAPIHandlerFunc(func() []byte {
		routes := buildYKRoutesSpec(b.Routes(), b.RouteContentType)
		return newOpenAPISpec("test", TypeYKustomizeInCluster, "dev", routes).Render()
	})
	srv := httptest.NewServer(handler)
	defer srv.Close()

	// Before any secret
	resp, _ := http.Get(srv.URL)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if strings.Contains(string(body), "/v1/") {
		t.Fatalf("empty spec should not list any /v1 path: %q", body)
	}

	store.Set(secretWithDefault("y-kustomize.blobs.setup-bucket-job", map[string]string{
		"base-for-annotations.yaml": "kind: Job\n",
	}))
	waitForRouteCount(t, b, 1)

	resp, _ = http.Get(srv.URL)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "/v1/blobs/setup-bucket-job/base-for-annotations.yaml") {
		t.Fatalf("spec did not adapt to poll: %q", body)
	}
}

func TestInCluster_MethodNotAllowed(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cfg := cfgInCluster()
	b, _ := newTestInClusterBackend(t, ctx, cfg,
		secretWithDefault("y-kustomize.a.b", map[string]string{"k.yaml": "v"}),
	)
	srv := httptest.NewServer(b)
	defer srv.Close()
	resp, _ := http.Post(srv.URL+"/v1/a/b/k.yaml", "text/plain", strings.NewReader(""))
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST: %d", resp.StatusCode)
	}
}

func TestInCluster_NotFoundOutsideV1(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cfg := cfgInCluster()
	b, _ := newTestInClusterBackend(t, ctx, cfg,
		secretWithDefault("y-kustomize.a.b", map[string]string{"k.yaml": "v"}),
	)
	srv := httptest.NewServer(b)
	defer srv.Close()

	resp, _ := http.Get(srv.URL + "/elsewhere")
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("outside /v1/: %d", resp.StatusCode)
	}

	resp, _ = http.Get(srv.URL + "/v1/missing")
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("unknown /v1 path: %d", resp.StatusCode)
	}
}

func TestInCluster_CloseIsIdempotent(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cfg := cfgInCluster()
	b, _ := newTestInClusterBackend(t, ctx, cfg)
	b.Close()
	b.Close() // must not panic
}

func TestInCluster_WrongType(t *testing.T) {
	cfg := &Config{Type: TypeStatic, InCluster: &YKustomizeInClusterConfig{}}
	_, err := newYKustomizeInClusterBackendWith(context.Background(), cfg, newConsoleLogger(),
		func(YKustomizeInClusterConfig) (secretLister, string, error) {
			return newFakeSecretStore(), "x", nil
		})
	if err == nil {
		t.Fatal("want error for wrong type")
	}
}

func TestInCluster_CustomSelector(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cfg := cfgInCluster()
	cfg.InCluster.LabelSelector = "app=custom"

	// Secret with our custom label and y-kustomize name prefix.
	match := secretWithDefault("y-kustomize.a.b", map[string]string{"k.yaml": "v"})
	match.labels = map[string]string{"app": "custom"}

	// Secret with the y-kustomize default label but not our custom one.
	other := secretWithDefault("y-kustomize.c.d", map[string]string{"k.yaml": "v"})

	b, _ := newTestInClusterBackend(t, ctx, cfg, match, other)
	if len(b.Routes()) != 1 {
		t.Fatalf("custom selector: %v", b.Routes())
	}
	if b.Routes()[0] != "/v1/a/b/k.yaml" {
		t.Fatalf("wrong match: %v", b.Routes())
	}
}

func TestInCluster_HealthHandlerDynamic(t *testing.T) {
	// Verify the HealthHandlerFunc in health.go re-reads the
	// provider on each request, which is what the in-cluster backend
	// relies on.
	calls := 0
	handler := HealthHandlerFunc(TypeYKustomizeInCluster, func() map[string]any {
		calls++
		return map[string]any{"routes": calls}
	})
	srv := httptest.NewServer(handler)
	defer srv.Close()

	for i := 1; i <= 3; i++ {
		resp, _ := http.Get(srv.URL)
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		var payload map[string]any
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Fatal(err)
		}
		if payload["routes"].(float64) != float64(i) {
			t.Fatalf("call %d reported routes=%v", i, payload["routes"])
		}
	}
}

// TestNewK8sClient_KubeconfigParse covers the hand-rolled
// kubeconfig parser (kubeclient.go) for the auth shape kwok-style
// tests use: HTTP server, no TLS, no bearer. The parser must
// pull server URL + namespace out of the named context.
//
// Verifies the new HTTPS-polling path's foundation works against
// the same kubeconfig shape e2e/cluster/kwok.go produces.
func TestNewK8sClient_KubeconfigParse(t *testing.T) {
	dir := t.TempDir()
	cfgPath := dir + "/kubeconfig"
	body := []byte(`apiVersion: v1
kind: Config
clusters:
- name: kctx
  cluster:
    server: http://127.0.0.1:9999
contexts:
- name: kctx
  context:
    cluster: kctx
    user: kctx
    namespace: from-kubeconfig
current-context: kctx
users:
- name: kctx
  user: {}
`)
	if err := os.WriteFile(cfgPath, body, 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := newK8sClient(YKustomizeInClusterConfig{Kubeconfig: cfgPath})
	if err != nil {
		t.Fatal(err)
	}
	if c.server != "http://127.0.0.1:9999" {
		t.Fatalf("server: %q", c.server)
	}
	if c.namespace != "from-kubeconfig" {
		t.Fatalf("namespace: %q", c.namespace)
	}
	if c.bearer != "" {
		t.Fatalf("bearer should be empty for kwok-shape kubeconfig: %q", c.bearer)
	}
}

// TestK8sClient_ListSecrets exercises the actual GET against an
// httptest server emulating the apiserver, including the
// label-selector encoding.
func TestK8sClient_ListSecrets(t *testing.T) {
	var seenPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.Path + "?" + r.URL.RawQuery
		_, _ = w.Write([]byte(`{"items":[]}`))
	}))
	defer srv.Close()

	c := &k8sClient{
		server:     srv.URL,
		httpClient: srv.Client(),
		namespace:  "default",
	}
	if _, err := c.listSecrets(context.Background(), "yolean.se/module-part=y-kustomize"); err != nil {
		t.Fatal(err)
	}
	wantPath := "/api/v1/namespaces/default/secrets?labelSelector="
	if !strings.Contains(seenPath, wantPath) {
		t.Fatalf("path: %q does not contain %q", seenPath, wantPath)
	}
	if !strings.Contains(seenPath, url.QueryEscape("yolean.se/module-part=y-kustomize")) {
		t.Fatalf("label selector not URL-encoded: %q", seenPath)
	}
}

