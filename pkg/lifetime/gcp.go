package lifetime

import (
	"fmt"
	"time"
)

// GCPFlags renders the `gcloud compute instances create` scheduling
// flags that enforce a cluster lifetime CLOUD-side. GCP measures
// max-run-duration from when the instance STARTS and then performs
// the termination action -- here DELETE -- with no dependency on the
// provisioning host or on the cluster staying up. That is exactly
// the "anchor to start, never host-bound" contract the appliance
// needs: the disk image carries only the duration, never an absolute
// deadline baked in at build time.
//
// Returns "" (no flags) when no budget is configured. The duration
// is normalized to integer seconds so gcloud's duration parser can
// never disagree with Go's time.ParseDuration.
func GCPFlags(maxRun string) (string, error) {
	if maxRun == "" || maxRun == "0" {
		return "", nil
	}
	d, err := time.ParseDuration(maxRun)
	if err != nil {
		return "", fmt.Errorf("lifetime maxRun %q is not a valid Go duration: %w", maxRun, err)
	}
	if d <= 0 {
		return "", fmt.Errorf("lifetime maxRun must be positive, got %q", maxRun)
	}
	secs := int(d / time.Second)
	return fmt.Sprintf("--max-run-duration=%ds --instance-termination-action=DELETE", secs), nil
}
