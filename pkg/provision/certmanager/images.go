package certmanager

import (
	"fmt"
	"io"

	"github.com/Yolean/y-cluster/pkg/images"
)

// Images lists the container images a cert-manager.yaml stream
// references, deduplicated and in stream order, for mirroring or
// pre-caching.
func Images(r io.Reader) ([]string, error) {
	refs, err := images.ListYAML(r)
	if err != nil {
		return nil, fmt.Errorf("list images from cert-manager.yaml: %w", err)
	}
	return refs, nil
}
