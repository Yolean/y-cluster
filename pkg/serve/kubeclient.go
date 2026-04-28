package serve

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"sigs.k8s.io/yaml"
)

// In-cluster service account mount paths. Present iff the process
// runs inside a pod with a SA mounted (the default).
const (
	saTokenPath     = "/var/run/secrets/kubernetes.io/serviceaccount/token"
	saCAPath        = "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"
	saNamespacePath = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"
)

// k8sClient is the minimal apiserver HTTP shape ykustomize-incluster
// needs: GET /api/v1/namespaces/<ns>/secrets?labelSelector=...
// plus a resolved namespace. Constructed from either in-cluster
// mounts or a kubeconfig file -- we hand-roll the schema so the
// binary doesn't pull in k8s.io/client-go.
type k8sClient struct {
	server     string       // base URL incl scheme
	httpClient *http.Client // CA pool + optional client cert
	bearer     string       // optional Bearer token; cert auth lives in TLS
	namespace  string
}

// listSecrets issues a GET against the Secret list endpoint with
// the given label selector. Returns the raw response body so the
// caller can json-decode into whatever shape it needs.
func (c *k8sClient) listSecrets(ctx context.Context, labelSelector string) ([]byte, error) {
	v := url.Values{}
	if labelSelector != "" {
		v.Set("labelSelector", labelSelector)
	}
	path := fmt.Sprintf("/api/v1/namespaces/%s/secrets", url.PathEscape(c.namespace))
	if encoded := v.Encode(); encoded != "" {
		path += "?" + encoded
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.server+path, nil)
	if err != nil {
		return nil, err
	}
	if c.bearer != "" {
		req.Header.Set("Authorization", "Bearer "+c.bearer)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("GET %s: %s: %s", path, resp.Status, snippet(body))
	}
	return body, nil
}

// snippet trims a response body for inclusion in error messages
// so a 500 doesn't dump kilobytes into a log line.
func snippet(b []byte) string {
	const max = 200
	s := string(b)
	if len(s) > max {
		return s[:max] + "..."
	}
	return s
}

// newK8sClient resolves apiserver auth from either a kubeconfig
// (when cfg.Kubeconfig or cfg.Context is set, or the in-cluster
// mounts aren't present) or in-cluster service account mounts.
//
// Strategy mirrors the previous clientcmd-based loader:
//
//  1. If the caller pinned cfg.Kubeconfig or cfg.Context, build
//     from those.
//  2. Otherwise, try in-cluster (SA token + CA + KUBERNETES_SERVICE_HOST).
//  3. Fall back to the default kubeconfig location ($KUBECONFIG, then
//     ~/.kube/config).
//
// Namespace, first match wins:
//
//  1. cfg.Namespace
//  2. /var/run/secrets/kubernetes.io/serviceaccount/namespace
//  3. kubeconfig current-context's namespace
//  4. "default"
func newK8sClient(cfg YKustomizeInClusterConfig) (*k8sClient, error) {
	explicit := cfg.Kubeconfig != "" || cfg.Context != ""
	if !explicit {
		if c, err := newInClusterClient(); err == nil {
			c.namespace = resolveNamespace(cfg.Namespace, c.namespace)
			return c, nil
		}
	}
	c, kcNS, err := newKubeconfigClient(cfg.Kubeconfig, cfg.Context)
	if err != nil {
		return nil, err
	}
	c.namespace = resolveNamespace(cfg.Namespace, kcNS)
	return c, nil
}

// resolveNamespace applies the documented fallback chain. Empty
// inputs skip to the next source; "default" is the floor.
func resolveNamespace(explicit, kubeconfigNS string) string {
	if explicit != "" {
		return explicit
	}
	if data, err := os.ReadFile(saNamespacePath); err == nil {
		if ns := strings.TrimSpace(string(data)); ns != "" {
			return ns
		}
	}
	if kubeconfigNS != "" {
		return kubeconfigNS
	}
	return "default"
}

