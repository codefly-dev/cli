// Package get contains read-only commands that report information about
// services and their endpoints.
package get

import (
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/core/standards"
	"github.com/spf13/cobra"
)

// EndpointsCmd lists a service's endpoints, optionally filtered by API type,
// preferring a running flow's concrete address and probing whether it is live.
var EndpointsCmd = &cobra.Command{
	Use:   "endpoints [service]",
	Short: "Report addresses and health for a service's declared endpoints",
	Long: `Endpoints lists the endpoints declared by a service — name, API type,
visibility — together with the localhost address each binds to and whether
it is currently reachable.

A running scoped flow is authoritative for addresses. Without one, addresses
fall back to the same deterministic hash used for offline planning. The STATUS
column reflects a live TCP probe: "up" means something is listening now.

Examples:
  codefly get endpoints                 # active service
	codefly get endpoints mind            # by name
	codefly get endpoints mind --type grpc`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx, done := common.NewContext()
		defer done()
		cli.Init()

		apiType, err := common.NormalizeAPIType(mustString(cmd, "type"))
		if err != nil {
			return err
		}
		namingScope := mustString(cmd, "naming-scope")

		workspace, module, service, err := common.LoadRequiredE(ctx, args)
		if err != nil {
			return err
		}

		endpoints := common.FilterEndpoints(service.Endpoints, apiType, "")
		if len(endpoints) == 0 {
			if apiType != "" {
				cli.Info("Service %s has no %s endpoints", service.Name, apiType)
			} else {
				cli.Info("Service %s has no endpoints", service.Name)
			}
			return nil
		}

		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "NAME\tTYPE\tVISIBILITY\tADDRESS\tSTATUS")
		anyUp := false
		for _, ep := range endpoints {
			resolved, rErr := common.ResolvePreferredNative(ctx, workspace.Name, module.Name, service.Name, namingScope, ep)
			address, statusText := "-", "-"
			switch {
			case rErr != nil:
				statusText = "error"
			case resolved.External:
				statusText = "external"
			case resolved.Unsupported:
				statusText = "unsupported"
			default:
				address = resolved.Address
				if common.Reachable(resolved.HostPort) {
					statusText, anyUp = "up", true
				} else {
					statusText = "down"
				}
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", ep.Name, dash(ep.API), dash(ep.Visibility), address, statusText)
		}
		_ = w.Flush()

		if !anyUp {
			cli.Info("Nothing listening yet — start it with `codefly run service %s`.", service.Name)
		}
		return nil
	},
}

func init() {
	EndpointsCmd.Flags().String("type", "", fmt.Sprintf("Filter by API type (%s)", strings.Join(standards.APIS(), ", ")))
	EndpointsCmd.Flags().String("naming-scope", "", "Naming scope the service runs under (advanced; empty for the normal case)")
}

func mustString(cmd *cobra.Command, name string) string {
	v, _ := cmd.Flags().GetString(name)
	return v
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
