package k8sapply

import (
	"testing"
)

func TestDecodeUnstructured_Multidoc(t *testing.T) {
	yml := []byte(`
apiVersion: v1
kind: Namespace
metadata:
  name: dev
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: cm
  namespace: dev
data:
  k: v
`)
	objs, err := decodeUnstructured(yml)
	if err != nil {
		t.Fatal(err)
	}
	if len(objs) != 2 {
		t.Fatalf("got %d objs, want 2", len(objs))
	}
	if objs[0].GetKind() != "Namespace" {
		t.Fatalf("kind 0: %q", objs[0].GetKind())
	}
	if objs[1].GetKind() != "ConfigMap" || objs[1].GetNamespace() != "dev" {
		t.Fatalf("obj 1: %+v", objs[1])
	}
}

func TestDecodeUnstructured_SkipsEmpty(t *testing.T) {
	yml := []byte(`
---
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: cm
---
`)
	objs, err := decodeUnstructured(yml)
	if err != nil {
		t.Fatal(err)
	}
	if len(objs) != 1 {
		t.Fatalf("got %d, want 1", len(objs))
	}
}

func TestSplitCRDs_OrdersCRDsFirst(t *testing.T) {
	yml := []byte(`
apiVersion: v1
kind: ConfigMap
metadata:
  name: a
---
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: foos.example.com
---
apiVersion: example.com/v1
kind: Foo
metadata:
  name: f
`)
	objs, err := decodeUnstructured(yml)
	if err != nil {
		t.Fatal(err)
	}
	crds, others := splitCRDs(objs)
	if len(crds) != 1 || crds[0].GetName() != "foos.example.com" {
		t.Fatalf("crds: %+v", crds)
	}
	if len(others) != 2 || others[0].GetKind() != "ConfigMap" || others[1].GetKind() != "Foo" {
		t.Fatalf("others: %+v", others)
	}
}

func TestSplitCRDs_NonExtensionsCRDKindIgnored(t *testing.T) {
	yml := []byte(`
apiVersion: somewhere.else/v1
kind: CustomResourceDefinition
metadata:
  name: not-a-real-crd
`)
	objs, err := decodeUnstructured(yml)
	if err != nil {
		t.Fatal(err)
	}
	crds, others := splitCRDs(objs)
	// We only special-case the real apiextensions.k8s.io CRD;
	// a same-Kind resource in a different group must not be
	// reordered.
	if len(crds) != 0 {
		t.Fatalf("crds: %+v", crds)
	}
	if len(others) != 1 {
		t.Fatalf("others: %+v", others)
	}
}
