// Package registries marshals a config.Registries into the
// k3s /etc/rancher/k3s/registries.yaml on-disk shape and helps
// providers stage it on the node before k3s starts.
//
// The qemu and docker provisioners both call Marshal; the
// per-provider write path differs (SSH+tee for qemu, the moby
// CopyToContainer API for docker) but the produced bytes and
// the destination path are identical.
package registries

import (
	"archive/tar"
	"bytes"
	"fmt"
	"time"

	"sigs.k8s.io/yaml"

	"github.com/Yolean/y-cluster/pkg/provision/config"
)

// Path is where k3s expects to read the registries file. Both
// providers write to this absolute path on the node.
const Path = "/etc/rancher/k3s/registries.yaml"

// Marshal renders r as a k3s-compatible registries.yaml. Returns
// the YAML bytes ready to write to the node, or an error if
// marshalling fails.
//
// We use sigs.k8s.io/yaml so the byte layout matches what k3s
// itself reads: indent-two-spaces, no document marker, lowercase
// keys driven by the struct tags in pkg/provision/config.
func Marshal(r config.Registries) ([]byte, error) {
	if r.Empty() {
		return nil, nil
	}
	out, err := yaml.Marshal(r)
	if err != nil {
		return nil, fmt.Errorf("marshal registries: %w", err)
	}
	return out, nil
}

// Tar wraps body in a tar archive whose layout matches the
// directories the docker provider's CopyToContainer call needs.
// The destination passed to CopyToContainer is "/" and the tar
// contains
//
//	etc/
//	etc/rancher/
//	etc/rancher/k3s/
//	etc/rancher/k3s/registries.yaml
//
// so the file lands at /etc/rancher/k3s/registries.yaml on
// extraction. Directory entries are required because the rancher/k3s
// container image doesn't necessarily ship those mkdir's already
// applied -- and CopyToContainer doesn't auto-mkdir.
func Tar(body []byte) ([]byte, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	now := time.Unix(0, 0)
	dirs := []string{"etc", "etc/rancher", "etc/rancher/k3s"}
	for _, d := range dirs {
		if err := tw.WriteHeader(&tar.Header{
			Name:     d + "/",
			Mode:     0o755,
			Typeflag: tar.TypeDir,
			ModTime:  now,
		}); err != nil {
			return nil, fmt.Errorf("tar dir %s: %w", d, err)
		}
	}
	if err := tw.WriteHeader(&tar.Header{
		Name:     "etc/rancher/k3s/registries.yaml",
		Mode:     0o600,
		Size:     int64(len(body)),
		Typeflag: tar.TypeReg,
		ModTime:  now,
	}); err != nil {
		return nil, fmt.Errorf("tar header: %w", err)
	}
	if _, err := tw.Write(body); err != nil {
		return nil, fmt.Errorf("tar body: %w", err)
	}
	if err := tw.Close(); err != nil {
		return nil, fmt.Errorf("tar close: %w", err)
	}
	return buf.Bytes(), nil
}
