package run

import (
	"testing"
)

// The solution verb delegates to runServiceCommand, which folds --naming-scope
// into env.NamingScope (and thus every port hash) — but only if the flag is
// actually registered on SolutionCmd, so a solution can boot on a disjoint port
// set in parallel with another running stack. Both port-isolation flags must
// also describe the same mechanism identically to their ServiceCmd twins, or
// `--help` documents one flag two different ways.
func TestSolutionCommandExposesPortIsolationFlags(t *testing.T) {
	for _, name := range []string{"naming-scope", "temporary-ports"} {
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
