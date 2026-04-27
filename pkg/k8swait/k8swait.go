// Package k8swait re-implements the subset of `kubectl wait` and
// `kubectl rollout status` y-cluster's checks need, using
// client-go directly so callers get typed errors instead of
// "exit status 1" parsed out of kubectl's stderr.
//
// What's supported:
//
//	wait --for=condition=<Type>           Resource has a status.conditions
//	wait --for=condition=<Type>=<Status>  entry of (Type, Status). Default
//	                                      status is "True" when omitted.
//	wait --for=delete                     Resource disappears.
//	wait --for=jsonpath={path}=value      JSONPath evaluation equals value.
//	rollout status deployment/<name>      Deployment fully rolled out.
//	rollout status statefulset/<name>     StatefulSet fully rolled out.
//	rollout status daemonset/<name>       DaemonSet fully rolled out.
//
// "Resource" is given as "<kind>/<name>" (or "<kind>.<group>/<name>"
// for non-core APIs). Discovery + RESTMapper resolves to the
// concrete GVR — same path kubectl walks.
//
// Anything outside this surface returns ErrUnsupportedFor or
// ErrUnsupportedKind. The caller can fall back to the kubectl
// CLI (or fail). This package is *not* a full kubectl wait
// re-implementation; it's the contract our checks rely on.
package k8swait

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/discovery"
	memory "k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/jsonpath"
)

var (
	// ErrUnsupportedFor signals that the given --for spec uses a
	// pattern this package doesn't implement (e.g. `--for=create`).
	// The caller can decide whether to fall back to kubectl or fail.
	ErrUnsupportedFor = errors.New("k8swait: unsupported --for syntax")

	// ErrUnsupportedKind signals that rollout status was asked for
	// a resource kind that doesn't have a meaningful rollout.
	ErrUnsupportedKind = errors.New("k8swait: unsupported rollout kind")

	// ErrTimeout is returned when the wait deadline elapses.
	ErrTimeout = errors.New("k8swait: timed out waiting for the condition")
)

// pollInterval is how often we re-fetch the resource. kubectl
// wait uses informers and reacts immediately; we use a 1s poll
// because the implementation is dramatically simpler and the
// difference doesn't matter for our convergence loops.
const pollInterval = 1 * time.Second

// Wait blocks until the --for predicate evaluates true on the
// named resource, or timeout. contextName picks the kubeconfig
// context (clientcmd default loading rules); namespace is empty
// for cluster-scoped kinds.
func Wait(ctx context.Context, contextName, resource, namespace, forSpec string, timeout time.Duration) error {
	cli, err := newClients(contextName)
	if err != nil {
		return err
	}
	gvr, name, err := cli.parseResource(resource)
	if err != nil {
		return err
	}
	predicate, err := compileForSpec(forSpec)
	if err != nil {
		return err
	}

	timedCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	pollErr := wait.PollUntilContextCancel(timedCtx, pollInterval, true, func(ctx context.Context) (bool, error) {
		obj, err := cli.dyn.Resource(gvr).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
		if predicate.OnDelete && apierrors.IsNotFound(err) {
			return true, nil
		}
		if err != nil {
			if apierrors.IsNotFound(err) {
				return false, nil
			}
			return false, err
		}
		ok, err := predicate.eval(obj.Object)
		return ok, err
	})
	if errors.Is(pollErr, context.DeadlineExceeded) {
		return fmt.Errorf("%w: %s for=%s", ErrTimeout, resource, forSpec)
	}
	return pollErr
}

// RolloutStatus blocks until the Deployment / StatefulSet /
// DaemonSet identified by `resource` finishes its current
// rollout, or timeout. The progression rules mirror kubectl's
// rollout status output for each kind.
func RolloutStatus(ctx context.Context, contextName, resource, namespace string, timeout time.Duration) error {
	cli, err := newClients(contextName)
	if err != nil {
		return err
	}
	kind, name, err := splitResource(resource)
	if err != nil {
		return err
	}
	timedCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	check, err := rolloutChecker(cli.kube, strings.ToLower(kind))
	if err != nil {
		return err
	}
	pollErr := wait.PollUntilContextCancel(timedCtx, pollInterval, true, func(ctx context.Context) (bool, error) {
		return check(ctx, namespace, name)
	})
	if errors.Is(pollErr, context.DeadlineExceeded) {
		return fmt.Errorf("%w: rollout %s", ErrTimeout, resource)
	}
	return pollErr
}

