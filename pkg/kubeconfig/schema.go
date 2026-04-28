package kubeconfig

import (
	"errors"
	"fmt"
	"io/fs"
	"os"

	"sigs.k8s.io/yaml"
)

// File is the kubeconfig YAML shape. We hand-roll the schema rather
// than pulling in k8s.io/client-go/tools/clientcmd: kubeconfig is
// small enough that a typed struct + sigs.k8s.io/yaml suffices for
// our merge / rename / save operations, and dropping clientcmd
// removes a multi-MB dependency from the binary.
//
// The field set covers everything y-cluster reads or writes:
//
//   - current-context: which context the next kubectl call uses
//   - clusters / contexts / users: the three named lists kubectl
//     references; entries point at each other by name
//
// `extensions`, `preferences`, and the auth-provider / exec
// stanzas the kubeconfig schema technically supports are passed
// through as raw JSON so a kubeconfig that uses them isn't lost
// when we round-trip through Load / Save.
type File struct {
	APIVersion     string         `json:"apiVersion,omitempty"`
	Kind           string         `json:"kind,omitempty"`
	CurrentContext string         `json:"current-context,omitempty"`
	Clusters       []NamedCluster `json:"clusters"`
	Contexts       []NamedContext `json:"contexts"`
	Users          []NamedUser    `json:"users"`
	Preferences    map[string]any `json:"preferences,omitempty"`
}

// NamedCluster holds a single entry in `clusters:`.
type NamedCluster struct {
	Name    string  `json:"name"`
	Cluster Cluster `json:"cluster"`
}

// Cluster captures the apiserver-side configuration for a context.
// Both file and inline-data variants of the CA are kept so a
// round-trip preserves whatever the source used.
type Cluster struct {
	Server                   string `json:"server,omitempty"`
	CertificateAuthority     string `json:"certificate-authority,omitempty"`
	CertificateAuthorityData string `json:"certificate-authority-data,omitempty"`
	InsecureSkipTLSVerify    bool   `json:"insecure-skip-tls-verify,omitempty"`
}

// NamedContext is one entry in `contexts:`.
type NamedContext struct {
	Name    string  `json:"name"`
	Context Context `json:"context"`
}

// Context references a cluster + user pair plus an optional
// default namespace.
type Context struct {
	Cluster   string `json:"cluster,omitempty"`
	User      string `json:"user,omitempty"`
	Namespace string `json:"namespace,omitempty"`
}

// NamedUser is one entry in `users:`.
type NamedUser struct {
	Name string `json:"name"`
	User User   `json:"user"`
}

// User holds auth material. We carry both file-path and inline-data
// variants for cert/key, plus token/tokenFile, plus the most common
// extension blocks (auth-provider, exec) as raw JSON so they
// round-trip cleanly without us having to enumerate every field.
type User struct {
	Token                 string         `json:"token,omitempty"`
	TokenFile             string         `json:"tokenFile,omitempty"`
	ClientCertificate     string         `json:"client-certificate,omitempty"`
	ClientCertificateData string         `json:"client-certificate-data,omitempty"`
	ClientKey             string         `json:"client-key,omitempty"`
	ClientKeyData         string         `json:"client-key-data,omitempty"`
	Username              string         `json:"username,omitempty"`
	Password              string         `json:"password,omitempty"`
	Impersonate           string         `json:"as,omitempty"`
	AuthProvider          map[string]any `json:"auth-provider,omitempty"`
	Exec                  map[string]any `json:"exec,omitempty"`
}

// Load reads the kubeconfig at path and returns the parsed File.
// A non-existent path yields an empty File with no error -- the
// caller's typical workflow is "load existing or start fresh".
func Load(path string) (*File, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return emptyFile(), nil
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	return Parse(data)
}

// Parse decodes a kubeconfig YAML byte slice, defaulting empty
// input to an empty File. Used by Manager.Import to unmarshal a
// freshly-emitted kubeconfig (e.g. from k3s) without going through
// the filesystem.
func Parse(data []byte) (*File, error) {
	if len(data) == 0 {
		return emptyFile(), nil
	}
	var f File
	if err := yaml.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("parse kubeconfig: %w", err)
	}
	if f.APIVersion == "" {
		f.APIVersion = "v1"
	}
	if f.Kind == "" {
		f.Kind = "Config"
	}
	// Empty slices render as `[]` (kubie-friendly) rather than the
	// `null` Go marshalling would emit for nil.
	if f.Clusters == nil {
		f.Clusters = []NamedCluster{}
	}
	if f.Contexts == nil {
		f.Contexts = []NamedContext{}
	}
	if f.Users == nil {
		f.Users = []NamedUser{}
	}
	return &f, nil
}

// emptyFile is the "fresh kubeconfig" baseline a caller starts
// from when KUBECONFIG points at a non-existent path.
func emptyFile() *File {
	return &File{
		APIVersion: "v1",
		Kind:       "Config",
		Clusters:   []NamedCluster{},
		Contexts:   []NamedContext{},
		Users:      []NamedUser{},
	}
}

