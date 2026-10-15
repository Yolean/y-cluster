package envoygateway

import (
	"bytes"
	"fmt"
	"regexp"
	"strings"

	"sigs.k8s.io/yaml"
)

// HelmHookAnnotation marks the upstream install.yaml's objects that
// Helm creates only on install and upgrade of the chart.
const HelmHookAnnotation = "helm.sh/hook"

// RenderInstall shapes the upstream install.yaml for apply:
//
//   - The controller container's resources.requests are set in the
//     Deployment itself (empty values leave upstream's). Applying the
//     upstream value and patching it afterwards would flip the pod
//     template, and roll the controller, on every re-apply.
//   - When liveControllerImage is the image of the manifest's
//     controller, the release is already installed and the chart's
//     hook Jobs are left out. The certgen Job is one: it deletes
//     itself 30s after it completes, so a re-apply would create and
//     run it again on every run, and a drift check would always find
//     it missing. On a first install or an upgrade (a different or no
//     live image) the Jobs are kept, as Helm runs them then.
func RenderInstall(installYAML []byte, cpuRequest, memRequest, liveControllerImage string) ([]byte, error) {
	docs := splitDocuments(installYAML)
	want, err := controllerImage(docs)
	if err != nil {
		return nil, err
	}
	dropHooks := liveControllerImage != "" && liveControllerImage == want
	out := make([][]byte, 0, len(docs))
	for _, d := range docs {
		switch {
		case dropHooks && isHookJob(d):
			continue
		case (cpuRequest != "" || memRequest != "") && isKind(d, "Deployment"):
			h, err := header(d)
			if err != nil {
				return nil, err
			}
			if h.Metadata.Name == DeploymentName && h.Metadata.Namespace == Namespace {
				if d, err = setControllerRequests(d, cpuRequest, memRequest); err != nil {
					return nil, err
				}
			}
		}
		out = append(out, d)
	}
	return joinDocuments(out), nil
}

// ControllerImage is the controller image the upstream install.yaml
// deploys.
func ControllerImage(installYAML []byte) (string, error) {
	return controllerImage(splitDocuments(installYAML))
}

var kindLine = regexp.MustCompile(`(?m)^kind:\s*(\S+)\s*$`)

// isKind reads the top-level kind without parsing the whole document,
// which matters for install.yaml's large CRDs.
func isKind(doc []byte, kind string) bool {
	m := kindLine.FindSubmatch(doc)
	return m != nil && string(m[1]) == kind
}

type docHeader struct {
	Kind     string `json:"kind"`
	Metadata struct {
		Name        string            `json:"name"`
		Namespace   string            `json:"namespace"`
		Annotations map[string]string `json:"annotations"`
	} `json:"metadata"`
}

func header(doc []byte) (docHeader, error) {
	var h docHeader
	if err := yaml.Unmarshal(doc, &h); err != nil {
		return h, fmt.Errorf("parse install.yaml document: %w", err)
	}
	return h, nil
}

func isHookJob(doc []byte) bool {
	if !isKind(doc, "Job") {
		return false
	}
	h, err := header(doc)
	return err == nil && h.Metadata.Annotations[HelmHookAnnotation] != ""
}

func controllerImage(docs [][]byte) (string, error) {
	for _, d := range docs {
		if !isKind(d, "Deployment") {
			continue
		}
		var dep struct {
			Metadata struct {
				Name      string `json:"name"`
				Namespace string `json:"namespace"`
			} `json:"metadata"`
			Spec struct {
				Template struct {
					Spec struct {
						Containers []struct {
							Name  string `json:"name"`
							Image string `json:"image"`
						} `json:"containers"`
					} `json:"spec"`
				} `json:"template"`
			} `json:"spec"`
		}
		if err := yaml.Unmarshal(d, &dep); err != nil {
			return "", fmt.Errorf("parse install.yaml Deployment: %w", err)
		}
		if dep.Metadata.Name != DeploymentName || dep.Metadata.Namespace != Namespace {
			continue
		}
		for _, c := range dep.Spec.Template.Spec.Containers {
			if c.Name == DeploymentName {
				return c.Image, nil
			}
		}
	}
	return "", fmt.Errorf("install.yaml has no %s/%s Deployment with a %s container", Namespace, DeploymentName, DeploymentName)
}

// setControllerRequests sets resources.requests of the controller
// container. Limits and every other field are left as upstream has
// them.
func setControllerRequests(doc []byte, cpu, mem string) ([]byte, error) {
	var obj map[string]any
	if err := yaml.Unmarshal(doc, &obj); err != nil {
		return nil, err
	}
	containers, _ := dig(obj, "spec", "template", "spec")["containers"].([]any)
	found := false
	for _, c := range containers {
		cm, ok := c.(map[string]any)
		if !ok || cm["name"] != DeploymentName {
			continue
		}
		found = true
		res, _ := cm["resources"].(map[string]any)
		if res == nil {
			res = map[string]any{}
			cm["resources"] = res
		}
		req, _ := res["requests"].(map[string]any)
		if req == nil {
			req = map[string]any{}
			res["requests"] = req
		}
		if cpu != "" {
			req["cpu"] = cpu
		}
		if mem != "" {
			req["memory"] = mem
		}
	}
	if !found {
		return nil, fmt.Errorf("install.yaml Deployment %s has no %s container", DeploymentName, DeploymentName)
	}
	return yaml.Marshal(obj)
}

func dig(obj map[string]any, keys ...string) map[string]any {
	for _, k := range keys {
		next, _ := obj[k].(map[string]any)
		if next == nil {
			return map[string]any{}
		}
		obj = next
	}
	return obj
}

// splitDocuments splits a YAML stream at its "---" separator lines
// and drops documents with nothing but comments and whitespace.
func splitDocuments(stream []byte) [][]byte {
	var docs [][]byte
	var cur bytes.Buffer
	flush := func() {
		if hasContent(cur.Bytes()) {
			docs = append(docs, append([]byte(nil), cur.Bytes()...))
		}
		cur.Reset()
	}
	for _, line := range strings.SplitAfter(string(stream), "\n") {
		if strings.TrimRight(line, " \t\r\n") == "---" {
			flush()
			continue
		}
		cur.WriteString(line)
	}
	flush()
	return docs
}

func hasContent(doc []byte) bool {
	for _, line := range strings.Split(string(doc), "\n") {
		t := strings.TrimSpace(line)
		if t != "" && !strings.HasPrefix(t, "#") {
			return true
		}
	}
	return false
}

func joinDocuments(docs [][]byte) []byte {
	var b bytes.Buffer
	for _, d := range docs {
		b.WriteString("---\n")
		b.Write(d)
		if len(d) > 0 && d[len(d)-1] != '\n' {
			b.WriteByte('\n')
		}
	}
	return b.Bytes()
}
