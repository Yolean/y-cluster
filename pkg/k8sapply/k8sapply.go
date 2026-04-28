// Package k8sapply runs server-side apply against a kubernetes
// API directly via client-go's dynamic client. Replaces a
// `kubectl apply --server-side` shell-out. The motivation is the
// same one the rest of the lib-swap audit calls out: typed
// errors. apierrors.IsConflict, IsForbidden, IsServerTimeout,
// IsServiceUnavailable categorise the failure cases that
// previously came back as "exit status 1, see stderr".
//
// The "diff" between this and kubectl apply for our purposes:
//
//   - We use Patch with PatchType=ApplyYAMLPatchType + Force=true
//     and FieldManager "y-cluster". Same wire format as kubectl
//     apply --server-side --force-conflicts --field-manager=y-cluster.
//   - kustomize build (sigs.k8s.io/kustomize/api/krusty) produces
//     the YAML — same library kubectl uses since 1.21.
//   - CRDs are applied first when present in the input, then the
//     RESTMapper cache is invalidated, then the rest of the
//     objects. kubectl handles this internally; we replicate the
//     bit our checks need.
package k8sapply

import (
	"bytes"
	"context"
	"fmt"
	"io"

	"go.uber.org/zap"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/discovery"
	memory "k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/kustomize/api/krusty"
	"sigs.k8s.io/kustomize/kyaml/filesys"
)

// FieldManager is the SSA field-manager identity y-cluster
// claims for fields it sets. Mirrors `--field-manager=y-cluster`
// callers would have passed to kubectl.
const FieldManager = "y-cluster"

// DryRun controls whether Apply hits the apiserver in a no-op
// mode (DryRunServer) or commits (DryRunNone).
type DryRun string

const (
	DryRunNone   DryRun = ""
	DryRunServer DryRun = "server"
)

// Apply builds the kustomize tree at kustomizeDir and applies
// every resource via server-side apply. Equivalent to:
//
//	kubectl --context=<contextName> apply --server-side \
//	  --force-conflicts --field-manager=y-cluster -k <kustomizeDir>
//
// CRDs in the build output are applied first so subsequent
// objects of those types are recognised by the RESTMapper. Other
// resources go in input order — kustomize already produces a
// deterministic stream.
func Apply(ctx context.Context, contextName, kustomizeDir string, dryRun DryRun, logger *zap.Logger) error {
	if logger == nil {
		logger = zap.NewNop()
	}
	yml, err := buildKustomize(kustomizeDir)
	if err != nil {
		return err
	}
	objects, err := decodeUnstructured(yml)
	if err != nil {
		return fmt.Errorf("decode kustomize output: %w", err)
	}
	return applyObjects(ctx, contextName, objects, dryRun, logger)
}

// ApplyYAML is the raw-YAML counterpart to Apply: it skips kustomize
// build and applies the objects in `manifests` directly. Useful for
// embedded vendor YAML (Envoy Gateway install.yaml, Gateway API
// CRDs) where there's nothing for kustomize to build.
//
// CRD-first ordering and SSA semantics are identical to Apply.
func ApplyYAML(ctx context.Context, contextName string, manifests []byte, dryRun DryRun, logger *zap.Logger) error {
	if logger == nil {
		logger = zap.NewNop()
	}
	objects, err := decodeUnstructured(manifests)
	if err != nil {
		return fmt.Errorf("decode manifests: %w", err)
	}
	return applyObjects(ctx, contextName, objects, dryRun, logger)
}

// applyObjects is the shared loop both Apply and ApplyYAML drive
// after they've produced their list of unstructured objects.
func applyObjects(ctx context.Context, contextName string, objects []*unstructured.Unstructured, dryRun DryRun, logger *zap.Logger) error {
	cfg, defaultNS, err := restConfigForContext(contextName)
	if err != nil {
		return err
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return fmt.Errorf("dynamic client: %w", err)
	}
	disc, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		return fmt.Errorf("discovery client: %w", err)
	}
	cache := memory.NewMemCacheClient(disc)
	mapper := restmapper.NewDeferredDiscoveryRESTMapper(cache)

	patchOpts := metav1.PatchOptions{
		FieldManager: FieldManager,
		Force:        ptrTrue(),
	}
	if dryRun == DryRunServer {
		patchOpts.DryRun = []string{metav1.DryRunAll}
	}

	crds, others := splitCRDs(objects)
	if len(crds) > 0 {
		for _, obj := range crds {
			if err := applyOne(ctx, dyn, mapper, obj, defaultNS, patchOpts, logger); err != nil {
				return err
			}
		}
		// CRDs landed; subsequent CRs need fresh discovery. Memory
		// cache invalidation forces the next RESTMapping call to
		// re-fetch.
		cache.Invalidate()
	}
	for _, obj := range others {
		if err := applyOne(ctx, dyn, mapper, obj, defaultNS, patchOpts, logger); err != nil {
			return err
		}
	}
	return nil
}

