package glesys

import (
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/Yolean/y-cluster/pkg/provision/devcert"
)

// The default Gateway is what puts envoy on the node's 80 and 443;
// its listeners, hostname and certificate reference are the contract
// consumer HTTPRoutes and the example workload rely on.
func TestDefaultGatewayManifest(t *testing.T) {
	cn, dns, ips := certSubjects("qa-glesys", []string{"203.0.113.10", "203.0.113.11"})
	certPEM, keyPEM, err := devcert.GenerateSelfSigned(cn, dns, ips)
	if err != nil {
		t.Fatal(err)
	}
	docs := map[string]map[string]any{}
	for _, raw := range strings.Split(string(defaultGatewayManifest("qa-glesys", "y-cluster", certPEM, keyPEM)), "\n---\n") {
		if strings.TrimSpace(raw) == "" {
			continue
		}
		var d map[string]any
		if err := yaml.Unmarshal([]byte(raw), &d); err != nil {
			t.Fatalf("parse: %v\n%s", err, raw)
		}
		docs[d["kind"].(string)] = d
	}
	for _, kind := range []string{"Namespace", "Secret", "Gateway", "ClientTrafficPolicy"} {
		if docs[kind] == nil {
			t.Fatalf("no %s document", kind)
		}
	}

	spec := docs["Gateway"]["spec"].(map[string]any)
	if spec["gatewayClassName"] != "y-cluster" {
		t.Errorf("gatewayClassName = %v", spec["gatewayClassName"])
	}
	listeners := spec["listeners"].([]any)
	if len(listeners) != 2 {
		t.Fatalf("want http and https listeners, got %d", len(listeners))
	}
	for _, l := range listeners {
		m := l.(map[string]any)
		if m["hostname"] != "*.qa-glesys.local.test" {
			t.Errorf("listener %v hostname = %v", m["name"], m["hostname"])
		}
		switch m["name"] {
		case "http":
			if m["port"] != float64(80) || m["protocol"] != "HTTP" {
				t.Errorf("http listener: %v", m)
			}
		case "https":
			if m["port"] != float64(443) || m["protocol"] != "HTTPS" {
				t.Errorf("https listener: %v", m)
			}
			tls := m["tls"].(map[string]any)
			ref := tls["certificateRefs"].([]any)[0].(map[string]any)
			if tls["mode"] != "Terminate" || ref["name"] != tlsSecretName {
				t.Errorf("https tls: %v", tls)
			}
		default:
			t.Errorf("unexpected listener %v", m["name"])
		}
	}

	// TLS 1.3 at the edge is a requirement row; the policy targets
	// the Gateway by name.
	ctp := docs["ClientTrafficPolicy"]["spec"].(map[string]any)
	if got := ctp["tls"].(map[string]any)["minVersion"]; got != "1.3" {
		t.Errorf("tls minVersion = %v, want 1.3", got)
	}
	if ref := ctp["targetRefs"].([]any)[0].(map[string]any); ref["kind"] != "Gateway" || ref["name"] != DefaultGatewayName {
		t.Errorf("policy target = %v", ref)
	}

	secret := docs["Secret"]
	if secret["type"] != "kubernetes.io/tls" {
		t.Errorf("secret type = %v", secret["type"])
	}
	data := secret["data"].(map[string]any)
	crt, err := base64.StdEncoding.DecodeString(data["tls.crt"].(string))
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(crt)
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	// What a client that resolves hello.<ctx>.local.test to the
	// node, or dials the address itself, has to see in the cert.
	if err := cert.VerifyHostname("hello.qa-glesys.local.test"); err != nil {
		t.Errorf("cert does not cover the wildcard: %v", err)
	}
	for _, ip := range []string{"203.0.113.10", "203.0.113.11"} {
		if err := cert.VerifyHostname(ip); err != nil {
			t.Errorf("cert does not cover node address %s: %v", ip, err)
		}
	}
	if _, err := base64.StdEncoding.DecodeString(data["tls.key"].(string)); err != nil {
		t.Errorf("tls.key: %v", err)
	}
}
