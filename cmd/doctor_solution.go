package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/solutionshape"
	"github.com/codefly-dev/core/resources"
	"github.com/spf13/cobra"
)

var (
	doctorSolutionJSON    bool
	doctorSolutionDir     string
	doctorSolutionRequire bool
)

// solutionManifestName is the file whose presence makes a module a solution.
const solutionManifestName = "solution.codefly.yaml"

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

Permissions are judged by reading the contract through Core's own loader,
which refuses a role granting a scope no binding backs and no ceiling covers
("role X grants documents:read, which is neither within the module's own scope
ceilings nor a scope one of its bindings spells out"). That is the
missing-permission class, caught before install rather than at the first
refused call. What a composition's values resolve each binding to is NOT
judged here: that needs the composition, not the directory.

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
page_shell_exported_too_widely, consumed_module_without_binding.`,
	Example: `  # Judge every solution in the workspace.
  codefly doctor solution

  # Judge one, and script against the result.
  codefly doctor solution wiki --json

  # Gate a consumer repository's tree, where a solution need not be a workspace.
  for s in solutions/*/; do codefly doctor solution --dir "$s" || exit 1; done

  # The same gate, where every directory walked must BE a solution: this
  # catches a solution whose manifest is missing, which otherwise reads as
  # "not a solution" and passes.
  codefly doctor solution --dir solutions/robin-builder --require-solution`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(_ *cobra.Command, args []string) error {
		ctx, done := common.NewContext()
		defer done()

		// --dir judges a module directory on its own, with no workspace
		// resolution. A consumer repository gating `solutions/*/` has a tree
		// of module directories and not necessarily a workspace at each one:
		// obin-ai/core-solutions' excel-extract is a real solution with no
		// workspace.codefly.yaml, and requiring one made it the single
		// solution that could not be judged at all.
		if doctorSolutionDir != "" {
			if len(args) > 0 {
				return fmt.Errorf("--dir judges the directory itself, so it takes no module name")
			}
			absolute, err := filepath.Abs(doctorSolutionDir)
			if err != nil {
				return fmt.Errorf("cannot resolve %s: %w", doctorSolutionDir, err)
			}
			name := filepath.Base(absolute)
			if module, loadErr := resources.LoadModuleFromDir(ctx, absolute); loadErr == nil && module.Name != "" {
				name = module.Name
			}
			report := solutionshape.Inspect(name, absolute)
			reports := []*solutionshape.Report{report}
			if doctorSolutionJSON {
				return printSolutionReportsJSON(reports)
			}
			// The caller named this directory, so "it is not a solution" is an
			// answer and not a vacuous pass. A tree gate looping solutions/*/
			// meets directories that are not solutions — core-solutions'
			// client-engagement is one — and must not fail on them.
			if !report.IsSolution {
				if doctorSolutionRequire {
					return fmt.Errorf("%s ships no %s, so it is not a solution — and --require-solution says it must be", report.Module, solutionManifestName)
				}
				fmt.Printf("OK    %s: not a solution (no %s)\n", report.Module, solutionManifestName)
				return nil
			}
			return printSolutionReports(reports)
		}

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
	DoctorSolutionCmd.Flags().BoolVar(&doctorSolutionRequire, "require-solution", false, "With --dir, fail when the directory ships no solution manifest instead of reporting it as not a solution. A tree gate uses it to say every directory it walks must BE a solution")
	DoctorSolutionCmd.Flags().StringVar(&doctorSolutionDir, "dir", "", "Judge this module directory on its own, without resolving a workspace. For a consumer repository gating a tree of solutions, where a directory need not be a workspace")
	DoctorCmd.AddCommand(DoctorSolutionCmd)
}
