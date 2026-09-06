package inventory

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSaveListRemove_Roundtrip(t *testing.T) {
	t.Setenv("Y_CLUSTER_INVENTORY_DIR", t.TempDir())

	recs, err := List()
	if err != nil {
		t.Fatalf("List on empty dir: %v", err)
	}
	if len(recs) != 0 {
		t.Fatalf("expected no records, got %d", len(recs))
	}

	if err := Save(Record{
		Context:   "node-agent",
		Name:      "node-agent",
		Provider:  "docker",
		ConfigDir: "/repo/itest/cluster/docker",
		HostPorts: []string{"26443"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := Save(Record{
		Context:   "local",
		Name:      "y-cluster",
		Provider:  "qemu",
		ConfigDir: "/repo/cluster",
		HostPorts: []string{"6443", "2222"},
	}); err != nil {
		t.Fatal(err)
	}

	recs, err = List()
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 2 {
		t.Fatalf("expected 2 records, got %d", len(recs))
	}
	// Sorted by context: "local" < "node-agent".
	if recs[0].Context != "local" || recs[1].Context != "node-agent" {
		t.Fatalf("unexpected order: %q, %q", recs[0].Context, recs[1].Context)
	}
	if recs[1].ConfigDir != "/repo/itest/cluster/docker" {
		t.Fatalf("configDir not persisted: %+v", recs[1])
	}
	if recs[0].Version != recordVersion || recs[0].ProvisionedAt == "" {
		t.Fatalf("Save should stamp version and timestamp: %+v", recs[0])
	}

	if err := Remove("node-agent"); err != nil {
		t.Fatal(err)
	}
	if err := Remove("node-agent"); err != nil {
		t.Fatalf("Remove must be idempotent: %v", err)
	}
	recs, err = List()
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 || recs[0].Context != "local" {
		t.Fatalf("expected only 'local' left, got %+v", recs)
	}
}

func TestSave_Overwrite(t *testing.T) {
	t.Setenv("Y_CLUSTER_INVENTORY_DIR", t.TempDir())
	for _, dir := range []string{"/old/path", "/new/path"} {
		if err := Save(Record{Context: "local", Name: "n", Provider: "docker", ConfigDir: dir}); err != nil {
			t.Fatal(err)
		}
	}
	recs, err := List()
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 || recs[0].ConfigDir != "/new/path" {
		t.Fatalf("re-provision should overwrite the context's record: %+v", recs)
	}
}

func TestSave_RejectsPathySeparatorContext(t *testing.T) {
	t.Setenv("Y_CLUSTER_INVENTORY_DIR", t.TempDir())
	if err := Save(Record{Context: "../escape"}); err == nil {
		t.Fatal("context with path separator must be rejected")
	}
	if err := Save(Record{Context: ""}); err == nil {
		t.Fatal("empty context must be rejected")
	}
}

func TestList_SkipsCorruptAndForeignFiles(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("Y_CLUSTER_INVENTORY_DIR", dir)
	if err := Save(Record{Context: "good", Name: "n", Provider: "docker", ConfigDir: "/p"}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "bad.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "future.json"), []byte(`{"version": 99, "context": "future"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "README.txt"), []byte("not a record"), 0o644); err != nil {
		t.Fatal(err)
	}
	recs, err := List()
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 || recs[0].Context != "good" {
		t.Fatalf("expected only the good record, got %+v", recs)
	}
}

func TestFindByHostPort(t *testing.T) {
	t.Setenv("Y_CLUSTER_INVENTORY_DIR", t.TempDir())
	if err := Save(Record{Context: "local", Name: "n", Provider: "docker", ConfigDir: "/p", HostPorts: []string{"26443", "80"}}); err != nil {
		t.Fatal(err)
	}
	if rec := FindByHostPort("26443"); rec == nil || rec.Context != "local" {
		t.Fatalf("expected match on 26443, got %+v", rec)
	}
	if rec := FindByHostPort("9999"); rec != nil {
		t.Fatalf("expected no match on 9999, got %+v", rec)
	}
	if rec := FindByHostPort(""); rec != nil {
		t.Fatalf("empty port must not match, got %+v", rec)
	}
}
