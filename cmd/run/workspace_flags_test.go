package run

import "testing"

// `codefly run workspace` documents itself as taking the flags of `codefly run
// service` that do not name one root service (docs/commands.md). The shared
// runner honours each of these from the same variable, so a flag the workspace
// command does not register is a documented behaviour nobody can reach: a
// naming scope that hashed two endpoints to one port aborted every run of a
// composition until --temporary-ports was registered here.
func TestRunWorkspaceRegistersTheRunServiceFlagsItDocuments(t *testing.T) {
	for _, name := range []string{"naming-scope", "temporary-ports", "output-env", "output-env-service", "runtime-context", "set", "exclude-dependency", "exclude-workspace-configuration"} {
		if ServiceCmd.Flags().Lookup(name) == nil {
			t.Fatalf("run service no longer registers --%s; the documented contract moved", name)
		}
		if WorkspaceCmd.Flags().Lookup(name) == nil {
			t.Errorf("run workspace does not register --%s, which its documentation promises", name)
		}
	}
	for _, name := range []string{"stand-alone", "exclude-root"} {
		if WorkspaceCmd.Flags().Lookup(name) != nil {
			t.Errorf("run workspace registers --%s, which names one root service and means nothing for a composition", name)
		}
	}
}
