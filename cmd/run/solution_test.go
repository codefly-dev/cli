package run

import (
	"testing"
)

// The solution verb delegates to runServiceCommand, so every shared lifecycle
// flag must be registered on SolutionCmd and described identically to its
// ServiceCmd twin. This covers the port-isolation controls and the readiness
// deadline that bounds the delegated run.
func TestSolutionCommandExposesSharedRunFlags(t *testing.T) {
	for _, name := range []string{"naming-scope", "temporary-ports", "readiness-timeout"} {
		solutionFlag := SolutionCmd.Flags().Lookup(name)
		if solutionFlag == nil {
			t.Fatalf("run solution has no --%s flag", name)
		}
		serviceFlag := ServiceCmd.Flags().Lookup(name)
		if serviceFlag == nil {
			t.Fatalf("run service has no --%s flag", name)
		}
		if solutionFlag.Usage != serviceFlag.Usage {
			t.Fatalf("--%s help diverges: solution=%q service=%q", name, solutionFlag.Usage, serviceFlag.Usage)
		}
	}
}
