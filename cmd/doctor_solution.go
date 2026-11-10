package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/solutionshape"
	"github.com/codefly-dev/core/resources"
	"github.com/spf13/cobra"
)

var (
	doctorSolutionJSON    bool
	doctorSolutionDir     string
	doctorSolutionRequire bool
	doctorSolutionEnv     string
	doctorSolutionRoles   bool
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
	Short:        "Check a solution or a module page, and what each of its roles reaches, without changing anything",
	Long: `Judge a solution module against the shape ` + "`codefly add solution`" + ` generates.

With no argument, every module in the workspace is judged and the ones that
ship no solution manifest are reported as "not a solution" rather than as
broken. Name a module to judge only that one.

A MODULE PAGE — a module with no solution manifest that declares
solution.frontend.codefly.yaml beside a service — is judged too, on what
decides who can use it: people are entitled to a page only through a role it
declares, so a page declaring no role is shown to nobody
(module_page_declares_no_roles), and a page holds no standing authority
(installation_role: none).

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

  role breadth   A role granting an action on one of the solution's OWN kinds
                 that no route it publishes requires is checked by nothing, so
                 holding it reaches nothing. It is reported as a warning: it
                 refuses no call and breaks no run, and either a route is
                 missing its policy or the role is wider than the solution.

Permissions are judged by reading the contract through Core's own loader,
which refuses a role granting a scope no binding backs and no ceiling covers
("role X grants documents:read, which is neither within the module's own scope
ceilings nor a scope one of its bindings spells out"). That is the
missing-permission class, caught before install rather than at the first
refused call.

The opposite class — a role far wider than anything it reaches — is judged by
comparing what each role grants on the solution's own kinds against what the
operations in its published contracts require, read with the same reader the
authority's own route policies are derived with. A role grant on a kind that
comes from a BINDING is not judged: the module at the other end authorizes the
relayed child, and its policies are not in this directory.

Every role is then judged against what it REACHES, with the authority's own
decide.AdmitMethod over the published operations — the function the local
authority uses at boot to log "no single declared role covers this method":

  operation_reached_by_no_role  an operation requires what no role carries, so
                                no person can ever call it.
  binding_wider_than_roles      a binding redeemed for a person (invoke,
                                lookup) asks for what no role grants. Issuance
                                cuts a child to the holder's grants AND the
                                binding, so that part is never carried, and the
                                consent an administrator reviews is wider than
                                anyone can use.
  installation_role_undeclared  roles, and no installation_role: a host refuses
                                the install unless the request names one.

A consumed module the contract declares under edge_dependencies is reached on
the person's own session and needs no binding, so it is not reported as
consumed_module_without_binding.

--roles prints, per role, what a holder reaches: the unit's own operations,
what it can carry through each binding, and the modules it reaches as the
person. It is the directory's view; what the module at the other end of a
binding then admits, and onward, is the composition's.

What a composition's values resolve each binding to needs the composition and
not the directory, and --env <environment> judges the rendered one: it compares
every rendered module manifest's operation bindings against that environment's
rendered module envelope — the ceiling the host enforces — and names each
binding the ceiling does not admit. The host drops such a binding one line at a
time on its own stdout, so nothing else says it: in a portal it reads as "tool
exposure is unavailable", and an installation refuses with "module ... changed
since review". Read-only, with no cluster and no credential.

Exit codes:
  0  every judged solution conforms (warnings allowed)
  1  at least one check failed, or the command itself failed

With --json, a versioned report per module is printed to stdout:
  {schema_version, module, dir, is_solution, is_module_page?, status: ok|fail,
   checks: [{code, status, message, remediation?}],
   roles?: [{role, operations, bindings?: [{binding, operation, carries?,
            resolved_by_composition?, whole_binding?}], as_person?}]}

Stable diagnostic codes: not_a_solution, solution_manifest_unreadable,
consumed_api_unnamed, service_entry_missing, service_entry_not_the_entry,
entry_service_missing, entry_service_unreadable, module_identity_missing,
module_edge_declared, entry_endpoint_missing, module_contract_missing,
module_contract_unreadable, owner_aware_missing, module_package_missing,
module_package_unreadable, module_package_publishes_shell,
frontend_declaration_missing, frontend_declaration_unreadable,
page_shell_missing, module_interface_missing, entry_endpoint_not_exported,
page_shell_exported_too_widely, consumed_module_without_binding,
page_shell_has_no_app, installer_role_missing, backend_contract_missing,
operation_policies_unread, role_grants_unreached_scope,
operation_reached_by_no_role, binding_wider_than_roles,
installation_role_undeclared, module_page_declares_no_roles, and with --env:
not_rendered_for_environment, module_envelope_not_rendered,
module_envelope_unreadable, module_envelope_omits_binding,
module_envelope_admits_every_binding.`,
	Example: `  # Judge every solution in the workspace.
  codefly doctor solution

  # Judge one, and script against the result.
  codefly doctor solution wiki --json

  # Gate a consumer repository's tree, where a solution need not be a workspace.
  for s in solutions/*/; do codefly doctor solution --dir "$s" || exit 1; done

  # The same gate, where every directory walked must BE a solution: this
  # catches a solution whose manifest is missing, which otherwise reads as
  # "not a solution" and passes.
  codefly doctor solution --dir solutions/robin-builder --require-solution

  # What each role reaches, before anything boots.
  codefly doctor solution --dir solutions/notes --roles

  # A module page is judged the same way.
  codefly doctor solution --dir modules/catalog --roles

  # Judge what a cell would actually enforce, not just the directory's shape.
  codefly doctor solution wiki --env blue`,
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
			// --dir has no workspace, and a rendered tree belongs to one.
			if doctorSolutionEnv != "" {
				return fmt.Errorf("--env judges an environment's rendered tree, which belongs to a workspace; --dir judges a directory with no workspace, so the two cannot be combined")
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
			if !report.IsSolution && !report.IsModulePage {
				if doctorSolutionRequire {
					return fmt.Errorf("%s ships no %s, so it is not a solution — and --require-solution says it must be", report.Module, solutionManifestName)
				}
				fmt.Printf("OK    %s: not a solution (no %s)\n", report.Module, solutionManifestName)
				return nil
			}
			if !report.IsSolution && doctorSolutionRequire {
				return fmt.Errorf("%s is a module page, not a solution (no %s) — and --require-solution says it must be a solution", report.Module, solutionManifestName)
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
			report := solutionshape.Inspect(module.Name, module.Dir())
			if doctorSolutionEnv != "" && report.IsSolution {
				report.InspectDelivered(workspace.Dir(), doctorSolutionEnv)
			}
			reports = append(reports, report)
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
				report := solutionshape.Inspect(module.Name, module.Dir())
				if doctorSolutionEnv != "" && report.IsSolution {
					report.InspectDelivered(workspace.Dir(), doctorSolutionEnv)
				}
				reports = append(reports, report)
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
		if !report.IsSolution && !report.IsModulePage {
			continue
		}
		solutions++
		kind := ""
		if report.IsModulePage {
			kind = "  (module page)"
		}
		fmt.Printf("\n%s  %s%s\n", statusGlyph(report.Status), report.Module, kind)
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
			if report.IsModulePage {
				fmt.Printf("  conforms to the module page shape\n")
			} else {
				fmt.Printf("  conforms to the solution shape\n")
			}
		}
		if doctorSolutionRoles {
			printRoleReach(report)
		}
	}
	if solutions == 0 {
		// Never a vacuous pass: a run that judged nothing must not read as a
		// run that found nothing wrong.
		return fmt.Errorf("no solution or module page was found to judge; a module is a solution when it ships a solution manifest, and a module page when it declares %s beside a service", solutionshape.FrontendFileName)
	}
	return solutionReportsOutcome(reports)
}

