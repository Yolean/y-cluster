package configfile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type sample struct {
	Dir   string
	Name  string `yaml:"name"`
	Value int    `yaml:"value"`
}

func (s *sample) SetDir(d string) { s.Dir = d }

type sampleValidator struct {
	sample
	failOn string
}

func (s *sampleValidator) SetDir(d string)   { s.Dir = d }
func (s *sampleValidator) Validate() error {
	if s.Name == s.failOn {
		return errSample("name == failOn")
	}
	return nil
}

type errSample string

func (e errSample) Error() string { return string(e) }

func TestLoad_Happy(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"),
		[]byte("name: foo\nvalue: 7\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var s sample
	if err := Load(dir, "config.yaml", &s); err != nil {
		t.Fatal(err)
	}
	if s.Name != "foo" || s.Value != 7 {
		t.Fatalf("decode: %+v", s)
	}
	if !filepath.IsAbs(s.Dir) || filepath.Base(s.Dir) != filepath.Base(dir) {
		t.Fatalf("SetDir not called or wrong: %q", s.Dir)
	}
}

func TestLoad_MissingDir(t *testing.T) {
	var s sample
	err := Load(filepath.Join(t.TempDir(), "nope"), "config.yaml", &s)
	if err == nil {
		t.Fatal("want missing-dir error")
	}
}

func TestLoad_NotADirectory(t *testing.T) {
	f := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	var s sample
	err := Load(f, "config.yaml", &s)
	if err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("want not-a-directory error, got %v", err)
	}
}

func TestLoad_MissingFile(t *testing.T) {
	var s sample
	err := Load(t.TempDir(), "config.yaml", &s)
	if err == nil {
		t.Fatal("want missing-file error")
	}
}

func TestLoad_StrictDecodeRejectsUnknownFields(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"),
		[]byte("name: foo\nbogus: yes\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var s sample
	err := Load(dir, "config.yaml", &s)
	if err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("want unknown-field error, got %v", err)
	}
}

func TestLoad_ValidationFailureWrapsPath(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"),
		[]byte("name: bad\nvalue: 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var s sampleValidator
	s.failOn = "bad"
	err := Load(dir, "config.yaml", &s)
	if err == nil {
		t.Fatal("want validate error")
	}
	if !strings.Contains(err.Error(), "config.yaml") {
		t.Fatalf("error should name the path, got %v", err)
	}
	if !strings.Contains(err.Error(), "name == failOn") {
		t.Fatalf("error should preserve the validator's message, got %v", err)
	}
}

type sampleDefaulter struct {
	sample
	defaultsApplied bool
}

func (s *sampleDefaulter) SetDir(d string)      { s.Dir = d }
func (s *sampleDefaulter) ApplyDefaults()       { s.defaultsApplied = true }
func (s *sampleDefaulter) Validate() error {
	if !s.defaultsApplied {
		return errSample("Validate ran before ApplyDefaults")
	}
	return nil
}

func TestLoad_DefaulterRunsBeforeValidate(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"),
		[]byte("name: x\nvalue: 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var s sampleDefaulter
	if err := Load(dir, "config.yaml", &s); err != nil {
		t.Fatal(err)
	}
	if !s.defaultsApplied {
		t.Fatal("ApplyDefaults was not called")
	}
}

func TestLoad_NoOptionalInterfacesIsFine(t *testing.T) {
	type plain struct {
		Name string `yaml:"name"`
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"),
		[]byte("name: x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var p plain
	if err := Load(dir, "config.yaml", &p); err != nil {
		t.Fatal(err)
	}
	if p.Name != "x" {
		t.Fatalf("got %+v", p)
	}
}

// envsubstSample exercises the configfile -> pkg/envsubst wiring.
// Tagged token gets expanded; untagged value rejects ${...} so
// existing configs (which tag nothing) cannot grow accidental
// substitution support without an explicit schema change.
type envsubstSample struct {
	Plain string `yaml:"plain"`
	Token string `yaml:"token" envsubst:"true"`
}

func TestLoad_EnvSubstExpandsTaggedField(t *testing.T) {
	t.Setenv("Y_CLUSTER_TEST_TOKEN", "from-env")
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"),
		[]byte("plain: literal\ntoken: 'pre-${Y_CLUSTER_TEST_TOKEN}-post'\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var s envsubstSample
	if err := Load(dir, "config.yaml", &s); err != nil {
		t.Fatal(err)
	}
	if s.Token != "pre-from-env-post" {
		t.Fatalf("token: %q", s.Token)
	}
	if s.Plain != "literal" {
		t.Fatalf("plain: %q", s.Plain)
	}
}

func TestLoad_EnvSubstRejectsUntaggedRef(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"),
		[]byte("plain: 'try-${SECRET}'\ntoken: ok\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var s envsubstSample
	err := Load(dir, "config.yaml", &s)
	if err == nil {
		t.Fatal("want policy error for ${...} on untagged field")
	}
	if !strings.Contains(err.Error(), "plain") {
		t.Fatalf("error should name the offending path `plain`, got %v", err)
	}
}

func TestLoad_EnvSubstUndefinedFailsLoud(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"),
		[]byte("plain: ok\ntoken: '${Y_CLUSTER_TEST_DEFINITELY_UNSET}'\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var s envsubstSample
	err := Load(dir, "config.yaml", &s)
	if err == nil || !strings.Contains(err.Error(), "Y_CLUSTER_TEST_DEFINITELY_UNSET") {
		t.Fatalf("want undefined-var error, got %v", err)
	}
}

func TestLoad_DirAwareWithoutValidator(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"),
		[]byte("name: only-dir\nvalue: 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var s sample
	if err := Load(dir, "config.yaml", &s); err != nil {
		t.Fatal(err)
	}
	if s.Dir == "" {
		t.Fatal("DirAware path skipped when no Validator on the type")
	}
}
