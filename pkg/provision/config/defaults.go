package config

import (
	_ "embed"
	"reflect"
	"strings"

	"sigs.k8s.io/yaml"
)

// k3sYAML is the byte content of pkg/provision/config/k3s.yaml,
// embedded so the binary can resolve k3s defaults without filesystem
// access. The mirror workflow and schemagen read the same file from
// disk; runtime reads the embedded snapshot.
//
//go:embed k3s.yaml
var k3sYAML []byte

// pinFile is the parsed shape of pkg/provision/config/k3s.yaml. The
// `version` field is the single edit point; mirror.upstream/target
// describe the rancher/k3s mirror but the tag itself is derived
// from `version` (with `+` -> `-` for Docker compatibility).
type pinFile struct {
	Version string `yaml:"version"`
	Mirror  struct {
		Upstream string `yaml:"upstream"`
		Target   string `yaml:"target"`
	} `yaml:"mirror"`
}

// k3sPin parses the embedded pin file once at package init.
var k3sPin = func() pinFile {
	var p pinFile
	_ = yaml.Unmarshal(k3sYAML, &p)
	return p
}()

// K3sDefaultVersion returns the k3s release version pinned in
// pkg/provision/config/k3s.yaml. Used as the default for
// CommonConfig.K3s.Version when the user leaves it blank.
func K3sDefaultVersion() string {
	return k3sPin.Version
}

// K3sMirrorTarget returns the y-cluster mirror repository (without
// a tag) from the pin file, e.g. `ghcr.io/yolean/k3s`.
func K3sMirrorTarget() string { return k3sPin.Mirror.Target }

// K3sUpstreamRepo returns the upstream rancher/k3s repository from
// the pin file, e.g. `docker.io/rancher/k3s`.
func K3sUpstreamRepo() string { return k3sPin.Mirror.Upstream }

// MirrorImage returns the y-cluster mirror image reference for the
// given k3s release version: `<mirror.target>:<docker-tag-form>`.
// The docker provisioner prefers this image but falls back to the
// upstream when the mirror lacks a manifest for the version (see
// pkg/provision/docker/image.go).
func MirrorImage(version string) string {
	if k3sPin.Mirror.Target == "" || version == "" {
		return ""
	}
	return k3sPin.Mirror.Target + ":" + dockerTag(version)
}

// UpstreamImage returns the upstream rancher/k3s image reference
// for the given k3s release version. The docker provisioner uses
// this as a fallback when MirrorImage's manifest doesn't yet
// exist, so a freshly released k3s version can be exercised
// before the mirror workflow has run.
func UpstreamImage(version string) string {
	if k3sPin.Mirror.Upstream == "" || version == "" {
		return ""
	}
	return k3sPin.Mirror.Upstream + ":" + dockerTag(version)
}

// dockerTag converts a GitHub-release-form k3s version to its
// Docker-tag-form equivalent by replacing the `+` separator with `-`.
// `+` is build-metadata in semver but Docker tag syntax forbids it,
// so rancher/k3s on Docker Hub stores e.g. v1.35.4-rc3+k3s1 as
// v1.35.4-rc3-k3s1. The same conversion is duplicated by
// .github/workflows/mirror-k3s.yaml (one line of `tr '+' '-'`); if
// the algorithm ever changes, both sides must update.
func dockerTag(v string) string {
	return strings.ReplaceAll(v, "+", "-")
}

// DockerTag is the public form of dockerTag.
func DockerTag(v string) string { return dockerTag(v) }

// applyTagDefaults reflects over a struct and fills any zero-valued
// string field whose `jsonschema:"default=..."` tag carries a value.
// Recurses into nested structs. The function intentionally limits
// itself to string fields; numeric fields in y-cluster configs are
// declared as strings (matching the historical qemu shape) so we
// don't have to mix types here.
//
// Tag values starting with `__...__` are treated as placeholders and
// skipped: those fields are filled by callers who know how to
// resolve the placeholder (e.g. K3sConfig fields read from the pin
// file via the K3sDefault* helpers above).
func applyTagDefaults(target any) {
	rv := reflect.ValueOf(target)
	if rv.Kind() == reflect.Pointer {
		rv = rv.Elem()
	}
	applyDefaultsInto(rv)
}

func applyDefaultsInto(rv reflect.Value) {
	if !rv.IsValid() || rv.Kind() != reflect.Struct {
		return
	}
	rt := rv.Type()
	for i := 0; i < rt.NumField(); i++ {
		f := rt.Field(i)
		fv := rv.Field(i)
		if !fv.CanSet() {
			continue
		}
		if fv.Kind() == reflect.Struct {
			applyDefaultsInto(fv)
			continue
		}
		if fv.Kind() != reflect.String {
			continue
		}
		if fv.String() != "" {
			continue
		}
		def := parseJSONSchemaDefault(f.Tag.Get("jsonschema"))
		if def == "" || isPlaceholder(def) {
			continue
		}
		fv.SetString(def)
	}
}

// parseJSONSchemaDefault extracts the value of a `default=...` clause
// from the comma-separated jsonschema struct tag. Returns the value
// or "" when no default is declared.
func parseJSONSchemaDefault(tag string) string {
	for _, part := range strings.Split(tag, ",") {
		part = strings.TrimSpace(part)
		if v, ok := strings.CutPrefix(part, "default="); ok {
			return v
		}
	}
	return ""
}

func isPlaceholder(s string) bool {
	return strings.HasPrefix(s, "__") && strings.HasSuffix(s, "__")
}
