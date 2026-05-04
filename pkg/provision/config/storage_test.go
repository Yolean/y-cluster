package config

import "testing"

// TestStorage_Defaults pins the appliance-shape defaults: an
// opinionated path that customers can mount a separate disk at,
// a predictable per-PV directory pattern that lets a fresh
// appliance disk rebind to the same data by namespace+name, and
// Retain reclaim so a stray `kubectl delete pvc` doesn't wipe
// customer data.
func TestStorage_Defaults(t *testing.T) {
	c := &CommonConfig{}
	c.applyCommonDefaults()
	if c.Storage.Path != "/data/yolean" {
		t.Errorf("Storage.Path: got %q, want /data/yolean", c.Storage.Path)
	}
	if c.Storage.PathPattern != "{{ .PVC.Namespace }}_{{ .PVC.Name }}" {
		t.Errorf("Storage.PathPattern: got %q, want {{ .PVC.Namespace }}_{{ .PVC.Name }}", c.Storage.PathPattern)
	}
	if c.Storage.ReclaimPolicy != "Retain" {
		t.Errorf("Storage.ReclaimPolicy: got %q, want Retain", c.Storage.ReclaimPolicy)
	}
}

// TestStorage_PreservesExplicitOverrides: each of the three knobs
// is independently overridable. Customers attaching a disk at a
// non-default mountpoint, or using the upstream UUID-suffixed
// pattern, or wanting Delete reclaim, must each survive the
// defaulter unchanged.
func TestStorage_PreservesExplicitOverrides(t *testing.T) {
	cases := []struct {
		name string
		in   StorageConfig
	}{
		{"path only", StorageConfig{Path: "/mnt/customer-data"}},
		{"pattern only", StorageConfig{PathPattern: "{{ .PVC.Namespace }}/{{ .PVC.Name }}-{{ .PVName }}"}},
		{"reclaim only", StorageConfig{ReclaimPolicy: "Delete"}},
		{"all three", StorageConfig{
			Path:          "/srv/data",
			PathPattern:   "{{ .PVName }}",
			ReclaimPolicy: "Delete",
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := &CommonConfig{Storage: tc.in}
			c.applyCommonDefaults()
			if tc.in.Path != "" && c.Storage.Path != tc.in.Path {
				t.Errorf("Path: got %q, want %q", c.Storage.Path, tc.in.Path)
			}
			if tc.in.PathPattern != "" && c.Storage.PathPattern != tc.in.PathPattern {
				t.Errorf("PathPattern: got %q, want %q", c.Storage.PathPattern, tc.in.PathPattern)
			}
			if tc.in.ReclaimPolicy != "" && c.Storage.ReclaimPolicy != tc.in.ReclaimPolicy {
				t.Errorf("ReclaimPolicy: got %q, want %q", c.Storage.ReclaimPolicy, tc.in.ReclaimPolicy)
			}
		})
	}
}
