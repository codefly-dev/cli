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

// A solution runs on the platform it composes: the entry is the root and every
// other root of the composition (what `run workspace` boots) is a co-root, so
// the modules the solution consumes come up with it instead of the entry
// starting beside nothing it names.
func TestSolutionRunSeedsTheCompositionAsCoRoots(t *testing.T) {
	args := solutionRunArgs("wiki/backend", []string{"saas/frontend", "wiki/backend", "robin/chat", "documents/documents", "robin/chat"})
	want := []string{"wiki/backend", "saas/frontend", "robin/chat", "documents/documents"}
	if len(args) != len(want) {
		t.Fatalf("args = %v, want %v", args, want)
	}
	for i := range want {
		if args[i] != want[i] {
			t.Fatalf("args = %v, want %v", args, want)
		}
	}
}
