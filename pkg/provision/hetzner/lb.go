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

func intPtr(n int) *int                       { return &n }
func durationPtr(d time.Duration) *time.Duration { return &d }

// ensureLoadBalancer reconciles a Hetzner LB for cfg.LBGroup and
// returns it. Reuses an existing LB by name (so a second server in
// the same lb-group attaches to the existing LB instead of standing
// up a new one); creates with a label_selector target so any server
// already labelled with this lb-group is auto-attached, and any
// future server with the same labels joins automatically.
//
// Service shape: TCP 80 -> backend 80, TCP 443 -> backend 443.
// Phase 3.a does TCP passthrough; envoy-gateway terminates TLS
// inside the cluster (phase 3.c lands cert-manager + the gateway
// listener wired to a self-signed cert). HTTP listener stays open
// so cert-manager HTTP-01 challenges work even when phase 3.c
// switches to LE-style flows.
//
// Refuses to reuse an LB whose location differs from cfg.Location:
// label_selector targets resolve only within the LB's location, so
// a cross-location reuse would silently fail to pick up new
// servers. Forcing a clear error matches the "no silent leakage"
// stance.
func ensureLoadBalancer(ctx context.Context, hc *hcloud.Client, cfg lbConfig, logger *zap.Logger) (*hcloud.LoadBalancer, error) {
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
	logger.Info("creating Hetzner LB",
		zap.String("name", name),
		zap.String("type", lbType),
		zap.String("location", cfg.Location),
		zap.String("targetSelector", labelSelectorForGroup(cfg.LBGroup)),
	)
	healthCheck := &hcloud.LoadBalancerCreateOptsServiceHealthCheck{
		Protocol: hcloud.LoadBalancerServiceProtocolTCP,
		Interval: durationPtr(lbHealthCheckInterval),
		Timeout:  durationPtr(lbHealthCheckTimeout),
		Retries:  intPtr(lbHealthCheckRetries),
	}
	res, _, err := hc.LoadBalancer.Create(ctx, hcloud.LoadBalancerCreateOpts{
		Name:             name,
		LoadBalancerType: &hcloud.LoadBalancerType{Name: lbType},
		Location:         &hcloud.Location{Name: cfg.Location},
		Algorithm:        &hcloud.LoadBalancerAlgorithm{Type: hcloud.LoadBalancerAlgorithmTypeRoundRobin},
		Services: []hcloud.LoadBalancerCreateOptsService{
			{
				Protocol:        hcloud.LoadBalancerServiceProtocolTCP,
				ListenPort:      intPtr(80),
				DestinationPort: intPtr(80),
				HealthCheck:     healthCheck,
			},
			{
				Protocol:        hcloud.LoadBalancerServiceProtocolTCP,
				ListenPort:      intPtr(443),
				DestinationPort: intPtr(443),
				HealthCheck:     healthCheck,
			},
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
