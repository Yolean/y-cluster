package dockerexec

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/moby/moby/client"
)

// fakeDaemon answers every request with 200, except DELETE, which gets
// deleteStatus, and records the DELETE requests it saw.
func fakeDaemon(t *testing.T, deleteStatus int) (*client.Client, func() []*http.Request) {
	t.Helper()
	var mu sync.Mutex
	var deletes []*http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Api-Version", "1.47")
		if r.Method == http.MethodDelete {
			mu.Lock()
			deletes = append(deletes, r)
			mu.Unlock()
			if deleteStatus != http.StatusOK {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(deleteStatus)
				_, _ = w.Write([]byte(`{"message":"No such container: y-cluster"}`))
				return
			}
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	cli, err := client.New(client.WithHost("tcp://"+strings.TrimPrefix(srv.URL, "http://")), client.WithAPIVersion("1.47"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cli.Close() })
	return cli, func() []*http.Request {
		mu.Lock()
		defer mu.Unlock()
		return deletes
	}
}

// TestRemove_RemovesAnonymousVolumes: Remove asks the daemon to drop
// the container's anonymous volumes (v=1) as well as the container,
// forcefully (force=1). Without v=1 every k3s container left its
// image-declared VOLUMEs, 0.6 to 1 GB per cluster, on the daemon.
func TestRemove_RemovesAnonymousVolumes(t *testing.T) {
	cli, deletes := fakeDaemon(t, http.StatusOK)
	if err := Remove(context.Background(), cli, "y-cluster"); err != nil {
		t.Fatal(err)
	}
	got := deletes()
	if len(got) != 1 {
		t.Fatalf("want one DELETE, got %d", len(got))
	}
	r := got[0]
	if !strings.HasSuffix(r.URL.Path, "/containers/y-cluster") {
		t.Errorf("path %q", r.URL.Path)
	}
	q := r.URL.Query()
	if q.Get("v") != "1" {
		t.Errorf("v=%q, want 1 (remove anonymous volumes); query %q", q.Get("v"), r.URL.RawQuery)
	}
	if q.Get("force") != "1" {
		t.Errorf("force=%q, want 1; query %q", q.Get("force"), r.URL.RawQuery)
	}
}

// TestRemove_NotFoundIsSuccess: removing a container that is gone is
// a no-op, which teardown of an already torn-down cluster relies on.
func TestRemove_NotFoundIsSuccess(t *testing.T) {
	cli, _ := fakeDaemon(t, http.StatusNotFound)
	if err := Remove(context.Background(), cli, "y-cluster"); err != nil {
		t.Fatalf("NotFound should be success: %v", err)
	}
}
