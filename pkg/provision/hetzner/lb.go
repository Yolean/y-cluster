package hetzner

import (
	"context"
	"fmt"
	"time"

	"github.com/hetznercloud/hcloud-go/v2/hcloud"
	"go.uber.org/zap"
)

// lbType is the cheapest tier (~5 EUR/mo, 10k connections) -- well
// over what a per-developer dev cluster needs. Phase 5 makes this
// configurable if a customer scenario ever lands here.
const lbType = "lb11"

// lbHealthCheckInterval / Timeout / Retries match Hetzner's UI
// defaults. Picked to keep an unhealthy node out of rotation
// within ~45s without pulling a momentarily-busy one.
const (
	lbHealthCheckInterval = 15 * time.Second
	lbHealthCheckTimeout  = 10 * time.Second
	lbHealthCheckRetries  = 3
)

// lbName composes the LB resource name from the lb-group. One LB
// per group, shared across every server provisioned with the same
// LBGroup. Per-developer dev clusters default to LBGroup = $USER.
func lbName(lbGroup string) string { return "y-cluster-" + lbGroup }

func intPtr(n int) *int                          { return &n }
func durationPtr(d time.Duration) *time.Duration { return &d }

// tcpService renders one Hetzner LB service: TCP listen=destination=port,
// with a TCP health check pinned to the same port. Hetzner's create-with-
// services payload requires health_check.port explicitly even for TCP
// services where it's "obviously" the destination port; the CLI adds
// that default but the API doesn't. Each service needs its OWN health-
// check struct -- sharing a pointer means both services would health-check
// the same port, which is wrong as soon as ports diverge.
func tcpService(port int) hcloud.LoadBalancerCreateOptsService {
	return hcloud.LoadBalancerCreateOptsService{
		Protocol:        hcloud.LoadBalancerServiceProtocolTCP,
		ListenPort:      intPtr(port),
		DestinationPort: intPtr(port),
		HealthCheck: &hcloud.LoadBalancerCreateOptsServiceHealthCheck{
			Protocol: hcloud.LoadBalancerServiceProtocolTCP,
			Port:     intPtr(port),
			Interval: durationPtr(lbHealthCheckInterval),
			Timeout:  durationPtr(lbHealthCheckTimeout),
			Retries:  intPtr(lbHealthCheckRetries),
		},
	}
}

// httpsService renders the LB's 443 service. Listen 443 (HTTPS,
// TLS terminated by Hetzner using the supplied certs; SNI selects
// which cert to present), destination 80 (klipper-lb on the
// backend's host:80 forwards to envoy-gateway as plaintext HTTP).
// Health check is TCP on 80 because the backend listens on 80 in
// HTTP only -- a tcp probe is enough for "klipper-lb is up".
//
// Multi-cert: each context contributes one cert covering its own
// FQDNs to the same 443 service. SNI handshake on the LB picks the
// matching cert; consumers' kustomize bases see no difference vs.
// in-cluster TLS termination.
func httpsService(certs []*hcloud.Certificate) hcloud.LoadBalancerCreateOptsService {
	return hcloud.LoadBalancerCreateOptsService{
		Protocol:        hcloud.LoadBalancerServiceProtocolHTTPS,
		ListenPort:      intPtr(443),
		DestinationPort: intPtr(80),
		HTTP: &hcloud.LoadBalancerCreateOptsServiceHTTP{
			Certificates: certs,
		},
		HealthCheck: &hcloud.LoadBalancerCreateOptsServiceHealthCheck{
			Protocol: hcloud.LoadBalancerServiceProtocolTCP,
			Port:     intPtr(80),
			Interval: durationPtr(lbHealthCheckInterval),
			Timeout:  durationPtr(lbHealthCheckTimeout),
			Retries:  intPtr(lbHealthCheckRetries),
		},
	}
}

