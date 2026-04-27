// Package images is the engine behind `y-cluster images list /
// cache / load`. The pipeline lets a developer extract container
// image references from any YAML stream, pull each into a local
// OCI layout, and (with a real cluster) import OCI archives into
// the node's containerd.
//
// This file is ListYAML: walks every PodSpec in a YAML stream
// and emits a sorted, deduplicated set of refs. No kustomize, no
// network, no cluster. Callers that have a kustomize tree pipe
// `kubectl kustomize ./base | y-cluster images list -` rather
// than asking us to embed the build step.
package images

import (
	"bytes"
	"fmt"
	"io"
	"sort"

	"sigs.k8s.io/yaml"
)

// ListYAML reads a multi-document YAML stream from r and
// returns the sorted, deduplicated container image references
// from any PodSpec the stream contains. Both
// `containers[*].image` and `initContainers[*].image` count,
// across Pod / Deployment / StatefulSet / DaemonSet /
// ReplicaSet / Job / CronJob — i.e. the kinds whose spec
// ultimately wraps a corev1.PodSpec.
//
// Empty / missing image fields are skipped (kubectl would
// reject such manifests; we don't second-guess the input).
func ListYAML(r io.Reader) ([]string, error) {
	body, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("read yaml: %w", err)
	}
	set := map[string]struct{}{}
	for _, doc := range splitYAMLDocs(body) {
		var obj map[string]any
		if err := yaml.Unmarshal(doc, &obj); err != nil {
			return nil, fmt.Errorf("parse yaml: %w", err)
		}
		if obj == nil {
			continue
		}
		extractImages(obj, set)
	}
	out := make([]string, 0, len(set))
	for ref := range set {
		out = append(out, ref)
	}
	sort.Strings(out)
	return out, nil
}

// splitYAMLDocs splits a multi-document YAML stream by `---`.
// `\n---\n` is the kustomize / kubectl convention; surrounding
// whitespace is trimmed so a leading/trailing separator doesn't
// produce an empty doc.
func splitYAMLDocs(b []byte) [][]byte {
	const sep = "\n---\n"
	// Tolerate an opening separator on the first line (kustomize
	// occasionally emits one) by prefixing a newline so the
	// SplitN sees a clean boundary.
	if bytes.HasPrefix(b, []byte("---\n")) {
		b = append([]byte{'\n'}, b...)
	}
	parts := bytes.Split(b, []byte(sep))
	out := make([][]byte, 0, len(parts))
	for _, p := range parts {
		p = bytes.TrimSpace(p)
		if len(p) > 0 {
			out = append(out, p)
		}
	}
	return out
}

// extractImages adds every image reference under doc's PodSpec
// (if any) to set. A document without a recognised PodSpec —
// ConfigMap, Service, Secret, etc. — contributes nothing.
func extractImages(doc map[string]any, set map[string]struct{}) {
	spec := findPodSpec(doc)
	if spec == nil {
		return
	}
	for _, key := range []string{"containers", "initContainers"} {
		list, _ := spec[key].([]any)
		for _, c := range list {
			cm, _ := c.(map[string]any)
			img, _ := cm["image"].(string)
			if img != "" {
				set[img] = struct{}{}
			}
		}
	}
}

// findPodSpec returns the corev1.PodSpec map nested inside doc,
// or nil for kinds that don't wrap a PodSpec. The cases below
// are the only kinds we expect to need; pod-template-bearing
// CRDs would require an explicit allow-list (and a kustomize
// plugin that kustomize already invokes for its own spec
// patches), out of scope here.
func findPodSpec(doc map[string]any) map[string]any {
	kind, _ := doc["kind"].(string)
	spec, _ := doc["spec"].(map[string]any)
	if spec == nil {
		return nil
	}
	switch kind {
	case "Pod":
		return spec
	case "Deployment", "StatefulSet", "DaemonSet", "ReplicaSet", "Job":
		t, _ := spec["template"].(map[string]any)
		if t == nil {
			return nil
		}
		ts, _ := t["spec"].(map[string]any)
		return ts
	case "CronJob":
		jt, _ := spec["jobTemplate"].(map[string]any)
		if jt == nil {
			return nil
		}
		js, _ := jt["spec"].(map[string]any)
		if js == nil {
			return nil
		}
		pt, _ := js["template"].(map[string]any)
		if pt == nil {
			return nil
		}
		ps, _ := pt["spec"].(map[string]any)
		return ps
	}
	return nil
}
