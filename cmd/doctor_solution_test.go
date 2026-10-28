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
	for _, flag := range []string{"json", "dir", "require-solution"} {
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
	} {
		if !strings.Contains(DoctorSolutionCmd.Long, code) {
			t.Errorf("diagnostic code %q is not documented in the command's help", code)
		}
	}
}
