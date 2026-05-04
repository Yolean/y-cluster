package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/Yolean/y-cluster/pkg/cluster"
	"github.com/Yolean/y-cluster/pkg/echo"
)

// echoCmd groups the appliance-test workload subcommands. Today
// only `deploy` is implemented; `undeploy` and `probe` are
// candidates if/when call sites grow. The deploy logic itself
// lives in pkg/echo so other Go consumers (e2e tests, future
// appliance build/verify subcommands) can call it directly.
func echoCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "echo",
		Short: "Manage the appliance-test echo workload (Gateway + Deployment + Service + HTTPRoute)",
	}
	cmd.AddCommand(echoDeployCmd())
	cmd.AddCommand(echoRenderCmd())
	return cmd
}

// echoRenderCmd prints the rendered manifest to stdout without
// touching a cluster. The Packer-based Hetzner appliance build
// uses this to drop the workload into k3s's auto-apply manifests
// dir at snapshot time, so first cold boot bootstraps clean
// rather than carrying a stale node registration in the
// datastore.
func echoRenderCmd() *cobra.Command {
	var (
		namespace    string
		gatewayClass string
	)
	cmd := &cobra.Command{
		Use:   "render",
		Short: "Print the rendered echo manifest YAML to stdout (no cluster contact)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			body, err := echo.Render(echo.Options{
				Namespace:    namespace,
				GatewayClass: gatewayClass,
			})
			if err != nil {
				return err
			}
			_, err = cmd.OutOrStdout().Write(body)
			return err
		},
	}
	cmd.Flags().StringVarP(&namespace, "namespace", "n", echo.DefaultNamespace, "namespace to render into")
	cmd.Flags().StringVar(&gatewayClass, "gateway-class", echo.DefaultGatewayClass,
		"GatewayClass the rendered Gateway resource references")
	return cmd
}

func echoDeployCmd() *cobra.Command {
	var (
		contextName  string
		namespace    string
		gatewayClass string
	)
	cmd := &cobra.Command{
		Use:   "deploy",
		Short: "Apply the standard echo workload (Namespace, Gateway, Deployment, Service, HTTPRoute)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := echo.Deploy(cmd.Context(), echo.Options{
				ContextName:  contextName,
				Namespace:    namespace,
				GatewayClass: gatewayClass,
			}); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(),
				"echo deployed to namespace %q on context %q\nprobe: curl http://<host-forward-of-guest-80>%s\n",
				namespace, contextName, echo.PathPrefix)
			return nil
		},
	}
	cmd.Flags().StringVar(&contextName, "context", cluster.DefaultContext, "kubeconfig context name")
	cmd.Flags().StringVarP(&namespace, "namespace", "n", echo.DefaultNamespace, "namespace to deploy into")
	cmd.Flags().StringVar(&gatewayClass, "gateway-class", echo.DefaultGatewayClass,
		"GatewayClass the created Gateway resource references; matches the y-cluster default")
	return cmd
}