// rolloutChecker returns a per-kind status function. Returning
// ErrUnsupportedKind for anything off the supported list keeps
// the public Wait/RolloutStatus surface honest.
func rolloutChecker(kube kubernetes.Interface, kind string) (func(context.Context, string, string) (bool, error), error) {
	switch kind {
	case "deployment", "deployments", "deploy":
		return func(ctx context.Context, ns, name string) (bool, error) {
			d, err := kube.AppsV1().Deployments(ns).Get(ctx, name, metav1.GetOptions{})
			if err != nil {
				if apierrors.IsNotFound(err) {
					return false, nil
				}
				return false, err
			}
			if d.Generation > d.Status.ObservedGeneration {
				return false, nil
			}
			desired := int32(0)
			if d.Spec.Replicas != nil {
				desired = *d.Spec.Replicas
			}
			return d.Status.UpdatedReplicas == desired &&
				d.Status.Replicas == desired &&
				d.Status.AvailableReplicas == desired, nil
		}, nil
	case "statefulset", "statefulsets", "sts":
		return func(ctx context.Context, ns, name string) (bool, error) {
			s, err := kube.AppsV1().StatefulSets(ns).Get(ctx, name, metav1.GetOptions{})
			if err != nil {
				if apierrors.IsNotFound(err) {
					return false, nil
				}
				return false, err
			}
			if s.Generation > s.Status.ObservedGeneration {
				return false, nil
			}
			desired := int32(0)
			if s.Spec.Replicas != nil {
				desired = *s.Spec.Replicas
			}
			return s.Status.UpdatedReplicas == desired &&
				s.Status.ReadyReplicas == desired, nil
		}, nil
	case "daemonset", "daemonsets", "ds":
		return func(ctx context.Context, ns, name string) (bool, error) {
			d, err := kube.AppsV1().DaemonSets(ns).Get(ctx, name, metav1.GetOptions{})
			if err != nil {
				if apierrors.IsNotFound(err) {
					return false, nil
				}
				return false, err
			}
			if d.Generation > d.Status.ObservedGeneration {
				return false, nil
			}
			return d.Status.UpdatedNumberScheduled == d.Status.DesiredNumberScheduled &&
				d.Status.NumberAvailable == d.Status.DesiredNumberScheduled, nil
		}, nil
	default:
		return nil, fmt.Errorf("%w: %s", ErrUnsupportedKind, kind)
	}
}

// clients bundles the dynamic + typed clients + a RESTMapper so
// every Wait/RolloutStatus call doesn't redo discovery.
type clients struct {
	dyn    dynamic.Interface
	kube   kubernetes.Interface
	mapper meta.RESTMapper
}

// newClients loads contextName, builds a *rest.Config, and
// instantiates the clients. Discovery is wrapped in a memory
// cache so repeat parseResource calls don't hammer the API.
func newClients(contextName string) (*clients, error) {
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	overrides := &clientcmd.ConfigOverrides{CurrentContext: contextName}
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, overrides).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("kubeconfig %s: %w", contextName, err)
	}
	return newClientsFromConfig(cfg)
}

func newClientsFromConfig(cfg *rest.Config) (*clients, error) {
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("dynamic client: %w", err)
	}
	kube, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("typed client: %w", err)
	}
	disc, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("discovery client: %w", err)
	}
	cached := memory.NewMemCacheClient(disc)
	mapper := restmapper.NewDeferredDiscoveryRESTMapper(cached)
	return &clients{dyn: dyn, kube: kube, mapper: mapper}, nil
}

