// Package audit implements the `codefly audit` subtree.
//
// `codefly audit service <name>` runs the agent's Builder.Audit RPC
// and prints a per-service report (CVEs + outdated deps).
//
// `codefly audit workspace` fans out across every service in the
// workspace, aggregates results, and exits non-zero if any service
// reported HIGH+ findings (when --fail-on-vuln is set).
package audit

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/services"
	"github.com/codefly-dev/core/wool"
	"github.com/spf13/cobra"

	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
)

var (
	jsonOut         bool
	includeOutdated bool
	failOnVuln      bool
)

// ServiceCmd implements `codefly audit service [name]`.
var ServiceCmd = &cobra.Command{
	Use:   "service [name]",
	Short: "Audit dependencies of a service",
	Long: `Run the service agent's Builder.Audit RPC. Reports CVEs from the
language's canonical scanner (govulncheck, npm audit, pip-audit, trivy)
plus outdated patch+minor releases. Read-only — never modifies code.`,
	Run: func(cmd *cobra.Command, args []string) {
		ctx, done := common.NewContext()
		defer done()
		defer services.ClearAgents()

		workspace, module, service := common.LoadRequired(ctx, args)
		resp, err := auditService(ctx, workspace, module, service)
		cli.ExitOnError(err, "audit failed")

		identity, err := service.Identity()
		cli.ExitOnError(err, "cannot get service identity")

		if jsonOut {
			emitJSON(resp)
		} else {
			emitTable(identity, resp)
		}

		if failOnVuln && hasHighSeverity(resp) {
			// os.Exit skips the deferred ClearAgents — run it explicitly so the
			// loaded builder agent isn't orphaned in the CI fail path.
			services.ClearAgents()
			os.Exit(1)
		}
	},
}

func auditService(ctx context.Context, workspace *resources.Workspace, module *resources.Module, service *resources.Service) (*builderv0.AuditResponse, error) {
	w := wool.Get(ctx).In("auditService", wool.NameField(service.Name))
	instance, err := services.Load(ctx, workspace, module, service)
	if err != nil {
		return nil, w.Wrapf(err, "cannot load service")
	}
	if err := instance.LoadBuilder(ctx); err != nil {
		return nil, w.Wrapf(err, "cannot load builder")
	}
	// Builder.Load primes the agent with identity/endpoints — required
	// before any other Builder RPC.
	if _, err := instance.Builder.Load(ctx); err != nil {
		return nil, w.Wrapf(err, "builder load")
	}
	return instance.Builder.Audit(ctx, &builderv0.AuditRequest{
		IncludeOutdated: includeOutdated,
		FailOnVuln:      failOnVuln,
	})
}

// hasHighSeverity is the gate for --fail-on-vuln; the agent's
// fail_on_vuln flag also makes the agent return ERROR, but we honor
// the CLI flag locally too so the exit code matches the table output.
func hasHighSeverity(r *builderv0.AuditResponse) bool {
	if r == nil {
		return false
	}
	for _, f := range r.Findings {
		if f.Severity >= builderv0.AuditFinding_HIGH {
			return true
		}
	}
	return false
}

func emitJSON(r *builderv0.AuditResponse) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(r)
}

func emitTable(identity *resources.ServiceIdentity, r *builderv0.AuditResponse) {
	if r == nil {
		fmt.Println("(no response)")
		return
	}
	cli.Header(1, "Audit: %s/%s [%s, tool=%s]", identity.Module, identity.Name, r.Language, r.Tool)
	state := r.GetState()
	if state != nil {
		fmt.Printf("  state: %s  %s\n", state.State.String(), state.Message)
	}
	if len(r.Findings) > 0 {
		fmt.Printf("\n  Findings (%d):\n", len(r.Findings))
		for _, f := range r.Findings {
			fmt.Printf("    [%s] %s  %s %s → %s\n",
				f.Severity.String(), f.Id, f.Package, f.CurrentVersion, f.FixedVersion)
			if f.Summary != "" {
				fmt.Printf("       %s\n", f.Summary)
			}
		}
	}
	if len(r.Outdated) > 0 {
		fmt.Printf("\n  Outdated (%d):\n", len(r.Outdated))
		for _, o := range r.Outdated {
			fmt.Printf("    %s  %s → %s", o.Package, o.Current, o.LatestSafe)
			if o.LatestMajor != "" && o.LatestMajor != o.LatestSafe {
				fmt.Printf("  (latest: %s)", o.LatestMajor)
			}
			fmt.Println()
		}
	}
	if len(r.Findings) == 0 && len(r.Outdated) == 0 {
		fmt.Println("  ✓ no findings")
	}
}

func init() {
	ServiceCmd.Flags().BoolVar(&jsonOut, "json", false, "Emit raw JSON instead of a table")
	ServiceCmd.Flags().BoolVar(&includeOutdated, "outdated", true, "Also report outdated patch+minor releases")
	ServiceCmd.Flags().BoolVar(&failOnVuln, "fail-on-vuln", false, "Exit non-zero if any HIGH/CRITICAL finding is present")
}
