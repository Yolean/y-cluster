package serve

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHealthHandler_GET(t *testing.T) {
	srv := httptest.NewServer(HealthHandler(TypeYKustomizeLocal, map[string]any{"routes": 3}))
	defer srv.Close()
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status: %d", resp.StatusCode)
	}
	if resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("cache-control: %s", resp.Header.Get("Cache-Control"))
	}
	body, _ := io.ReadAll(resp.Body)
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if got["ok"] != true {
		t.Fatalf("ok: %v", got)
	}
	if got["type"] != "y-kustomize-local" {
		t.Fatalf("type: %v", got)
	}
	if got["routes"].(float64) != 3 {
		t.Fatalf("routes: %v", got)
	}
}

func TestHealthHandler_HEAD(t *testing.T) {
	srv := httptest.NewServer(HealthHandler(TypeStatic, nil))
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodHead, srv.URL, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status: %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if len(body) != 0 {
		t.Fatalf("HEAD body: %q", body)
	}
}

func TestHealthHandler_BadMethod(t *testing.T) {
	srv := httptest.NewServer(HealthHandler(TypeStatic, nil))
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodPost, srv.URL, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status: %d", resp.StatusCode)
	}
}
