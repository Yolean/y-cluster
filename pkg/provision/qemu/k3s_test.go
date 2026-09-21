package qemu

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestURLEncodeK3sVersion(t *testing.T) {
	cases := map[string]string{
		"v1.35.4-rc3-k3s1": "v1.35.4-rc3-k3s1",
		"v1.35.3+k3s1":     "v1.35.3%2Bk3s1",
		"plain":            "plain",
	}
	for in, want := range cases {
		if got := urlEncodeK3sVersion(in); got != want {
			t.Errorf("urlEncodeK3sVersion(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestShellQuote(t *testing.T) {
	cases := map[string]string{
		"plain":                       "'plain'",
		"with sp":                     "'with sp'",
		"a'b":                         `'a'\''b'`,
		"--write-kubeconfig-mode=644": "'--write-kubeconfig-mode=644'",
	}
	for in, want := range cases {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDownloadFile_Atomic(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/missing") {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte("payload\n"))
	}))
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "out.bin")
	if err := downloadFile(context.Background(), srv.URL+"/ok", dest); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "payload\n" {
		t.Fatalf("body: %q", got)
	}
	if _, err := os.Stat(dest + ".tmp"); !os.IsNotExist(err) {
		t.Fatal(".tmp file leaked after successful download")
	}
}

func TestDownloadFile_Non200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "out.bin")
	err := downloadFile(context.Background(), srv.URL+"/x", dest)
	if err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("want 404 error, got %v", err)
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Fatal("dest file should not exist after failed download")
	}
}