// ensureLoadBalancer reconciles a Hetzner LB for cfg.LBGroup and
// returns it. Reuses an existing LB by name (so a second server in
// the same lb-group attaches to the existing LB instead of standing
// up a new one); creates with a label_selector target so any server
// already labelled with this lb-group is auto-attached, and any
// future server with the same labels joins automatically.
//
// Service shape on a fresh create:
//
//   - TCP 80 -> 80 (klipper-lb on backend host:80, plain HTTP).
//   - HTTPS 443 -> 80 with [firstCert]. The LB terminates TLS
//     using SNI to pick a cert; backend gets plaintext HTTP, so
//     envoy-gateway only needs to handle :80. firstCert is the
//     just-uploaded per-context cert -- a fresh LB always lands
//     with at least one cert because Hetzner rejects an empty
//     HTTPS service certs list.
//
// On reuse the existing LB's services are left alone; the caller
// follows up with attachCertificateToLB to add the new context's
// cert to the 443 service's cert list.
//
// Refuses to reuse an LB whose location differs from cfg.Location:
// label_selector targets resolve only within the LB's location, so
// a cross-location reuse would silently fail to pick up new
// servers. Forcing a clear error matches the "no silent leakage"
// stance.
func ensureLoadBalancer(ctx context.Context, hc *hcloud.Client, cfg lbConfig, firstCert *hcloud.Certificate, logger *zap.Logger) (*hcloud.LoadBalancer, error) {
	name := lbName(cfg.LBGroup)
	existing, _, err := hc.LoadBalancer.GetByName(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("describe LB %q: %w", name, err)
	}
	if existing != nil {
		if existing.Location != nil && existing.Location.Name != cfg.Location {
			return nil, fmt.Errorf("LB %q exists in location %q but cfg.Location=%q; pick a matching location or `hcloud load-balancer delete %s` first", name, existing.Location.Name, cfg.Location, name)
		}
		logger.Info("reusing Hetzner LB",
			zap.Int64("id", existing.ID),
			zap.String("name", existing.Name),
			zap.String("location", existing.Location.Name),
		)
		return existing, nil
	}
	if firstCert == nil {
		return nil, fmt.Errorf("create LB %q: firstCert is required (Hetzner HTTPS service rejects an empty cert list)", name)
	}
	logger.Info("creating Hetzner LB",
		zap.String("name", name),
		zap.String("type", lbType),
		zap.String("location", cfg.Location),
		zap.String("targetSelector", labelSelectorForGroup(cfg.LBGroup)),
		zap.Int64("firstCertID", firstCert.ID),
	)
	res, _, err := hc.LoadBalancer.Create(ctx, hcloud.LoadBalancerCreateOpts{
		Name:             name,
		LoadBalancerType: &hcloud.LoadBalancerType{Name: lbType},
		Location:         &hcloud.Location{Name: cfg.Location},
		Algorithm:        &hcloud.LoadBalancerAlgorithm{Type: hcloud.LoadBalancerAlgorithmTypeRoundRobin},
		Services: []hcloud.LoadBalancerCreateOptsService{
			tcpService(80),
			httpsService([]*hcloud.Certificate{firstCert}),
		},
		Targets: []hcloud.LoadBalancerCreateOptsTarget{
			{
				Type: hcloud.LoadBalancerTargetTypeLabelSelector,
				LabelSelector: hcloud.LoadBalancerCreateOptsTargetLabelSelector{
					Selector: labelSelectorForGroup(cfg.LBGroup),
				},
			},
		},
		Labels: map[string]string{
			"managed-by": "y-cluster",
			"lb-group":   cfg.LBGroup,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("create LB: %w", err)
	}
	if res.Action != nil {
		if err := waitForActions(ctx, hc, []*hcloud.Action{res.Action}); err != nil {
			return nil, fmt.Errorf("wait for LB create: %w", err)
		}
	}
	// Re-fetch to populate the newly assigned PublicNet.IPv4.
	lb, _, err := hc.LoadBalancer.GetByID(ctx, res.LoadBalancer.ID)
	if err != nil {
		return nil, fmt.Errorf("re-fetch LB %d: %w", res.LoadBalancer.ID, err)
	}
	logger.Info("Hetzner LB ready",
		zap.Int64("id", lb.ID),
		zap.String("ipv4", lb.PublicNet.IPv4.IP.String()),
	)
	return lb, nil
}

// uploadCertificate creates a Hetzner Certificate of type
// "uploaded" (we generate the bytes locally) tagged with our
// management labels. The Certificate name is the context, so a
// future operator looking at the Hetzner UI sees the 1:1 mapping
// to server / SSH key / state sidecar.
//
// Refuses when a same-named Certificate already exists: that
// would be a leftover from a stranded teardown, and silently
// reusing it would hide stale state. The operator is told to
// delete by name.
func uploadCertificate(ctx context.Context, hc *hcloud.Client, contextName, lbGroup string, certPEM, keyPEM []byte, logger *zap.Logger) (*hcloud.Certificate, error) {
	if existing, _, err := hc.Certificate.GetByName(ctx, contextName); err != nil {
		return nil, fmt.Errorf("probe existing certificate %q: %w", contextName, err)
	} else if existing != nil {
		return nil, fmt.Errorf("certificate %q already exists in this project (id=%d); delete via `hcloud certificate delete %s` first or pick a different context", contextName, existing.ID, contextName)
	}
	logger.Info("uploading TLS certificate to Hetzner", zap.String("name", contextName))
	cert, _, err := hc.Certificate.Create(ctx, hcloud.CertificateCreateOpts{
		Name:        contextName,
		Type:        hcloud.CertificateTypeUploaded,
		Certificate: string(certPEM),
		PrivateKey:  string(keyPEM),
		Labels: map[string]string{
			"managed-by": "y-cluster",
			"lb-group":   lbGroup,
			"context":    contextName,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("create certificate: %w", err)
	}
	return cert, nil
}

// attachCertificateToLB ensures cert is in the LB's HTTPS service
// (listenPort 443) certs list. No-op if already present (so a
// retried Provision is idempotent). Used in the LB-reuse path:
// the second context provisioned in an lb-group adds its cert to
// the existing LB's 443 service.
func attachCertificateToLB(ctx context.Context, hc *hcloud.Client, lb *hcloud.LoadBalancer, cert *hcloud.Certificate, logger *zap.Logger) error {
	httpsService := findService(lb, 443)
	if httpsService == nil {
		return fmt.Errorf("LB %q has no HTTPS service on 443; was it created by a non-y-cluster path?", lb.Name)
	}
	merged := mergeCert(httpsService.HTTP.Certificates, cert)
	if len(merged) == len(httpsService.HTTP.Certificates) {
		// cert already present; nothing to update.
		return nil
	}
	logger.Info("attaching TLS certificate to Hetzner LB",
		zap.Int64("certID", cert.ID),
		zap.Int64("lbID", lb.ID),
		zap.Int("totalCerts", len(merged)))
	action, _, err := hc.LoadBalancer.UpdateService(ctx, lb, 443, hcloud.LoadBalancerUpdateServiceOpts{
		Protocol: hcloud.LoadBalancerServiceProtocolHTTPS,
		HTTP: &hcloud.LoadBalancerUpdateServiceOptsHTTP{
			Certificates: merged,
		},
	})
	if err != nil {
		return fmt.Errorf("UpdateService(443) on LB %q: %w", lb.Name, err)
	}
	if action != nil {
		if err := waitForActions(ctx, hc, []*hcloud.Action{action}); err != nil {
			return fmt.Errorf("wait for UpdateService action: %w", err)
		}
	}
	return nil
}

// detachCertificateFromLB removes certID from the LB's HTTPS-443
// certs list. Used in Teardown when the LB stays alive (other
// lb-group servers remain) but THIS context's cert needs to come
// off so the Certificate resource can be deleted afterwards.
//
// No-op if the cert wasn't on the LB (or if the LB has no 443
// service): Teardown-time idempotency.
func detachCertificateFromLB(ctx context.Context, hc *hcloud.Client, lb *hcloud.LoadBalancer, certID int64, logger *zap.Logger) error {
	httpsService := findService(lb, 443)
	if httpsService == nil {
		return nil
	}
	remaining := make([]*hcloud.Certificate, 0, len(httpsService.HTTP.Certificates))
	for _, c := range httpsService.HTTP.Certificates {
		if c.ID != certID {
			remaining = append(remaining, c)
		}
	}
	if len(remaining) == len(httpsService.HTTP.Certificates) {
		return nil // not present
	}
	if len(remaining) == 0 {
		// Hetzner rejects an HTTPS service with empty cert list.
		// We end up here when the operator tears down the only
		// context with a cert on a still-shared LB -- meaning
		// other lb-group servers exist but contributed no cert.
		// That can't happen in the normal Provision flow (every
		// Provision uploads a cert), but tolerate it by keeping
		// our cert on the LB and warning. The next Provision in
		// the lb-group will add its own cert and a future
		// Teardown will be able to drop ours.
		logger.Warn("cannot remove last cert from LB; HTTPS service requires at least one. Cert stays attached and persists -- delete manually if you intend to retire the LB.",
			zap.Int64("certID", certID),
			zap.Int64("lbID", lb.ID))
		return nil
	}
	logger.Info("detaching TLS certificate from Hetzner LB",
		zap.Int64("certID", certID),
		zap.Int64("lbID", lb.ID),
		zap.Int("remainingCerts", len(remaining)))
	action, _, err := hc.LoadBalancer.UpdateService(ctx, lb, 443, hcloud.LoadBalancerUpdateServiceOpts{
		Protocol: hcloud.LoadBalancerServiceProtocolHTTPS,
		HTTP: &hcloud.LoadBalancerUpdateServiceOptsHTTP{
			Certificates: remaining,
		},
	})
	if err != nil {
		return fmt.Errorf("UpdateService(443) detach cert %d: %w", certID, err)
	}
	if action != nil {
		if err := waitForActions(ctx, hc, []*hcloud.Action{action}); err != nil {
			return fmt.Errorf("wait for UpdateService action: %w", err)
		}
	}
	return nil
}

// findService returns the LB service matching listenPort, or nil
// if no service has that listen port.
func findService(lb *hcloud.LoadBalancer, listenPort int) *hcloud.LoadBalancerService {
	for i := range lb.Services {
		if lb.Services[i].ListenPort == listenPort {
			return &lb.Services[i]
		}
	}
	return nil
}

// mergeCert returns the input list with cert appended iff not
// already present (matched by ID). Lets attachCertificateToLB be
// idempotent under retries.
func mergeCert(certs []*hcloud.Certificate, cert *hcloud.Certificate) []*hcloud.Certificate {
	for _, c := range certs {
		if c.ID == cert.ID {
			return certs
		}
	}
	return append(append([]*hcloud.Certificate{}, certs...), cert)
}

// lbConfig narrows what ensureLoadBalancer needs from the full
// HetznerConfig so unit tests can drive it without the rest of the
// cfg machinery.
type lbConfig struct {
	LBGroup  string
	Location string
}

// deleteLBIfEmpty deletes the LB iff no servers managed by us with
// the given lb-group remain. Called from Teardown after the server
// delete completes; the label_selector target makes the just-
// deleted server fall out of the LB target list automatically, so
// we only need to count remaining matched servers.
//
// Idempotent: missing LB = nothing to do.
func deleteLBIfEmpty(ctx context.Context, hc *hcloud.Client, lbGroup string, lbID int64, logger *zap.Logger) error {
	if lbGroup == "" {
		return nil
	}
	servers, err := hc.Server.AllWithOpts(ctx, hcloud.ServerListOpts{
		ListOpts: hcloud.ListOpts{LabelSelector: labelSelectorForGroup(lbGroup)},
	})
	if err != nil {
		return fmt.Errorf("list lb-group %q servers: %w", lbGroup, err)
	}
	if len(servers) > 0 {
		logger.Info("LB retains members; not deleting",
			zap.String("lbGroup", lbGroup),
			zap.Int("remainingServers", len(servers)))
		return nil
	}
	// Last server gone; delete the LB.
	var lb *hcloud.LoadBalancer
	if lbID != 0 {
		lb, _, err = hc.LoadBalancer.GetByID(ctx, lbID)
		if err != nil {
			return fmt.Errorf("describe LB id=%d: %w", lbID, err)
		}
	}
	if lb == nil {
		// Fall back to name lookup -- the operator may have
		// torn down state out of band but not the LB.
		lb, _, err = hc.LoadBalancer.GetByName(ctx, lbName(lbGroup))
		if err != nil {
			return fmt.Errorf("describe LB by name: %w", err)
		}
	}
	if lb == nil {
		return nil
	}
	logger.Info("deleting Hetzner LB (last lb-group member gone)",
		zap.Int64("id", lb.ID), zap.String("name", lb.Name))
	if _, err := hc.LoadBalancer.Delete(ctx, lb); err != nil {
		return fmt.Errorf("delete LB: %w", err)
	}
	return nil
}
