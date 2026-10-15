package k3s

import (
	"reflect"
	"sort"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// config.yaml is the flags qemu passed on the command line before
// the server options moved there, key by key.
func TestCiliumServerSettings_AreServerFlagsInConfigForm(t *testing.T) {
	s, err := CiliumServerSettings([]string{"192.0.2.10", "node.example.test"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	flags := append(append([]string{}, CiliumFlags...), "--tls-san=192.0.2.10", "--tls-san=node.example.test")
	want := strings.Fields(ServerFlags(flags...))
	sort.Strings(want)
	got := s.Flags()
	sort.Strings(got)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("as flags:\n got %v\nwant %v", got, want)
	}

	body, err := s.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	var parsed map[string]any
	if err := yaml.Unmarshal(body, &parsed); err != nil {
		t.Fatal(err)
	}
	wantParsed := map[string]any{
		"write-kubeconfig-mode":  "644",
		"disable":                []any{"traefik", "local-storage", "gateway-api-crd"},
		"flannel-backend":        "none",
		"disable-network-policy": true,
		"tls-san":                []any{"192.0.2.10", "node.example.test"},
	}
	if !reflect.DeepEqual(parsed, wantParsed) {
		t.Errorf("config.yaml:\n%s\nparsed %v", body, parsed)
	}
	// k3s reads "644" as text; unquoted it would be a YAML number.
	if !strings.Contains(string(body), `write-kubeconfig-mode: "644"`) {
		t.Errorf("mode not quoted:\n%s", body)
	}
}

func TestCiliumServerSettings_Additions(t *testing.T) {
	s, err := CiliumServerSettings([]string{"192.0.2.10"}, map[string]any{
		"secrets-encryption":    true,
		"write-kubeconfig-mode": "0600",
		"disable":               []any{"metrics-server", "traefik"},
		"tls-san":               "192.0.2.10",
		"kube-apiserver-arg":    []any{"audit-log-maxage=30"},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := ServerSettings{
		"write-kubeconfig-mode":  "0600",
		"disable":                []string{"traefik", "local-storage", "gateway-api-crd", "metrics-server"},
		"flannel-backend":        "none",
		"disable-network-policy": true,
		"tls-san":                []string{"192.0.2.10"},
		"secrets-encryption":     true,
		"kube-apiserver-arg":     []string{"audit-log-maxage=30"},
	}
	if !reflect.DeepEqual(s, want) {
		t.Errorf("merged:\n got %v\nwant %v", s, want)
	}
	// The defaults are not shared state between calls.
	again, _ := CiliumServerSettings(nil, nil)
	if !reflect.DeepEqual(again["disable"], []string{"traefik", "local-storage", "gateway-api-crd"}) {
		t.Errorf("defaults changed by an earlier merge: %v", again["disable"])
	}
}

func TestCiliumServerSettings_Refused(t *testing.T) {
	for name, add := range map[string]map[string]any{
		"flannel":         {"flannel-backend": "vxlan"},
		"same value":      {"disable-network-policy": true},
		"data dir":        {"data-dir": "/srv/k3s"},
		"kubeconfig path": {"write-kubeconfig": "/root/k3s.yaml"},
		"apiserver port":  {"https-listen-port": "16443"},
		"number":          {"write-kubeconfig-mode": float64(600)},
		"nested":          {"kubelet": map[string]any{"a": "b"}},
		"list of numbers": {"node-label": []any{1}},
		"dashes":          {"--secrets-encryption": true},
		"bool as list":    {"disable": true},
	} {
		if _, err := CiliumServerSettings(nil, add); err == nil {
			t.Errorf("%s: %v accepted", name, add)
		}
		if err := ValidateAdditions(add); err == nil {
			t.Errorf("%s: ValidateAdditions accepted %v", name, add)
		}
	}
}

func TestSettingsFromFlags(t *testing.T) {
	got, err := SettingsFromFlags([]string{"--a=1", "--b", "--a=2", "--disable=x", "--c=3"})
	if err != nil {
		t.Fatal(err)
	}
	want := ServerSettings{"a": []string{"1", "2"}, "b": true, "disable": []string{"x"}, "c": "3"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v", got)
	}
	if _, err := SettingsFromFlags([]string{"server"}); err == nil {
		t.Error("a non-flag word is refused")
	}
	if got := (ServerSettings{"b": false}).Flags(); !reflect.DeepEqual(got, []string{"--b=false"}) {
		t.Errorf("false switch: %v", got)
	}
}
