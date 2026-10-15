package k3s

import (
	"fmt"
	"sort"
	"strings"

	"sigs.k8s.io/yaml"
)

// ConfigPath is the k3s config file. k3s reads it, then every file
// in ConfigPath+".d/" in lexical order, and a key in a later file
// replaces the earlier value. y-cluster writes ConfigPath only, so the
// drop-in directory is left to whatever else configures the node.
const ConfigPath = "/etc/rancher/k3s/config.yaml"

// ServerSettings are k3s server options in config-file form: a key is
// a flag without its leading dashes, a repeatable flag is a list of
// strings, a flag without a value is true. The values are strings,
// booleans or lists of strings: numbers are refused, because YAML
// reads an unquoted 0600 as octal and k3s takes every value as text.
type ServerSettings map[string]any

// listSettings are the repeatable flags y-cluster itself sets. An
// addition's entries are appended to y-cluster's.
var listSettings = map[string]bool{"disable": true, "tls-san": true}

// requiredSettings are the keys an addition cannot set. The platform
// y-cluster installs depends on them: Cilium is the CNI and enforces
// NetworkPolicy, the airgap images and the portmap link sit under the
// default data dir, and the kubeconfig is read from KubeconfigPath with
// the apiserver at 127.0.0.1:6443.
var requiredSettings = []string{
	"flannel-backend",
	"disable-network-policy",
	"data-dir",
	"write-kubeconfig",
	"https-listen-port",
}

// CiliumServerSettings is the server config of a node that runs the
// platform y-cluster installs (Cilium as the CNI): the same options
// ServerFlags(CiliumFlags..., --tls-san=...) passes on the command
// line, with tlsSANs as tls-san, then additions merged over them.
//
// An addition overrides a default (write-kubeconfig-mode), appends to
// a list (disable, tls-san), adds a key y-cluster does not set, and
// is refused for a key in requiredSettings or a value that is not a
// string, a boolean or a list of strings.
func CiliumServerSettings(tlsSANs []string, additions map[string]any) (ServerSettings, error) {
	flags := append([]string{}, CiliumFlags...)
	for _, san := range tlsSANs {
		flags = append(flags, "--tls-san="+san)
	}
	base, err := SettingsFromFlags(strings.Fields(ServerFlags(flags...)))
	if err != nil {
		return nil, err
	}
	return mergeSettings(base, additions)
}

// SettingsFromFlags turns k3s server flags into config-file form.
// "--key=value" is a string, a bare "--key" is true, and a key that
// repeats, or is one of listSettings, is a list in flag order.
func SettingsFromFlags(flags []string) (ServerSettings, error) {
	s := ServerSettings{}
	for _, f := range flags {
		kv, ok := strings.CutPrefix(f, "--")
		if !ok || kv == "" {
			return nil, fmt.Errorf("k3s flag %q does not start with --", f)
		}
		key, value, hasValue := strings.Cut(kv, "=")
		if !hasValue {
			s[key] = true
			continue
		}
		prev, seen := s[key]
		switch {
		case listSettings[key] && !seen:
			s[key] = []string{value}
		case !seen:
			s[key] = value
		default:
			switch p := prev.(type) {
			case []string:
				s[key] = append(p, value)
			case string:
				s[key] = []string{p, value}
			default:
				return nil, fmt.Errorf("k3s flag --%s is both a switch and has a value", key)
			}
		}
	}
	return s, nil
}

// Flags is the command-line form of s, sorted by key with list
// entries in order: what k3s would be started with if s were passed
// as flags instead of in ConfigPath.
func (s ServerSettings) Flags() []string {
	var flags []string
	for _, key := range s.keys() {
		switch v := s[key].(type) {
		case bool:
			if v {
				flags = append(flags, "--"+key)
			} else {
				flags = append(flags, "--"+key+"=false")
			}
		case string:
			flags = append(flags, "--"+key+"="+v)
		case []string:
			for _, e := range v {
				flags = append(flags, "--"+key+"="+e)
			}
		}
	}
	return flags
}

// Marshal renders s as the content of ConfigPath.
func (s ServerSettings) Marshal() ([]byte, error) {
	body, err := yaml.Marshal(map[string]any(s))
	if err != nil {
		return nil, fmt.Errorf("marshal k3s config: %w", err)
	}
	return append([]byte("# Written by y-cluster. Drop-ins in config.yaml.d/ are read after this file.\n"), body...), nil
}

func (s ServerSettings) keys() []string {
	keys := make([]string, 0, len(s))
	for k := range s {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// ValidateAdditions checks additions the way CiliumServerSettings
// merges them, without a base: config validation calls it so a
// conflict is reported when the config loads.
func ValidateAdditions(additions map[string]any) error {
	_, err := mergeSettings(ServerSettings{}, additions)
	return err
}

func mergeSettings(base ServerSettings, additions map[string]any) (ServerSettings, error) {
	out := ServerSettings{}
	for k, v := range base {
		if l, ok := v.([]string); ok {
			v = append([]string(nil), l...)
		}
		out[k] = v
	}
	keys := make([]string, 0, len(additions))
	for k := range additions {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if strings.HasPrefix(key, "-") || key == "" || strings.Contains(key, "=") {
			return nil, fmt.Errorf("k3s.serverConfig key %q: write the flag name without dashes, e.g. secrets-encryption", key)
		}
		for _, r := range requiredSettings {
			if key == r {
				return nil, fmt.Errorf("k3s.serverConfig cannot set %s: y-cluster's platform depends on its value", key)
			}
		}
		v, err := normalizeSetting(key, additions[key])
		if err != nil {
			return nil, err
		}
		if listSettings[key] {
			var l []string
			switch t := v.(type) {
			case []string:
				l = t
			case string:
				l = []string{t}
			default:
				return nil, fmt.Errorf("k3s.serverConfig %s is a list of strings", key)
			}
			prev, _ := out[key].([]string)
			out[key] = appendNew(prev, l)
			continue
		}
		out[key] = v
	}
	return out, nil
}

// normalizeSetting accepts what a YAML or JSON decoder hands over for
// a string, a boolean or a list of strings.
func normalizeSetting(key string, v any) (any, error) {
	switch t := v.(type) {
	case string, bool:
		return t, nil
	case []string:
		return append([]string(nil), t...), nil
	case []any:
		l := make([]string, 0, len(t))
		for i, e := range t {
			s, ok := e.(string)
			if !ok {
				return nil, fmt.Errorf("k3s.serverConfig %s[%d] is %T; list entries must be strings (quote numbers)", key, i, e)
			}
			l = append(l, s)
		}
		return l, nil
	default:
		return nil, fmt.Errorf("k3s.serverConfig %s is %T; values are strings, booleans or lists of strings (quote numbers such as \"0600\")", key, v)
	}
}

func appendNew(list, add []string) []string {
	have := map[string]bool{}
	for _, e := range list {
		have[e] = true
	}
	for _, e := range add {
		if !have[e] {
			list = append(list, e)
			have[e] = true
		}
	}
	return list
}
