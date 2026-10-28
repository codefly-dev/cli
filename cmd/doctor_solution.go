package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/solutionshape"
	"github.com/spf13/cobra"
)

var doctorSolutionJSON bool

// DoctorSolutionCmd judges a solution against the one shape a solution has.
//
// It exists because the shape was, until now, checkable only against
// somebody's survey of the solutions that happened to exist — which is how one
// fleet reached nine solutions in four different states, two of them unable to
// receive a person capability at all with nothing failing to say so. A shape
// nothing can check is a convention, and a convention drifts.
var DoctorSolutionCmd = &cobra.Command{
	Use: "solution [module]",
	// A non-conforming solution is an expected outcome, not a usage mistake.
	SilenceUsage: true,
	Short:        "Check a solution against the one shape a solution has, without changing anything",
	Long: `Judge a solution module against the shape ` + "`codefly add solution`" + ` generates.

With no argument, every module in the workspace is judged and the ones that
ship no solution manifest are reported as "not a solution" rather than as
broken. Name a module to judge only that one.

It is strictly read-only: it opens files and writes none.

Every rule is one that silently broke something. The two worth knowing:

  owner_aware    The authority issues a person a capability at a solution ONLY
                 when its module contract declares owner_aware: true, and
                 refuses by name otherwise. Without it the solution receives no
                 owner-selected scope and can act for nobody — and nothing
                 fails at build time to tell you.

  module edges   A solution reaches every module through the host's door,
                 which checks the call. A service-dependency naming a module
                 renders into a mesh allow rule straight to that module's
                 service, past the check.

Exit codes:
  0  every judged solution conforms (warnings allowed)
  1  at least one check failed, or the command itself failed

With --json, a versioned report per module is printed to stdout:
  {schema_version, module, dir, is_solution, status: ok|fail,
   checks: [{code, status, message, remediation?}]}

Stable diagnostic codes: not_a_solution, solution_manifest_unreadable,
consumed_api_unnamed, service_entry_missing, service_entry_not_the_entry,
entry_service_missing, entry_service_unreadable, module_identity_missing,
module_edge_declared, entry_endpoint_missing, module_contract_missing,
module_contract_unreadable, owner_aware_missing, module_package_missing,
module_package_unreadable, module_package_publishes_shell,
frontend_declaration_missing, frontend_declaration_unreadable,
page_shell_missing, module_interface_missing, entry_endpoint_not_exported,
page_shell_exported_too_widely.`,
	Example: `  # Judge every solution in the workspace.
  codefly doctor solution

  # Judge one, and script against the result.
  codefly doctor solution wiki --json`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(_ *cobra.Command, args []string) error {
		ctx, done := common.NewContext()
		defer done()

		workspace, err := common.LoadWorkspace(ctx)
		if err != nil {
			return fmt.Errorf("cannot load workspace: %w", err)
		}

		var reports []*solutionshape.Report
		if len(args) == 1 {
			module, loadErr := workspace.LoadModuleFromName(ctx, args[0])
			if loadErr != nil {
				return fmt.Errorf("cannot load module %s: %w", args[0], loadErr)
			}
			reports = append(reports, solutionshape.Inspect(module.Name, module.Dir()))
		} else {
			names := make([]string, 0, len(workspace.Modules))
			for _, reference := range workspace.Modules {
				if reference != nil {
					names = append(names, reference.Name)
				}
			}
			sort.Strings(names)
			for _, name := range names {
				module, loadErr := workspace.LoadModuleFromName(ctx, name)
				if loadErr != nil {
					// A module that does not resolve is the workspace's
					// problem, not the shape's; `doctor workspace` reports it.
					continue
				}
				reports = append(reports, solutionshape.Inspect(module.Name, module.Dir()))
			}
		}

		if doctorSolutionJSON {
			return printSolutionReportsJSON(reports)
		}
		return printSolutionReports(reports)
	},
}

func printSolutionReportsJSON(reports []*solutionshape.Report) error {
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	for _, report := range reports {
		if err := encoder.Encode(report); err != nil {
			return err
		}
	}
	return solutionReportsOutcome(reports)
}

func printSolutionReports(reports []*solutionshape.Report) error {
	solutions := 0
	for _, report := range reports {
		if !report.IsSolution {
			continue
		}
		solutions++
		fmt.Printf("\n%s  %s\n", statusGlyph(report.Status), report.Module)
		for _, check := range report.Checks {
			if check.Status == solutionshape.StatusOK {
				continue
			}
			fmt.Printf("  %s %s: %s\n", statusGlyph(check.Status), check.Code, check.Message)
			if check.Remediation != "" {
				fmt.Printf("      %s\n", check.Remediation)
			}
		}
		if report.Conforms() {
			fmt.Printf("  conforms to the solution shape\n")
		}
	}
	if solutions == 0 {
		// Never a vacuous pass: a run that judged nothing must not read as a
		// run that found nothing wrong.
		return fmt.Errorf("no solution was found to judge; a module is a solution when it ships a solution manifest")
	}
	return solutionReportsOutcome(reports)
}

func solutionReportsOutcome(reports []*solutionshape.Report) error {
	var broken []string
	for _, report := range reports {
		if report.IsSolution && !report.Conforms() {
			broken = append(broken, report.Module)
		}
	}
	if len(broken) > 0 {
		return fmt.Errorf("%d solution(s) do not conform to the shape: %v", len(broken), broken)
	}
	return nil
}

func statusGlyph(status solutionshape.Status) string {
	switch status {
	case solutionshape.StatusFail:
		return "FAIL"
	case solutionshape.StatusWarn:
		return "WARN"
	default:
		return "OK  "
	}
}

func init() {
	DoctorSolutionCmd.Flags().BoolVar(&doctorSolutionJSON, "json", false, "Print a versioned report per module to stdout")
	DoctorCmd.AddCommand(DoctorSolutionCmd)
}