// newInClusterClient builds a client from the SA mount + the
// KUBERNETES_SERVICE_HOST/PORT env. Returns an error if any of
// the required pieces are missing -- callers fall back to
// kubeconfig in that case.
func newInClusterClient() (*k8sClient, error) {
	host := os.Getenv("KUBERNETES_SERVICE_HOST")
	port := os.Getenv("KUBERNETES_SERVICE_PORT")
	if host == "" || port == "" {
		return nil, fmt.Errorf("KUBERNETES_SERVICE_HOST/PORT not set")
	}
	token, err := os.ReadFile(saTokenPath)
	if err != nil {
		return nil, fmt.Errorf("read SA token: %w", err)
	}
	caPEM, err := os.ReadFile(saCAPath)
	if err != nil {
		return nil, fmt.Errorf("read SA CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("parse SA CA: no certs found")
	}
	hc := &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
		},
	}
	saNS, _ := os.ReadFile(saNamespacePath)
	return &k8sClient{
		server:     "https://" + net.JoinHostPort(host, port),
		httpClient: hc,
		bearer:     strings.TrimSpace(string(token)),
		namespace:  strings.TrimSpace(string(saNS)),
	}, nil
}

// kubeconfigYAML is the subset of the kubeconfig schema we need.
// Carries enough to resolve server URL, TLS verification material,
// auth (token or client cert), and the current-context namespace
// fallback. Other fields (preferences, extensions) are ignored.
type kubeconfigYAML struct {
	CurrentContext string `json:"current-context,omitempty"`
	Clusters       []struct {
		Name    string `json:"name"`
		Cluster struct {
			Server                   string `json:"server"`
			CertificateAuthority     string `json:"certificate-authority,omitempty"`
			CertificateAuthorityData string `json:"certificate-authority-data,omitempty"`
			InsecureSkipTLSVerify    bool   `json:"insecure-skip-tls-verify,omitempty"`
		} `json:"cluster"`
	} `json:"clusters"`
	Contexts []struct {
		Name    string `json:"name"`
		Context struct {
			Cluster   string `json:"cluster"`
			User      string `json:"user"`
			Namespace string `json:"namespace,omitempty"`
		} `json:"context"`
	} `json:"contexts"`
	Users []struct {
		Name string `json:"name"`
		User struct {
			Token                 string `json:"token,omitempty"`
			TokenFile             string `json:"tokenFile,omitempty"`
			ClientCertificate     string `json:"client-certificate,omitempty"`
			ClientCertificateData string `json:"client-certificate-data,omitempty"`
			ClientKey             string `json:"client-key,omitempty"`
			ClientKeyData         string `json:"client-key-data,omitempty"`
		} `json:"user"`
	} `json:"users"`
}

