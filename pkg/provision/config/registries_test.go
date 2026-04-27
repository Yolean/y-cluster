package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Yolean/y-cluster/pkg/configfile"
)

func TestRegistries_Empty(t *testing.T) {
	if !(Registries{}).Empty() {
		t.Fatal("zero-value Registries should be Empty")
	}
	r := Registries{Mirrors: map[string]RegistryMirror{"x": {}}}
	if r.Empty() {
		t.Fatal("non-empty mirrors should not be Empty")
	}
}

// TestLoad_RegistriesEnvSubst is the integration that motivates
// the whole envsubst groundwork: an operator's
// y-cluster-provision.yaml carries ${GCP_ACCESS_TOKEN} in
// registries.configs.<host>.auth.password, and Load resolves it
// from the environment without leaking the literal ${...} into
// the rendered registries.yaml downstream provisioners write.
func TestLoad_RegistriesEnvSubst(t *testing.T) {
	t.Setenv("Y_CLUSTER_TEST_GCP_TOKEN", "ya29.access-token-value")

	yamlBody := `provider: docker
registries:
  mirrors:
    europe-docker.pkg.dev:
      endpoint:
      - https://europe-docker.pkg.dev
  configs:
    europe-docker.pkg.dev:
      auth:
        username: oauth2accesstoken
        password: ${Y_CLUSTER_TEST_GCP_TOKEN}
`
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "y-cluster-provision.yaml"), []byte(yamlBody), 0o644); err != nil {
		t.Fatal(err)
	}
	var c DockerConfig
	if err := configfile.Load(dir, "y-cluster-provision.yaml", &c); err != nil {
		t.Fatal(err)
	}
	got := c.Registries.Configs["europe-docker.pkg.dev"]
	if got.Auth == nil {
		t.Fatal("auth should not be nil")
	}
	if got.Auth.Password != "ya29.access-token-value" {
		t.Fatalf("password not expanded: %q", got.Auth.Password)
	}
	if got.Auth.Username != "oauth2accesstoken" {
		t.Fatalf("plain username should pass through: %q", got.Auth.Username)
	}
}

// TestLoad_RegistriesUntaggedRefRejected covers the forward-compat
// guarantee: only fields the schema has tagged accept ${...}.
// rewrite values are intentionally untagged (regex patterns); a
// user trying to env-substitute one fails loud.
func TestLoad_RegistriesUntaggedRefRejected(t *testing.T) {
	yamlBody := `provider: docker
registries:
  mirrors:
    europe-docker.pkg.dev:
      rewrite:
        '^${PREFIX}/(.*)': 'rewrite/$1'
`
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "y-cluster-provision.yaml"), []byte(yamlBody), 0o644); err != nil {
		t.Fatal(err)
	}
	var c DockerConfig
	err := configfile.Load(dir, "y-cluster-provision.yaml", &c)
	if err == nil {
		t.Fatal("want policy error: rewrite values are not envsubst-tagged")
	}
	if !strings.Contains(err.Error(), "rewrite") && !strings.Contains(err.Error(), "envsubst") {
		t.Fatalf("error should hint at the tag fix or the path: %v", err)
	}
}

// TestLoad_RegistriesUndefinedTokenFailsLoud locks in the
// security-sensitive guarantee: a missing credential variable
// errors at load time rather than silently leaving the config
// without auth.
func TestLoad_RegistriesUndefinedTokenFailsLoud(t *testing.T) {
	// Deliberately do NOT set the env var.
	yamlBody := `provider: docker
registries:
  configs:
    europe-docker.pkg.dev:
      auth:
        username: oauth2accesstoken
        password: ${Y_CLUSTER_TEST_DEFINITELY_UNSET_TOKEN}
`
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "y-cluster-provision.yaml"), []byte(yamlBody), 0o644); err != nil {
		t.Fatal(err)
	}
	var c DockerConfig
	err := configfile.Load(dir, "y-cluster-provision.yaml", &c)
	if err == nil || !strings.Contains(err.Error(), "Y_CLUSTER_TEST_DEFINITELY_UNSET_TOKEN") {
		t.Fatalf("want undefined-var error naming the var, got %v", err)
	}
}
