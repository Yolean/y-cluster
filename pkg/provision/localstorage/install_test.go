package localstorage

import (
	"strings"
	"testing"
)

// TestRender_Defaults pins the rendered shape against the
// appliance-default values: path /data/yolean, predictable
// pattern, Retain reclaim. These are the values that ship in
// every customer build unless they explicitly override.
func TestRender_Defaults(t *testing.T) {
	body := mustRender(t, Options{
		Path:          "/data/yolean",
		Pattern:       "{{ .PVC.Namespace }}_{{ .PVC.Name }}",
		ReclaimPolicy: "Retain",
	})

	for _, want := range []string{
		// Path lands in the nodePathMap.
		`"paths": ["/data/yolean"]`,
		// Pattern lands on the StorageClass parameters --
		// upstream local-path-provisioner reads pathPattern
		// from there, NOT from the ConfigMap.
		`pathPattern: "{{ .PVC.Namespace }}_{{ .PVC.Name }}"`,
		// allowUnsafePathPattern: "true" lets the underscore
		// form pass the upstream "must start with ns/name/"
		// safety check.
		`allowUnsafePathPattern: "true"`,
		// ReclaimPolicy: Retain is on the StorageClass, not
		// the upstream Delete.
		"reclaimPolicy: Retain",
		// StorageClass name + default-class annotation pinned.
		"name: local-path",
		`storageclass.kubernetes.io/is-default-class: "true"`,
		// Image pinned by digest (regression guard against an
		// accidental `:latest` retag).
		"@sha256:34ff0847cc47ebf69656ba44a3de9324596d0036b66ffd323b21614dd8221530",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("rendered manifest missing %q", want)
		}
	}
}

// TestRender_RespectsOverrides covers the three knobs being
// independent: a customer who only overrides Path keeps the
// pattern + reclaim defaults, etc.
func TestRender_RespectsOverrides(t *testing.T) {
	body := mustRender(t, Options{
		Path:          "/mnt/customer-data",
		Pattern:       "{{ .PVName }}",
		ReclaimPolicy: "Delete",
	})

	for _, want := range []string{
		`"paths": ["/mnt/customer-data"]`,
		`pathPattern: "{{ .PVName }}"`,
		"reclaimPolicy: Delete",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("rendered manifest missing %q\n\n---rendered---\n%s", want, body)
		}
	}
}

// TestRender_RequiresAllFields surfaces friendly errors when
// callers forget to fill a knob (a defaults bug would land here
// before any kubectl invocation, not silently as an empty value
// in the ConfigMap).
func TestRender_RequiresAllFields(t *testing.T) {
	cases := []struct {
		name string
		opts Options
		want string
	}{
		{"path", Options{Pattern: "p", ReclaimPolicy: "Retain"}, "Path is required"},
		{"pattern", Options{Path: "/p", ReclaimPolicy: "Retain"}, "Pattern is required"},
		{"reclaim", Options{Path: "/p", Pattern: "p"}, "ReclaimPolicy is required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Render(tc.opts)
			if err == nil {
				t.Fatalf("expected error mentioning %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q should mention %q", err.Error(), tc.want)
			}
		})
	}
}

// TestRender_NoTemplateLeakage: the .Pattern field contains
// `{{ ... }}` text/template syntax that the local-path-provisioner
// re-parses at runtime. Our Go-side render must NOT try to
// interpret it -- a leak would manifest as the pattern resolving
// to "<no value>" in the rendered ConfigMap.
func TestRender_NoTemplateLeakage(t *testing.T) {
	body := mustRender(t, Options{
		Path:          "/data/yolean",
		Pattern:       "{{ .PVC.Namespace }}_{{ .PVC.Name }}",
		ReclaimPolicy: "Retain",
	})
	if strings.Contains(body, "<no value>") {
		t.Errorf("template leakage; rendered manifest contains <no value>:\n%s", body)
	}
	if !strings.Contains(body, `{{ .PVC.Namespace }}_{{ .PVC.Name }}`) {
		t.Errorf("pattern not preserved verbatim:\n%s", body)
	}
}

func mustRender(t *testing.T, opts Options) string {
	t.Helper()
	body, err := Render(opts)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}