// applyOne issues an SSA Patch for a single object.
//
// defaultNS is the namespace to use for namespace-scoped objects
// that don't declare metadata.namespace -- mirrors kubectl's
// "no metadata.namespace + no -n flag → context namespace (or
// 'default')" behaviour. Without this, namespace-scoped objects
// hit the apiserver with an empty namespace path segment, which
// kwok / a real apiserver alike answer with "the server could
// not find the requested resource".
func applyOne(ctx context.Context, dyn dynamic.Interface, mapper meta.RESTMapper, obj *unstructured.Unstructured, defaultNS string, opts metav1.PatchOptions, logger *zap.Logger) error {
	gvk := obj.GroupVersionKind()
	mapping, err := mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	if err != nil {
		return fmt.Errorf("rest mapping %s: %w", gvk, err)
	}
	ns := obj.GetNamespace()
	var ri dynamic.ResourceInterface
	if mapping.Scope.Name() == meta.RESTScopeNameNamespace {
		if ns == "" {
			ns = defaultNS
		}
		ri = dyn.Resource(mapping.Resource).Namespace(ns)
	} else {
		ri = dyn.Resource(mapping.Resource)
	}
	body, err := obj.MarshalJSON()
	if err != nil {
		return fmt.Errorf("marshal %s/%s: %w", gvk.Kind, obj.GetName(), err)
	}
	_, err = ri.Patch(ctx, obj.GetName(), types.ApplyPatchType, body, opts)
	if err != nil {
		// Surface the categorisable error for callers; e.g.
		// apierrors.IsConflict / IsForbidden / IsServerTimeout.
		return fmt.Errorf("apply %s/%s: %w", gvk.Kind, obj.GetName(), err)
	}
	logger.Debug("applied",
		zap.String("kind", gvk.Kind),
		zap.String("name", obj.GetName()),
		zap.String("namespace", ns),
	)
	return nil
}

// splitCRDs returns the CRDs first, the rest second, both in
// input order. Used so we can apply CRDs, invalidate discovery,
// then apply CRs of those CRDs in the same call.
func splitCRDs(objects []*unstructured.Unstructured) (crds, others []*unstructured.Unstructured) {
	for _, o := range objects {
		gvk := o.GroupVersionKind()
		if gvk.Kind == "CustomResourceDefinition" && gvk.Group == "apiextensions.k8s.io" {
			crds = append(crds, o)
			continue
		}
		others = append(others, o)
	}
	return
}

// buildKustomize runs the kustomize build at dir and returns the
// concatenated YAML stream. Same package the rest of y-cluster
// uses (pkg/images.List).
func buildKustomize(dir string) ([]byte, error) {
	fs := filesys.MakeFsOnDisk()
	k := krusty.MakeKustomizer(krusty.MakeDefaultOptions())
	rm, err := k.Run(fs, dir)
	if err != nil {
		return nil, fmt.Errorf("kustomize build %s: %w", dir, err)
	}
	yml, err := rm.AsYaml()
	if err != nil {
		return nil, fmt.Errorf("encode kustomize output: %w", err)
	}
	return yml, nil
}

// decodeUnstructured parses a kustomize YAML stream into typed
// unstructured objects. Empty / nil docs are skipped — kustomize
// occasionally emits trailing separators.
func decodeUnstructured(yml []byte) ([]*unstructured.Unstructured, error) {
	dec := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(yml), 4096)
	var out []*unstructured.Unstructured
	for {
		raw := map[string]any{}
		if err := dec.Decode(&raw); err != nil {
			if err == io.EOF {
				break
			}
			return nil, err
		}
		if len(raw) == 0 {
			continue
		}
		out = append(out, &unstructured.Unstructured{Object: raw})
	}
	return out, nil
}

// restConfigForContext loads the kubeconfig (default search
// rules), returns the *rest.Config for the named context plus
// the resolved default namespace. The default namespace is what
// kubectl uses for namespace-scoped resources whose manifest
// omits metadata.namespace and no -n flag is given: the
// context's namespace if set, otherwise "default".
func restConfigForContext(contextName string) (*rest.Config, string, error) {
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	overrides := &clientcmd.ConfigOverrides{CurrentContext: contextName}
	cc := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, overrides)
	cfg, err := cc.ClientConfig()
	if err != nil {
		return nil, "", fmt.Errorf("kubeconfig %s: %w", contextName, err)
	}
	ns, _, err := cc.Namespace()
	if err != nil {
		// clientcmd.Namespace returns "default" when the context
		// is missing a namespace; an actual error here means the
		// kubeconfig is unparseable, which ClientConfig() above
		// would already have caught. Defend against it anyway.
		return nil, "", fmt.Errorf("resolve namespace for %s: %w", contextName, err)
	}
	return cfg, ns, nil
}

func ptrTrue() *bool {
	t := true
	return &t
}