func solutionReportsOutcome(reports []*solutionshape.Report) error {
	var broken []string
	for _, report := range reports {
		if (report.IsSolution || report.IsModulePage) && !report.Conforms() {
			broken = append(broken, report.Module)
		}
	}
	if len(broken) > 0 {
		return fmt.Errorf("%d solution(s) do not conform to the shape: %v", len(broken), broken)
	}
	return nil
}

// printRoleReach prints what each role reaches: on the unit itself, through
// each binding it redeems for the holder, and on the holder's own session.
func printRoleReach(report *solutionshape.Report) {
	if len(report.Roles) == 0 {
		return
	}
	fmt.Printf("  roles — what a holder reaches, as this directory declares it:\n")
	if report.OpenOperations > 0 {
		fmt.Printf("    (every role, and any checked caller, also reaches the %d operation(s) that require no scope)\n", report.OpenOperations)
	}
	for _, role := range report.Roles {
		fmt.Printf("    %s\n", role.Role)
		switch {
		case role.Operations == nil:
			fmt.Printf("      on %s: not judged (its published operations could not be read)\n", report.Module)
		case len(role.Operations) == 0:
			fmt.Printf("      on %s: no operation that requires a scope\n", report.Module)
		default:
			fmt.Printf("      on %s: %s\n", report.Module, strings.Join(role.Operations, ", "))
		}
		for _, binding := range role.Bindings {
			carries := strings.Join(binding.Carries, ", ")
			switch {
			case binding.WholeBinding && carries == "":
				carries = "everything it carries (the role names the binding)"
			case binding.ResolvedByComposition && carries == "":
				carries = "nothing (its ceiling is what its calls require, and the role grants nothing outside this unit's kinds)"
			case binding.ResolvedByComposition:
				carries += " at most (its calls decide which)"
			}
			fmt.Printf("      through binding %s (%s): %s\n", binding.Binding, binding.Operation, carries)
		}
		if len(role.AsPerson) > 0 {
			fmt.Printf("      as the person (their own grants decide): %s\n", strings.Join(role.AsPerson, ", "))
		}
	}
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
	DoctorSolutionCmd.Flags().BoolVar(&doctorSolutionRoles, "roles", false, "Also print, per role, what a holder reaches: the unit's own operations (decided by the authority's own decide.AdmitMethod), what it carries through each binding, and the modules reached on the person's own session")
	DoctorSolutionCmd.Flags().StringVar(&doctorSolutionEnv, "env", "", "Also judge what this environment's RENDERED tree would enforce: every rendered manifest's operation bindings against the rendered module envelope, naming each binding the ceiling does not admit")
	DoctorSolutionCmd.Flags().StringVar(&doctorSolutionDir, "dir", "", "Judge this module directory on its own, without resolving a workspace. For a consumer repository gating a tree of solutions, where a directory need not be a workspace")
	DoctorCmd.AddCommand(DoctorSolutionCmd)
}