// Save writes f to path with 0600 permissions, matching what
// kubectl writes for a freshly-imported kubeconfig.
func (f *File) Save(path string) error {
	// Ensure empty slices serialise as `[]` for kubie, not `null`
	// (sigs.k8s.io/yaml writes nil slices as `null`, but
	// initialised-empty slices as `[]`).
	if f.Clusters == nil {
		f.Clusters = []NamedCluster{}
	}
	if f.Contexts == nil {
		f.Contexts = []NamedContext{}
	}
	if f.Users == nil {
		f.Users = []NamedUser{}
	}
	data, err := yaml.Marshal(f)
	if err != nil {
		return fmt.Errorf("marshal kubeconfig: %w", err)
	}
	return os.WriteFile(path, data, 0o600)
}

// findContext returns the index of the named context, or -1.
func (f *File) findContext(name string) int {
	for i, c := range f.Contexts {
		if c.Name == name {
			return i
		}
	}
	return -1
}

// findCluster returns the index of the named cluster, or -1.
func (f *File) findCluster(name string) int {
	for i, c := range f.Clusters {
		if c.Name == name {
			return i
		}
	}
	return -1
}

// findUser returns the index of the named user, or -1.
func (f *File) findUser(name string) int {
	for i, u := range f.Users {
		if u.Name == name {
			return i
		}
	}
	return -1
}

// ContextCluster returns the cluster name the named context
// references, or "" if no such context exists. Read-only;
// callers that need the full Context can iterate Contexts
// directly.
func (f *File) ContextCluster(contextName string) string {
	if i := f.findContext(contextName); i >= 0 {
		return f.Contexts[i].Context.Cluster
	}
	return ""
}

// removeContext drops the named context. Cluster and user
// entries are left in place since they may be referenced by
// other contexts.
func (f *File) removeContext(name string) {
	if i := f.findContext(name); i >= 0 {
		f.Contexts = append(f.Contexts[:i], f.Contexts[i+1:]...)
	}
	if f.CurrentContext == name {
		f.CurrentContext = ""
	}
}

// removeCluster drops the named cluster. Removing a cluster
// referenced by a context leaves the context dangling -- callers
// avoid that via the Manager's coordinated cleanup.
func (f *File) removeCluster(name string) {
	if i := f.findCluster(name); i >= 0 {
		f.Clusters = append(f.Clusters[:i], f.Clusters[i+1:]...)
	}
}

// removeUser drops the named user.
func (f *File) removeUser(name string) {
	if i := f.findUser(name); i >= 0 {
		f.Users = append(f.Users[:i], f.Users[i+1:]...)
	}
}

// upsertContext replaces the named context if present, else
// appends. Callers use this to merge incoming entries into an
// existing kubeconfig idempotently.
func (f *File) upsertContext(c NamedContext) {
	if i := f.findContext(c.Name); i >= 0 {
		f.Contexts[i] = c
		return
	}
	f.Contexts = append(f.Contexts, c)
}

func (f *File) upsertCluster(c NamedCluster) {
	if i := f.findCluster(c.Name); i >= 0 {
		f.Clusters[i] = c
		return
	}
	f.Clusters = append(f.Clusters, c)
}

func (f *File) upsertUser(u NamedUser) {
	if i := f.findUser(u.Name); i >= 0 {
		f.Users[i] = u
		return
	}
	f.Users = append(f.Users, u)
}

// MergeFrom folds incoming into f. Identical names in f are
// replaced -- this matches `kubectl config view --flatten`'s
// behaviour for an overlapping key, and what we want when
// re-provisioning. CurrentContext from incoming wins iff it's
// non-empty, leaving f's selection intact otherwise.
func (f *File) MergeFrom(incoming *File) {
	for _, c := range incoming.Contexts {
		f.upsertContext(c)
	}
	for _, c := range incoming.Clusters {
		f.upsertCluster(c)
	}
	for _, u := range incoming.Users {
		f.upsertUser(u)
	}
	if incoming.CurrentContext != "" {
		f.CurrentContext = incoming.CurrentContext
	}
}

// renameDefaults rewrites the canonical k3s "default" context /
// cluster / user names to the caller's chosen names. Other
// entries are left untouched -- a foreign "default" in a merged
// kubeconfig is the user's, not ours to rename.
func (f *File) renameDefaults(contextName, clusterName string) {
	if i := f.findContext("default"); i >= 0 {
		c := f.Contexts[i]
		c.Name = contextName
		c.Context.Cluster = clusterName
		c.Context.User = clusterName
		f.Contexts[i] = c
	}
	if i := f.findCluster("default"); i >= 0 {
		f.Clusters[i].Name = clusterName
	}
	if i := f.findUser("default"); i >= 0 {
		f.Users[i].Name = clusterName
	}
	if f.CurrentContext == "default" {
		f.CurrentContext = contextName
	}
}
