package cmd

import (
	"strings"
	"testing"

	"github.com/codefly-dev/cli/pkg/solutionshape"
)

func TestDoctorSolutionIsRegisteredAndHelpStandsAlone(t *testing.T) {
	var found bool
	for _, child := range DoctorCmd.Commands() {
		if child.Name() == "solution" {
			found = true
		}
	}
	if !found {
		t.Fatal("doctor solution is not registered under doctor")
	}
	if DoctorSolutionCmd.RunE == nil || DoctorSolutionCmd.Run != nil {
		t.Error("doctor solution must use RunE so the root owns exit codes")
	}
	for name, text := range map[string]string{
		"Short": DoctorSolutionCmd.Short, "Long": DoctorSolutionCmd.Long, "Example": DoctorSolutionCmd.Example,
	} {
		if strings.TrimSpace(text) == "" {
			t.Errorf("%s is empty; help must stand alone without network access", name)
		}
	}
	if !DoctorSolutionCmd.SilenceUsage {
		t.Error("a non-conforming solution is an outcome, not a usage mistake")
	}
	for _, flag := range []string{"json", "dir", "require-solution", "roles"} {
		if DoctorSolutionCmd.Flags().Lookup(flag) == nil {
			t.Errorf("--%s is not registered", flag)
		}
	}
}

// The help promises stable diagnostic codes, so a caller scripts against them.
// A code that exists in the package and is not documented cannot be relied on.
func TestEveryDiagnosticCodeIsDocumented(t *testing.T) {
	for _, code := range []string{
		solutionshape.CodeNotASolution, solutionshape.CodeManifestUnreadable,
		solutionshape.CodeConsumedAPIUnnamed, solutionshape.CodeServiceEntryMissing,
		solutionshape.CodeServiceEntryNotTheEntry, solutionshape.CodeEntryServiceMissing,
		solutionshape.CodeEntryServiceUnreadable, solutionshape.CodeModuleIdentityMissing,
		solutionshape.CodeModuleEdgeDeclared, solutionshape.CodeEntryEndpointMissing,
		solutionshape.CodeContractMissing, solutionshape.CodeContractUnreadable,
		solutionshape.CodeOwnerAwareMissing, solutionshape.CodePackageMissing,
		solutionshape.CodePackageUnreadable, solutionshape.CodePackagePublishesShell,
		solutionshape.CodeFrontendMissing, solutionshape.CodeFrontendUnreadable,
		solutionshape.CodePageServiceMissing, solutionshape.CodeInterfaceMissing,
		solutionshape.CodeEntryNotExported, solutionshape.CodePageExportedTooWidely,
		solutionshape.CodeConsumedWithoutBinding, solutionshape.CodePageShellHasNoApp,
		solutionshape.CodeInstallerRoleMissing, solutionshape.CodeBackendContractMissing,
		solutionshape.CodeOperationPoliciesUnread, solutionshape.CodeRoleScopeUnreached,
		solutionshape.CodeOperationReachedByNoRole, solutionshape.CodeBindingWiderThanRoles,
		solutionshape.CodeInstallationRoleUndeclared, solutionshape.CodeModulePageWithoutRoles,
	} {
		if !strings.Contains(DoctorSolutionCmd.Long, code) {
			t.Errorf("diagnostic code %q is not documented in the command's help", code)
		}
	}
}

// --roles is read by people deciding what to grant, so what it prints is the
// contract: per role, the unit's own operations, what each binding carries,
// and the modules reached as the person — and never a vacuous empty line.
func TestRoleReachPrintsWhereEachRoleReaches(t *testing.T) {
	report := &solutionshape.Report{Module: "notes", OpenOperations: 2, Roles: []solutionshape.RoleReach{
		{Role: "viewer", Operations: []string{"GET /v1/notes/items"}, AsPerson: []string{"files"},
			Bindings: []solutionshape.BindingReach{{Binding: "search", Operation: "invoke", Carries: []string{"search.index:read"}}}},
		{Role: "auditor", Operations: []string{},
			Bindings: []solutionshape.BindingReach{{Binding: "rows", Operation: "invoke", ResolvedByComposition: true}}},
		{Role: "unread"},
	}}
	out, err := captureStdout(t, func() error { printRoleReach(report); return nil })
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"also reaches the 2 operation(s) that require no scope",
		"on notes: GET /v1/notes/items",
		"through binding search (invoke): search.index:read",
		"as the person (their own grants decide): files",
		"on notes: no operation that requires a scope",
		"through binding rows (invoke): nothing",
		"on notes: not judged",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("--roles output lacks %q:\n%s", want, out)
		}
	}
}
