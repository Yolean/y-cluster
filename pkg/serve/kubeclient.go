package serve

import (
	"fmt"
	"os"
	"strings"

	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// saNamespacePath is where a pod's assigned namespace lives when the
// process runs inside a Kubernetes cluster. Present only in-cluster,
// so stat-ing it is a quick in-cluster detector.
const saNamespacePath = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"

// loadK8sConfig returns a REST config and resolved namespace for the
// in-cluster backend. Strategy:
//
//  1. If the caller pinned Kubeconfig or Context, build from those.
//  2. Otherwise, try rest.InClusterConfig().
//  3. Fall back to clientcmd default loading rules (KUBECONFIG env,
//     ~/.kube/config, etc.).
//
// Namespace resolution, first match wins:
//
//  1. cfg.Namespace (explicit).
//  2. /var/run/secrets/kubernetes.io/serviceaccount/namespace
//     (present in-cluster).
//  3. kubeconfig current-context namespace.
//  4. "default".
func loadK8sConfig(cfg YKustomizeInClusterConfig) (*rest.Config, string, error) {
	var restCfg *rest.Config
	var kubeconfigNamespace string

	explicit := cfg.Kubeconfig != "" || cfg.Context != ""
	if !explicit {
		if c, err := rest.InClusterConfig(); err == nil {
			restCfg = c
		}
	}

	if restCfg == nil {
		rules := clientcmd.NewDefaultClientConfigLoadingRules()
		if cfg.Kubeconfig != "" {
			rules.ExplicitPath = cfg.Kubeconfig
		}
		overrides := &clientcmd.ConfigOverrides{}
		if cfg.Context != "" {
			overrides.CurrentContext = cfg.Context
		}
		kc := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, overrides)
		c, err := kc.ClientConfig()
		if err != nil {
			return nil, "", fmt.Errorf("load kubeconfig: %w", err)
		}
		restCfg = c
		// The current-context namespace becomes a namespace fallback.
		if ns, _, err := kc.Namespace(); err == nil {
			kubeconfigNamespace = ns
		}
	}

	ns := cfg.Namespace
	if ns == "" {
		if inCluster, err := os.ReadFile(saNamespacePath); err == nil {
			ns = strings.TrimSpace(string(inCluster))
		}
	}
	if ns == "" {
		ns = kubeconfigNamespace
	}
	if ns == "" {
		ns = "default"
	}

	return restCfg, ns, nil
}
