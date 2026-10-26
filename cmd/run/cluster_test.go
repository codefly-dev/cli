package run

import (
	"testing"
	"time"
)

// TestClusterCommandDeclaresItsRun: `run cluster` takes the modules to run as
// arguments, runs on the local environment unless told otherwise, confirms
// each signed promotion unless --yes, and waits at most the delivery Job's own
// deadline for each Job by default.
func TestClusterCommandDeclaresItsRun(t *testing.T) {
	if ClusterCmd.Use != "cluster [module...]" {
		t.Fatalf("use %q", ClusterCmd.Use)
	}
	for name, want := range map[string]string{
		"env": "local", "app-project": "", "yes": "false", "rebuild": "false",
		"delivery-timeout": (30 * time.Minute).String(),
	} {
		flag := ClusterCmd.Flags().Lookup(name)
		if flag == nil {
			t.Fatalf("flag --%s is not declared", name)
		}
		if flag.DefValue != want {
			t.Errorf("--%s defaults to %q, want %q", name, flag.DefValue, want)
		}
	}
}
