package qemu

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// configFieldsNotPersisted names every Config field the sidecar
// deliberately leaves out, with the reason. loadState must return
// the zero value for them, except Kubeconfig which it re-reads from
// the environment.
var configFieldsNotPersisted = map[string]string{
	"Kubeconfig":   "environmental; re-resolved from $KUBECONFIG on load",
	"Registries":   "cluster state; written into the guest at provision",
	"Gateway":      "cluster state; installed into the cluster at provision",
	"Storage":      "cluster state; installed into the cluster at provision",
	"DataDiskSize": "only used to create a missing data disk at provision",
}

// fillNonZero sets every string reachable from v to a distinct
// non-zero value, recursing into structs and giving slices of
// structs one filled element.
func fillNonZero(t *testing.T, v reflect.Value, path string) {
	t.Helper()
	switch v.Kind() {
	case reflect.String:
		v.SetString("x-" + path)
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			fillNonZero(t, v.Field(i), path+"."+v.Type().Field(i).Name)
		}
	case reflect.Slice:
		elem := reflect.New(v.Type().Elem()).Elem()
		fillNonZero(t, elem, path+"[0]")
		v.Set(reflect.Append(v, elem))
	case reflect.Map, reflect.Ptr, reflect.Interface:
		// Left at zero. A persisted field of this kind fails the
		// round trip below, which is the prompt to teach this
		// helper about it.
	default:
		t.Fatalf("fillNonZero: unhandled kind %s at %s", v.Kind(), path)
	}
}

// TestSaveLoadState_RoundtripCoversEveryConfigField is the guard
// against a Config field that start silently loses: a field added to
// Config has to be persisted or listed in configFieldsNotPersisted.
func TestSaveLoadState_RoundtripCoversEveryConfigField(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("KUBECONFIG", "/path/to/kubeconfig")

	var cfg Config
	fillNonZero(t, reflect.ValueOf(&cfg).Elem(), "Config")
	cfg.CacheDir = dir
	if err := saveState(cfg); err != nil {
		t.Fatalf("saveState: %v", err)
	}
	got, err := loadState(dir, cfg.Name)
	if err != nil {
		t.Fatalf("loadState: %v", err)
	}

	want, have := reflect.ValueOf(cfg), reflect.ValueOf(got)
	for i := 0; i < want.NumField(); i++ {
		name := want.Type().Field(i).Name
		w, h := want.Field(i).Interface(), have.Field(i).Interface()
		if _, skip := configFieldsNotPersisted[name]; skip {
			if name == "Kubeconfig" {
				if h != "/path/to/kubeconfig" {
					t.Errorf("Kubeconfig: got %v, want the $KUBECONFIG value", h)
				}
				continue
			}
			if !reflect.ValueOf(h).IsZero() {
				t.Errorf("%s is listed as not persisted but came back as %v", name, h)
			}
			continue
		}
		if !reflect.DeepEqual(w, h) {
			t.Errorf("Config.%s does not survive stop/start: saved %v, loaded %v.\n"+
				"Persist it in savedState (saveState AND loadState), or add it to configFieldsNotPersisted with the reason.", name, w, h)
		}
	}
	for name := range configFieldsNotPersisted {
		if _, ok := want.Type().FieldByName(name); !ok {
			t.Errorf("configFieldsNotPersisted names %q, which is not a Config field", name)
		}
	}
}

func TestStartDisks(t *testing.T) {
	dataDisk := filepath.Join(t.TempDir(), "data.qcow2")
	if err := os.WriteFile(dataDisk, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	t.Run("no data disk: extras only", func(t *testing.T) {
		got, err := startDisks(Config{}, []string{"/x.qcow2"})
		if err != nil || !reflect.DeepEqual(got, []string{"/x.qcow2"}) {
			t.Fatalf("got %v, %v", got, err)
		}
	})
	t.Run("data disk keeps its provision-time slot, before extras", func(t *testing.T) {
		got, err := startDisks(Config{DataDisk: dataDisk}, []string{"/x.qcow2"})
		if err != nil || !reflect.DeepEqual(got, []string{dataDisk, "/x.qcow2"}) {
			t.Fatalf("got %v, %v", got, err)
		}
	})
	t.Run("missing data disk refuses to start", func(t *testing.T) {
		_, err := startDisks(Config{DataDisk: filepath.Join(t.TempDir(), "gone.qcow2")}, nil)
		if err == nil || !strings.Contains(err.Error(), "must not start without it") {
			t.Fatalf("want a refusal, got %v", err)
		}
	})
}

// TestLoadState_VersionMismatch covers the forward-compat guard:
// a sidecar with an unknown schema version errors loud rather
// than letting the caller proceed with a half-deserialized Config.
func TestLoadState_VersionMismatch(t *testing.T) {
	dir := t.TempDir()
	stale := []byte(`{"version":99,"name":"y-cluster-test","cacheDir":"/x"}`)
	if err := os.WriteFile(filepath.Join(dir, "y-cluster-test.json"), stale, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := loadState(dir, "y-cluster-test")
	if err == nil {
		t.Fatal("want error for stale version")
	}
	if !contains(err.Error(), "unsupported state version 99") {
		t.Fatalf("want version error, got %v", err)
	}
}

// TestLoadState_NotFound surfaces the os.ErrNotExist expected by
// `y-cluster start` so it can produce a friendly message ("no
// stopped cluster to start; run `y-cluster provision`").
func TestLoadState_NotFound(t *testing.T) {
	dir := t.TempDir()
	_, err := loadState(dir, "missing")
	if !os.IsNotExist(err) {
		t.Fatalf("want IsNotExist, got %v", err)
	}
}

func TestRemoveState_Idempotent(t *testing.T) {
	dir := t.TempDir()
	if err := removeState(dir, "missing"); err != nil {
		t.Fatalf("removeState on missing should be no-op: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "x.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := removeState(dir, "x"); err != nil {
		t.Fatalf("removeState: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "x.json")); !os.IsNotExist(err) {
		t.Fatal("file should have been removed")
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
