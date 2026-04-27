package cache

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRoot_FlagBeatsEverything(t *testing.T) {
	t.Setenv("Y_CLUSTER_CACHE_DIR", "/tmp/env-cache")
	t.Setenv("XDG_CACHE_HOME", "/tmp/xdg-cache")
	got, err := Root("/explicit/flag")
	if err != nil {
		t.Fatal(err)
	}
	if got != "/explicit/flag" {
		t.Fatalf("got %q", got)
	}
}

func TestRoot_EnvBeatsXDG(t *testing.T) {
	t.Setenv("Y_CLUSTER_CACHE_DIR", "/tmp/env-cache")
	t.Setenv("XDG_CACHE_HOME", "/tmp/xdg-cache")
	got, err := Root("")
	if err != nil {
		t.Fatal(err)
	}
	if got != "/tmp/env-cache" {
		t.Fatalf("got %q", got)
	}
}

func TestRoot_XDGBeatsHomeFallback(t *testing.T) {
	t.Setenv("Y_CLUSTER_CACHE_DIR", "")
	t.Setenv("XDG_CACHE_HOME", "/tmp/xdg-cache")
	got, err := Root("")
	if err != nil {
		t.Fatal(err)
	}
	if got != "/tmp/xdg-cache/y-cluster" {
		t.Fatalf("got %q", got)
	}
}

func TestRoot_HomeFallback(t *testing.T) {
	t.Setenv("Y_CLUSTER_CACHE_DIR", "")
	t.Setenv("XDG_CACHE_HOME", "")
	got, err := Root("")
	if err != nil {
		t.Fatal(err)
	}
	home, _ := os.UserHomeDir()
	want := filepath.Join(home, ".cache", "y-cluster")
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestRoot_FlagIsAbsolutized(t *testing.T) {
	t.Setenv("Y_CLUSTER_CACHE_DIR", "")
	got, err := Root("relative/path")
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(got) {
		t.Fatalf("expected absolute, got %q", got)
	}
	if !strings.HasSuffix(got, "relative/path") {
		t.Fatalf("got %q does not end with the input", got)
	}
}

func TestImages_AndK3sShareRoot(t *testing.T) {
	t.Setenv("Y_CLUSTER_CACHE_DIR", "/tmp/y-cache")
	imgs, err := Images("")
	if err != nil {
		t.Fatal(err)
	}
	k3s, err := K3s("")
	if err != nil {
		t.Fatal(err)
	}
	if imgs != "/tmp/y-cache/images" {
		t.Fatalf("images: %q", imgs)
	}
	if k3s != "/tmp/y-cache/k3s" {
		t.Fatalf("k3s: %q", k3s)
	}
}

func TestEnvoyGateway_VersionLayout(t *testing.T) {
	t.Setenv("Y_CLUSTER_CACHE_DIR", "/tmp/y-cache")
	root, err := EnvoyGateway("")
	if err != nil {
		t.Fatal(err)
	}
	if root != "/tmp/y-cache/envoygateway" {
		t.Fatalf("envoygateway root: %q", root)
	}
	versioned, err := EnvoyGatewayVersion("", "v1.7.2")
	if err != nil {
		t.Fatal(err)
	}
	if versioned != "/tmp/y-cache/envoygateway/v1.7.2" {
		t.Fatalf("versioned: %q", versioned)
	}
}

func TestEnvoyGatewayVersion_RejectsEmptyVersion(t *testing.T) {
	if _, err := EnvoyGatewayVersion("", ""); err == nil {
		t.Fatal("want error for empty version")
	}
}