// newKubeconfigClient parses the kubeconfig at path (or the default
// location when path is empty), picks the named context (or
// current-context when contextName is empty), and returns a client
// pointed at the cluster behind that context.
//
// Returns the namespace declared on the chosen context as the
// kubeconfig-namespace fallback for the caller's resolution chain.
func newKubeconfigClient(path, contextName string) (*k8sClient, string, error) {
	resolved := path
	if resolved == "" {
		if env := os.Getenv("KUBECONFIG"); env != "" {
			// Take the first entry for KUBECONFIG=a:b. clientcmd
			// merges across all entries; this code path only
			// handles single-file kubeconfigs because that's all
			// y-cluster's existing tests exercise.
			resolved = strings.SplitN(env, string(os.PathListSeparator), 2)[0]
		}
	}
	if resolved == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, "", fmt.Errorf("locate home: %w", err)
		}
		resolved = filepath.Join(home, ".kube", "config")
	}
	data, err := os.ReadFile(resolved)
	if err != nil {
		return nil, "", fmt.Errorf("read kubeconfig %s: %w", resolved, err)
	}
	var kc kubeconfigYAML
	if err := yaml.Unmarshal(data, &kc); err != nil {
		return nil, "", fmt.Errorf("parse kubeconfig %s: %w", resolved, err)
	}

	ctxName := contextName
	if ctxName == "" {
		ctxName = kc.CurrentContext
	}
	if ctxName == "" {
		return nil, "", fmt.Errorf("kubeconfig %s: no current-context and no override given", resolved)
	}

	var clusterName, userName, ns string
	for _, c := range kc.Contexts {
		if c.Name == ctxName {
			clusterName = c.Context.Cluster
			userName = c.Context.User
			ns = c.Context.Namespace
			break
		}
	}
	if clusterName == "" {
		return nil, "", fmt.Errorf("kubeconfig %s: context %q not found", resolved, ctxName)
	}

	var server string
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
	for _, cl := range kc.Clusters {
		if cl.Name != clusterName {
			continue
		}
		server = cl.Cluster.Server
		if cl.Cluster.InsecureSkipTLSVerify {
			tlsCfg.InsecureSkipVerify = true
		} else if pool, err := loadCAPool(cl.Cluster.CertificateAuthority, cl.Cluster.CertificateAuthorityData, resolved); err != nil {
			return nil, "", fmt.Errorf("kubeconfig %s: cluster %q CA: %w", resolved, clusterName, err)
		} else if pool != nil {
			tlsCfg.RootCAs = pool
		}
		break
	}
	if server == "" {
		return nil, "", fmt.Errorf("kubeconfig %s: cluster %q not found or has no server", resolved, clusterName)
	}

	var bearer string
	for _, u := range kc.Users {
		if u.Name != userName {
			continue
		}
		if u.User.Token != "" {
			bearer = u.User.Token
		} else if u.User.TokenFile != "" {
			data, err := os.ReadFile(absPath(u.User.TokenFile, resolved))
			if err != nil {
				return nil, "", fmt.Errorf("read tokenFile %s: %w", u.User.TokenFile, err)
			}
			bearer = strings.TrimSpace(string(data))
		}
		certPEM, keyPEM, err := loadClientKeypair(
			u.User.ClientCertificate, u.User.ClientCertificateData,
			u.User.ClientKey, u.User.ClientKeyData,
			resolved,
		)
		if err != nil {
			return nil, "", fmt.Errorf("kubeconfig %s: user %q cert: %w", resolved, userName, err)
		}
		if certPEM != nil && keyPEM != nil {
			pair, err := tls.X509KeyPair(certPEM, keyPEM)
			if err != nil {
				return nil, "", fmt.Errorf("kubeconfig %s: user %q keypair: %w", resolved, userName, err)
			}
			tlsCfg.Certificates = []tls.Certificate{pair}
		}
		break
	}

	hc := &http.Client{
		Timeout:   30 * time.Second,
		Transport: &http.Transport{TLSClientConfig: tlsCfg},
	}
	return &k8sClient{
		server:     server,
		httpClient: hc,
		bearer:     bearer,
	}, ns, nil
}

// loadCAPool returns an x509 pool from either a path or a base64
// inline blob; nil + nil err means "no CA configured" (caller falls
// back to system roots).
func loadCAPool(path, b64 string, kubeconfigPath string) (*x509.CertPool, error) {
	if path == "" && b64 == "" {
		return nil, nil
	}
	var pem []byte
	if b64 != "" {
		decoded, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			return nil, fmt.Errorf("decode CA data: %w", err)
		}
		pem = decoded
	} else {
		data, err := os.ReadFile(absPath(path, kubeconfigPath))
		if err != nil {
			return nil, fmt.Errorf("read CA file %s: %w", path, err)
		}
		pem = data
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("no certs in CA")
	}
	return pool, nil
}

// loadClientKeypair returns the cert and key PEM bytes from
// either inline-data or file paths. Returns nil, nil, nil when
// neither is configured (the user has token-only auth).
func loadClientKeypair(certPath, certData, keyPath, keyData, kubeconfigPath string) ([]byte, []byte, error) {
	cert, err := loadOneOf(certPath, certData, kubeconfigPath)
	if err != nil {
		return nil, nil, fmt.Errorf("cert: %w", err)
	}
	key, err := loadOneOf(keyPath, keyData, kubeconfigPath)
	if err != nil {
		return nil, nil, fmt.Errorf("key: %w", err)
	}
	if cert == nil || key == nil {
		return nil, nil, nil
	}
	return cert, key, nil
}

func loadOneOf(path, b64, kubeconfigPath string) ([]byte, error) {
	if b64 != "" {
		return base64.StdEncoding.DecodeString(b64)
	}
	if path != "" {
		return os.ReadFile(absPath(path, kubeconfigPath))
	}
	return nil, nil
}

// absPath resolves a kubeconfig-relative path against the
// kubeconfig's directory, matching clientcmd's behaviour for
// relative file references in the kubeconfig schema.
func absPath(p, kubeconfigPath string) string {
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(filepath.Dir(kubeconfigPath), p)
}
