package images

import (
	"reflect"
	"strings"
	"testing"
)

func mustList(t *testing.T, body string) []string {
	t.Helper()
	got, err := ListYAML(strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestListYAML_DeploymentAndInitContainers(t *testing.T) {
	got := mustList(t, `apiVersion: apps/v1
kind: Deployment
metadata:
  name: app
spec:
  template:
    spec:
      initContainers:
      - name: init
        image: busybox:1.36
      containers:
      - name: app
        image: nginx:1.27
      - name: side
        image: nginx:1.27
`)
	want := []string{"busybox:1.36", "nginx:1.27"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestListYAML_DedupAcrossKinds(t *testing.T) {
	got := mustList(t, `apiVersion: v1
kind: Pod
metadata:
  name: p
spec:
  containers:
  - name: c
    image: shared:v1
---
apiVersion: batch/v1
kind: Job
metadata:
  name: j
spec:
  template:
    spec:
      containers:
      - name: w
        image: shared:v1
      restartPolicy: Never
`)
	want := []string{"shared:v1"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestListYAML_CronJobNested(t *testing.T) {
	got := mustList(t, `apiVersion: batch/v1
kind: CronJob
metadata:
  name: nightly
spec:
  schedule: "0 0 * * *"
  jobTemplate:
    spec:
      template:
        spec:
          containers:
          - name: r
            image: registry.example.com/runner:abc123
          restartPolicy: OnFailure
`)
	want := []string{"registry.example.com/runner:abc123"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

// TestListYAML_NonPodKindsIgnored guards against accidentally
// pulling out images from places that aren't a real PodSpec —
// e.g. a ConfigMap that mentions an image name in its body.
func TestListYAML_NonPodKindsIgnored(t *testing.T) {
	got := mustList(t, `apiVersion: v1
kind: ConfigMap
metadata:
  name: meta
data:
  note: |
    image: nginx:1.0  # this is just yaml-in-yaml, not a real PodSpec
---
apiVersion: v1
kind: Service
metadata:
  name: db
spec:
  ports:
  - port: 5432
`)
	if len(got) != 0 {
		t.Fatalf("expected no images, got %v", got)
	}
}

func TestListYAML_EmptyImageSkipped(t *testing.T) {
	got := mustList(t, `apiVersion: v1
kind: Pod
metadata:
  name: p
spec:
  containers:
  - name: c
    image: ""
  - name: c2
    image: real:v1
`)
	want := []string{"real:v1"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

// TestListYAML_LeadingSeparator covers the kustomize quirk of
// emitting `---\n` as the very first line of the stream.
func TestListYAML_LeadingSeparator(t *testing.T) {
	got := mustList(t, `---
apiVersion: v1
kind: Pod
metadata:
  name: p
spec:
  containers:
  - name: c
    image: nginx:1
`)
	want := []string{"nginx:1"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestListYAML_MalformedYAMLErrors(t *testing.T) {
	_, err := ListYAML(strings.NewReader("kind: Pod\nspec:\n  containers: [oops"))
	if err == nil {
		t.Fatal("expected YAML parse error")
	}
}
