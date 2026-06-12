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
	Short: "Resolve a single endpoint to host:port (script-friendly)",
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
	Run: func(cmd *cobra.Command, args []string) {
		ctx, done := common.NewContext()
		defer done()
		cli.Init()

		apiType, err := common.NormalizeAPIType(mustString(cmd, "type"))
		if err != nil {
			fail(done, "%v", err)
		}
		wantName := strings.TrimSpace(mustString(cmd, "endpoint"))
		namingScope := mustString(cmd, "naming-scope")
		requireUp := mustBool(cmd, "require-up")

		workspace, module, service := common.LoadRequired(ctx, args)

		matches := common.FilterEndpoints(service.Endpoints, apiType, wantName)
		switch {
		case len(matches) == 0:
			fail(done, "no endpoint on %s matches (type=%q endpoint=%q)", service.Name, apiType, wantName)
		case len(matches) > 1:
			names := make([]string, len(matches))
			for i, ep := range matches {
				names[i] = ep.Name
			}
			fail(done, "%s has %d matching endpoints (%s) — narrow with --type or --endpoint",
				service.Name, len(matches), strings.Join(names, ", "))
		}

		ep := matches[0]
		resolved, err := common.ResolveNative(ctx, workspace.Name, module.Name, service.Name, namingScope, ep)
		if err != nil {
			fail(done, "cannot resolve %s/%s endpoint %q: %v", module.Name, service.Name, ep.Name, err)
		}
		if resolved.External {
			fail(done, "endpoint %q is external (DNS-resolved at runtime); cannot resolve offline", ep.Name)
		}
		if resolved.Unsupported {
			fail(done, "endpoint %q has an unsupported api (%q); codefly binds no port for it", ep.Name, ep.API)
		}
		if requireUp && !common.Reachable(resolved.HostPort) {
			fail(done, "endpoint %q is not reachable at %s — is `codefly run service %s` up?", ep.Name, resolved.HostPort, service.Name)
		}

		// The ONLY thing on stdout: the address. Everything else is stderr.
		fmt.Fprintln(os.Stdout, resolved.Address)
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

// fail writes a diagnostic to stderr, runs context cleanup, and exits non-zero,
// keeping stdout clean for the (absent) address so callers can rely on
// `$( ... )` being empty. done() is invoked before os.Exit so deferred cleanup
// (wool flush, file logging, signal handlers) is not bypassed.
func fail(done func(), format string, a ...any) {
	fmt.Fprintf(os.Stderr, "codefly endpoint: "+format+"\n", a...)
	done()
	os.Exit(1)
}
