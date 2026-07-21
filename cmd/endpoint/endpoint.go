// Package endpoint implements `codefly endpoint` — resolve a single service
// endpoint to a bare host:port, suitable for shell capture
// (`$(codefly endpoint mind --type grpc)`).
//
// This is the script-friendly counterpart to `codefly get endpoints`: where
// that prints a table for humans, this prints exactly one address to stdout
// and exits non-zero if it can't resolve one — so external clients (e.g.
// Mind's CLI/desktop) can discover an endpoint WITHOUT a hand-written
// discovery file.
//
// The address is computed deterministically from the service's static
// definition (the same port hash the runtime uses), so it resolves whether or
// not the service is currently running. Liveness is the caller's concern; pass
// --require-up to additionally fail when nothing is listening.
package endpoint

import (
	"fmt"
	"os"
	"strings"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/core/standards"
	"github.com/spf13/cobra"
)

var Cmd = &cobra.Command{
	Use:   "endpoint [service]",
	Short: "Resolve one service endpoint to a script-friendly host and port",
	Long: `Endpoint resolves ONE endpoint of a service to its concrete host:port and
prints just that address to stdout (nothing else). Exits non-zero if no
single endpoint matches.

The address is computed deterministically, so it resolves even before the
service starts. Use --type to pick the API when a service has several
endpoints, or --endpoint to pick by name; if exactly one endpoint matches,
neither is required. Pass --require-up to fail unless the endpoint is
actually reachable.

Examples:
  codefly endpoint mind --type grpc           # -> localhost:6690
  codefly endpoint mind --type http           # -> http://localhost:6691
  PORT=$(codefly endpoint mind --type grpc)
  codefly endpoint mind --type grpc --require-up`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx, done := common.NewContext()
		defer done()
		cli.Init()

		apiType, err := common.NormalizeAPIType(mustString(cmd, "type"))
		if err != nil {
			return fmt.Errorf("codefly endpoint: %w", err)
		}
		wantName := strings.TrimSpace(mustString(cmd, "endpoint"))
		namingScope := mustString(cmd, "naming-scope")
		requireUp := mustBool(cmd, "require-up")

		workspace, module, service, err := common.LoadRequiredE(ctx, args)
		if err != nil {
			return fmt.Errorf("codefly endpoint: %w", err)
		}

		matches := common.FilterEndpoints(service.Endpoints, apiType, wantName)
		switch {
		case len(matches) == 0:
			return fmt.Errorf("codefly endpoint: no endpoint on %s matches (type=%q endpoint=%q)", service.Name, apiType, wantName)
		case len(matches) > 1:
			names := make([]string, len(matches))
			for i, ep := range matches {
				names[i] = ep.Name
			}
			return fmt.Errorf("codefly endpoint: %s has %d matching endpoints (%s) — narrow with --type or --endpoint",
				service.Name, len(matches), strings.Join(names, ", "))
		}

		ep := matches[0]
		resolved, err := common.ResolveNative(ctx, workspace.Name, module.Name, service.Name, namingScope, ep)
		if err != nil {
			return fmt.Errorf("codefly endpoint: cannot resolve %s/%s endpoint %q: %w", module.Name, service.Name, ep.Name, err)
		}
		if resolved.External {
			return fmt.Errorf("codefly endpoint: endpoint %q is external (DNS-resolved at runtime); cannot resolve offline", ep.Name)
		}
		if resolved.Unsupported {
			return fmt.Errorf("codefly endpoint: endpoint %q has an unsupported api (%q); codefly binds no port for it", ep.Name, ep.API)
		}
		if requireUp && !common.Reachable(resolved.HostPort) {
			return fmt.Errorf("codefly endpoint: endpoint %q is not reachable at %s — is `codefly run service %s` up?", ep.Name, resolved.HostPort, service.Name)
		}

		// The ONLY thing on stdout: the address. Everything else is stderr.
		fmt.Fprintln(os.Stdout, resolved.Address)
		return nil
	},
}

func init() {
	Cmd.Flags().String("type", "", fmt.Sprintf("API type to resolve (%s)", strings.Join(standards.APIS(), ", ")))
	Cmd.Flags().String("endpoint", "", "Endpoint name to resolve (when a service has several of the same API)")
	Cmd.Flags().String("naming-scope", "", "Naming scope the service runs under (advanced; empty for the normal case)")
	Cmd.Flags().Bool("require-up", false, "Fail unless the endpoint is actually reachable")
}

func mustString(cmd *cobra.Command, name string) string {
	v, _ := cmd.Flags().GetString(name)
	return v
}

func mustBool(cmd *cobra.Command, name string) bool {
	v, _ := cmd.Flags().GetBool(name)
	return v
}
