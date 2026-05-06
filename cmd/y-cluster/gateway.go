package main

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/Yolean/y-cluster/pkg/cluster"
	"github.com/Yolean/y-cluster/pkg/gateway"
)

// gatewayCmd is the parent of `y-cluster gateway *`. Today
// it has one child (`state`); `clear-dns-hint-ip` is wired
// here too because it lives in the same surface area, but
// the canonical caller is prepare-export, not interactive.
//
// Future ops (rotate-cert, diff-vs-baseline, route-test) slot
// under this same command group when use cases land.
func gatewayCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "gateway",
		Short: "Inspect and manage the y-cluster Gateway state",
	}
	cmd.AddCommand(gatewayStateCmd())
	cmd.AddCommand(gatewayClearDNSHintIPCmd())
	return cmd
}

func gatewayStateCmd() *cobra.Command {
	var contextName string
	cmd := &cobra.Command{
		Use:   "state",
		Short: "Print the cluster's reconciled Gateway state as JSON",
		Long: `Snapshot the cluster's GatewayClass, Gateway, HTTPRoute, GRPCRoute,
ClientTrafficPolicy, and BackendTrafficPolicy resources -- including
their reconciliation status conditions -- and print as JSON to stdout.

The shape is documented in pkg/gateway/state.schema.json
(generated). Consumers parse the JSON to determine HTTPS readiness
(walk gateways[].status.listeners[]), redirect-vs-real-traffic on a
port (walk httpRoutes[].rules), and policy effects (walk
clientTrafficPolicies[].spec for numTrustedHops / trustedCIDRs +
.status for whether envoy-gateway accepted them).

Used by:
  - prepare-export: dumps to <cacheDir>/<name>-gateway-state.json so
    the appliance bundle ships the snapshot the customer received.
  - Operator interactive use: ` + "`y-cluster gateway state | jq ...`" + ` for
    debugging.`,
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			st, err := gateway.Fetch(c.Context(), contextName)
			if err != nil {
				return err
			}
			out, err := json.MarshalIndent(st, "", "  ")
			if err != nil {
				return fmt.Errorf("marshal state: %w", err)
			}
			fmt.Fprintln(c.OutOrStdout(), string(out))
			return nil
		},
	}
	cmd.Flags().StringVar(&contextName, "context", cluster.DefaultContext, "kubeconfig context name")
	return cmd
}

func gatewayClearDNSHintIPCmd() *cobra.Command {
	var contextName string
	var gatewayClassName string
	cmd := &cobra.Command{
		Use:   "clear-dns-hint-ip",
		Short: "Remove the yolean.se/dns-hint-ip annotation from the y-cluster GatewayClass",
		Long: `Used by prepare-export to strip the per-deploy IP from the appliance
snapshot. The annotation is set by envoygateway.Install at provision
time, scoped to the operator's local LB or public IP -- baking it
into the customer-facing snapshot would point their tooling at our
infrastructure.

Idempotent: a no-op when the annotation isn't present (or the
GatewayClass doesn't exist).`,
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			return gateway.ClearDNSHintIPAnnotation(c.Context(), contextName, gatewayClassName)
		},
	}
	cmd.Flags().StringVar(&contextName, "context", cluster.DefaultContext, "kubeconfig context name")
	cmd.Flags().StringVar(&gatewayClassName, "gateway-class", "y-cluster", "GatewayClass name to patch")
	return cmd
}
