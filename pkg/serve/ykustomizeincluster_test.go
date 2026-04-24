package serve

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
)

// newTestInClusterBackend builds a backend wired to a fake clientset.
// Caller controls lifetime via ctx (cancel to stop the informer).
// The backend's initial cache sync is waited on inside the
// constructor, so returning means /v1 lookups reflect `seed`.
func newTestInClusterBackend(t *testing.T, ctx context.Context, cfg *Config, seed ...*corev1.Secret) (*ykInClusterBackend, kubernetes.Interface) {
	t.Helper()
	objs := make([]runtime.Object, 0, len(seed))
	for _, s := range seed {
		objs = append(objs, s)
	}
	cs := fake.NewClientset(objs...)
	factory := func(ic YKustomizeInClusterConfig) (kubernetes.Interface, string, error) {
		ns := ic.Namespace
		if ns == "" {
			ns = "default"
		}
		return cs, ns, nil
	}
	b, err := newYKustomizeInClusterBackendWith(ctx, cfg, newConsoleLogger(), factory)
	if err != nil {
		t.Fatal(err)
	}
	return b, cs
}

// mkSecret builds a Secret with the y-kustomize label set so the
// default selector matches it.
func mkSecret(name string, data map[string]string) *corev1.Secret {
	byteData := map[string][]byte{}
	for k, v := range data {
		byteData[k] = []byte(v)
	}
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
			Labels: map[string]string{
				"yolean.se/module-part": "y-kustomize",
			},
		},
		Data: byteData,
	}
}

// mkSecretBare builds a Secret without the default label, so the
// default selector filters it out.
func mkSecretBare(name string, data map[string]string) *corev1.Secret {
	s := mkSecret(name, data)
	s.Labels = nil
	return s
}

func cfgInCluster() *Config {
	return &Config{
		Port:      1,
		Type:      TypeYKustomizeInCluster,
		InCluster: &YKustomizeInClusterConfig{Namespace: "default"},
	}
}

// waitForRouteCount polls the backend until it reports exactly `want`
// routes, or the deadline fires. Informer events are asynchronous so
// tests that mutate secrets after backend construction have to wait.
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
		mkSecret("y-kustomize.blobs.setup-bucket-job", map[string]string{
			"base-for-annotations.yaml": "kind: Job\n",
		}),
		mkSecret("y-kustomize.kafka.setup-topic-job", map[string]string{
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
		mkSecret("unrelated-secret", map[string]string{"key": "value"}),
		mkSecret("y-kustomize.blobs.setup-bucket-job", map[string]string{
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
		mkSecretBare("y-kustomize.blobs.setup-bucket-job", map[string]string{
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
		mkSecret("y-kustomize.blobs.setup-bucket-job", map[string]string{
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
	b, cs := newTestInClusterBackend(t, ctx, cfg) // no seed

	if len(b.Routes()) != 0 {
		t.Fatalf("routes before add: %v", b.Routes())
	}

	_, err := cs.CoreV1().Secrets("default").Create(ctx,
		mkSecret("y-kustomize.blobs.setup-bucket-job", map[string]string{
			"base-for-annotations.yaml": "kind: Job\n",
		}),
		metav1.CreateOptions{},
	)
	if err != nil {
		t.Fatal(err)
	}
	waitForRouteCount(t, b, 1)
}

func TestInCluster_UpdateChangesBody(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cfg := cfgInCluster()
	b, cs := newTestInClusterBackend(t, ctx, cfg,
		mkSecret("y-kustomize.blobs.setup-bucket-job", map[string]string{
			"values.yaml": "bucket: builds\n",
		}),
	)
	srv := httptest.NewServer(b)
	defer srv.Close()

	// Update the secret
	s := mkSecret("y-kustomize.blobs.setup-bucket-job", map[string]string{
		"values.yaml": "bucket: builds-v2\n",
	})
	_, err := cs.CoreV1().Secrets("default").Update(ctx, s, metav1.UpdateOptions{})
	if err != nil {
		t.Fatal(err)
	}

	// Poll until the served body changes
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
	b, cs := newTestInClusterBackend(t, ctx, cfg,
		mkSecret("y-kustomize.blobs.setup-bucket-job", map[string]string{
			"values.yaml": "x\n",
		}),
	)
	if len(b.Routes()) != 1 {
		t.Fatalf("initial routes: %v", b.Routes())
	}

	if err := cs.CoreV1().Secrets("default").Delete(ctx, "y-kustomize.blobs.setup-bucket-job", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	waitForRouteCount(t, b, 0)
}

func TestInCluster_Health(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cfg := cfgInCluster()
	b, _ := newTestInClusterBackend(t, ctx, cfg,
		mkSecret("y-kustomize.a.b", map[string]string{"k.yaml": "x"}),
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
	// End-to-end through buildServers-style wiring: an OpenAPIHandlerFunc
	// should re-render the spec on each request so a secret added after
	// start appears in the response.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cfg := cfgInCluster()
	b, cs := newTestInClusterBackend(t, ctx, cfg)

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

	// Add a secret
	_, err := cs.CoreV1().Secrets("default").Create(ctx,
		mkSecret("y-kustomize.blobs.setup-bucket-job", map[string]string{
			"base-for-annotations.yaml": "kind: Job\n",
		}),
		metav1.CreateOptions{},
	)
	if err != nil {
		t.Fatal(err)
	}
	waitForRouteCount(t, b, 1)

	resp, _ = http.Get(srv.URL)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "/v1/blobs/setup-bucket-job/base-for-annotations.yaml") {
		t.Fatalf("spec did not adapt to watch: %q", body)
	}
}

func TestInCluster_MethodNotAllowed(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cfg := cfgInCluster()
	b, _ := newTestInClusterBackend(t, ctx, cfg,
		mkSecret("y-kustomize.a.b", map[string]string{"k.yaml": "v"}),
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
		mkSecret("y-kustomize.a.b", map[string]string{"k.yaml": "v"}),
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
		func(YKustomizeInClusterConfig) (kubernetes.Interface, string, error) {
			return fake.NewClientset(), "x", nil
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
	match := mkSecret("y-kustomize.a.b", map[string]string{"k.yaml": "v"})
	match.Labels = map[string]string{"app": "custom"}

	// Secret with the y-kustomize default label but not our custom one.
	other := mkSecret("y-kustomize.c.d", map[string]string{"k.yaml": "v"})

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
