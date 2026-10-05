package qemu

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// The provider's document ends inside write_files; a guest's files
// must continue that list and runcmd must parse as its own key, with
// content of any shape surviving byte for byte.
func TestRenderGuestUserData_JoinsProviderDocument(t *testing.T) {
	tricky := "  leading spaces\n---\nkey: value # not a comment\n\n\ttab\n-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n"
	spec := GuestSpec{
		Files: []GuestFile{
			{Path: "/etc/a.pem", Permissions: "0600", Content: tricky},
			{Path: "/usr/local/sbin/setup", Permissions: "0755", Content: "#!/bin/sh\nset -eu\necho 'quoted \"twice\"'\n"},
			{Path: "/etc/no-trailing-newline", Permissions: "0644", Content: "x"},
		},
		RunCmd: []string{"/usr/local/sbin/setup", "echo done"},
	}
	extra, err := renderGuestUserData(spec)
	if err != nil {
		t.Fatal(err)
	}
	for _, staticNetwork := range []bool{false, true} {
		doc := renderCloudInitUserData("dockerhost", "ssh-ed25519 KEY u@h\n", false, staticNetwork) + extra
		if !strings.HasPrefix(doc, "#cloud-config\n") {
			t.Fatal("the joined document must still be a cloud-config")
		}
		var parsed struct {
			Hostname   string `json:"hostname"`
			WriteFiles []struct {
				Path        string `json:"path"`
				Permissions string `json:"permissions"`
				Content     string `json:"content"`
			} `json:"write_files"`
			RunCmd []string `json:"runcmd"`
		}
		if err := yaml.UnmarshalStrict([]byte(doc), &parsed); err != nil {
			// UnmarshalStrict also refuses keys the struct lacks; the
			// provider's other keys are expected, so fall back.
			if err := yaml.Unmarshal([]byte(doc), &parsed); err != nil {
				t.Fatalf("joined document does not parse: %v\n%s", err, doc)
			}
		}
		if parsed.Hostname != "dockerhost" {
			t.Errorf("hostname %q", parsed.Hostname)
		}
		var paths []string
		for _, f := range parsed.WriteFiles {
			paths = append(paths, f.Path)
		}
		wantFirst := "/etc/cloud/cloud.cfg.d/99-y-cluster-pin.cfg"
		if len(paths) == 0 || paths[0] != wantFirst {
			t.Fatalf("the provider's own files come first, got %v", paths)
		}
		n := len(parsed.WriteFiles)
		got := parsed.WriteFiles[n-len(spec.Files):]
		for i, f := range spec.Files {
			if got[i].Path != f.Path || got[i].Permissions != f.Permissions || got[i].Content != f.Content {
				t.Errorf("file %d round-tripped as %+v, want %+v", i, got[i], f)
			}
		}
		if strings.Join(parsed.RunCmd, "|") != strings.Join(spec.RunCmd, "|") {
			t.Errorf("runcmd %v", parsed.RunCmd)
		}
	}
}

func TestRenderGuestUserData_Empty(t *testing.T) {
	extra, err := renderGuestUserData(GuestSpec{})
	if err != nil || extra != "" {
		t.Fatalf("an empty spec adds nothing, got %q, %v", extra, err)
	}
}

func TestProvisionGuest_RefusesClusterFields(t *testing.T) {
	cfg := Config{Name: "g", CacheDir: t.TempDir(), Context: "local"}
	if _, err := ProvisionGuest(context.Background(), cfg, GuestSpec{CloudImage: "/x.img"}, nil); err == nil || !strings.Contains(err.Error(), "no kubeconfig context") {
		t.Fatalf("a guest with a kubeconfig context must be refused, got %v", err)
	}
	cfg.Context = ""
	if _, err := ProvisionGuest(context.Background(), cfg, GuestSpec{}, nil); err == nil || !strings.Contains(err.Error(), "no cloud image") {
		t.Fatalf("a guest without a cloud image must be refused, got %v", err)
	}
}

func TestRemoveGuestUserData(t *testing.T) {
	dir := t.TempDir()
	keep := []string{"g.qcow2", "g-ssh", "g-console.log", "g.json"}
	gone := []string{"g-cloud-init.yaml", "g-seed.img"}
	for _, f := range append(append([]string{}, keep...), gone...) {
		if err := os.WriteFile(filepath.Join(dir, f), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := RemoveGuestUserData(dir, "g"); err != nil {
		t.Fatal(err)
	}
	if err := RemoveGuestUserData(dir, "g"); err != nil {
		t.Fatalf("a second removal is a no-op, got %v", err)
	}
	for _, f := range keep {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Errorf("%s must stay: %v", f, err)
		}
	}
	for _, f := range gone {
		if _, err := os.Stat(filepath.Join(dir, f)); !os.IsNotExist(err) {
			t.Errorf("%s must be removed", f)
		}
	}
}

// A guest has no context, so its teardown must leave every kubeconfig
// alone, including the one loadState picks up from $KUBECONFIG and a
// cluster entry that happens to share the guest's name.
func TestTeardownConfig_GuestLeavesKubeconfigAlone(t *testing.T) {
	cfg := defaultedRuntimeConfig(t)
	cfg.CacheDir = t.TempDir()
	path := seedKubeconfig(t, cfg)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("KUBECONFIG", path)
	cfg.Kubeconfig = path
	cfg.Context = ""
	if err := saveState(cfg); err != nil {
		t.Fatal(err)
	}
	loaded, err := GuestConfig(cfg.CacheDir, cfg.Name)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Kubeconfig != "" {
		t.Errorf("GuestConfig must not carry $KUBECONFIG, got %q", loaded.Kubeconfig)
	}
	if err := TeardownConfig(cfg, false, nil); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatalf("guest teardown modified the kubeconfig:\n%s", after)
	}
}

func TestGuestPID_NotRunning(t *testing.T) {
	if _, ok := GuestPID(t.TempDir(), "g"); ok {
		t.Fatal("no pidfile means not running")
	}
}