// parseResource turns "kind/name" or "kind.group/name" into
// (GVR, name). The RESTMapper resolves kind/group → GroupVersionResource.
func (c *clients) parseResource(resource string) (schema.GroupVersionResource, string, error) {
	kind, name, err := splitResource(resource)
	if err != nil {
		return schema.GroupVersionResource{}, "", err
	}
	gk := schema.ParseGroupKind(kind)
	mapping, err := c.mapper.RESTMapping(gk)
	if err != nil {
		return schema.GroupVersionResource{}, "", fmt.Errorf("resolve %s: %w", kind, err)
	}
	return mapping.Resource, name, nil
}

func splitResource(resource string) (kind, name string, err error) {
	parts := strings.SplitN(resource, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("resource must be kind/name, got %q", resource)
	}
	return parts[0], parts[1], nil
}

// predicate is a compiled --for spec.
type predicate struct {
	OnDelete    bool
	conditionT  string // condition type, lowercased
	conditionV  string // expected status, lowercased
	jsonPathExp *jsonpath.JSONPath
	jsonPathRaw string // for error messages
	expected    string // expected jsonpath value
}

func (p predicate) eval(obj map[string]any) (bool, error) {
	if p.OnDelete {
		// Resource still exists → not yet deleted.
		return false, nil
	}
	if p.conditionT != "" {
		conds, _, _ := unstructuredConditions(obj)
		for _, c := range conds {
			if strings.EqualFold(c.Type, p.conditionT) && strings.EqualFold(c.Status, p.conditionV) {
				return true, nil
			}
		}
		return false, nil
	}
	if p.jsonPathExp != nil {
		var buf strings.Builder
		if err := p.jsonPathExp.Execute(&buf, obj); err != nil {
			return false, nil // jsonpath miss → not yet ready
		}
		return strings.TrimSpace(buf.String()) == p.expected, nil
	}
	return false, fmt.Errorf("predicate is empty")
}

// compileForSpec parses a kubectl-style --for=… string.
func compileForSpec(spec string) (predicate, error) {
	switch {
	case spec == "delete":
		return predicate{OnDelete: true}, nil
	case strings.HasPrefix(spec, "condition="):
		body := strings.TrimPrefix(spec, "condition=")
		// "Available" or "Available=True"
		t, v, hasV := strings.Cut(body, "=")
		if !hasV {
			v = "True"
		}
		return predicate{conditionT: t, conditionV: v}, nil
	case strings.HasPrefix(spec, "jsonpath="):
		body := strings.TrimPrefix(spec, "jsonpath=")
		// jsonpath={path}=value -- split on the first `=` after
		// the closing `}`.
		end := strings.LastIndex(body, "}")
		if end < 0 || end+1 >= len(body) || body[end+1] != '=' {
			return predicate{}, fmt.Errorf("%w: jsonpath needs {path}=value, got %q", ErrUnsupportedFor, spec)
		}
		path := body[:end+1]
		expected := body[end+2:]
		jp := jsonpath.New("k8swait")
		jp.AllowMissingKeys(true)
		if err := jp.Parse(path); err != nil {
			return predicate{}, fmt.Errorf("parse jsonpath %q: %w", path, err)
		}
		return predicate{jsonPathExp: jp, jsonPathRaw: path, expected: expected}, nil
	default:
		return predicate{}, fmt.Errorf("%w: %q", ErrUnsupportedFor, spec)
	}
}

// unstructuredConditions extracts status.conditions from an
// arbitrary object so the condition matcher works without
// typed-client knowledge of every kind.
type unstructuredCondition struct {
	Type   string
	Status string
}

func unstructuredConditions(obj map[string]any) ([]unstructuredCondition, bool, error) {
	statusObj, ok := obj["status"].(map[string]any)
	if !ok {
		return nil, false, nil
	}
	rawConds, ok := statusObj["conditions"].([]any)
	if !ok {
		return nil, false, nil
	}
	out := make([]unstructuredCondition, 0, len(rawConds))
	for _, rc := range rawConds {
		m, ok := rc.(map[string]any)
		if !ok {
			continue
		}
		t, _ := m["type"].(string)
		s, _ := m["status"].(string)
		out = append(out, unstructuredCondition{Type: t, Status: s})
	}
	return out, true, nil
}
