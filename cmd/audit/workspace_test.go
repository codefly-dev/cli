package audit

import (
	"testing"

	"github.com/spf13/cobra"
)

func TestWorkspaceAuditResult(t *testing.T) {
	tests := []struct {
		name            string
		anyError        bool
		failOnVuln      bool
		anyHighSeverity bool
		wantError       bool
	}{
		{name: "success"},
		{name: "high severity ignored without gate", anyHighSeverity: true},
		{name: "incomplete", anyError: true, wantError: true},
		{name: "vulnerability gate", failOnVuln: true, anyHighSeverity: true, wantError: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := workspaceAuditResult(test.anyError, test.failOnVuln, test.anyHighSeverity)
			if (err != nil) != test.wantError {
				t.Fatalf("workspaceAuditResult() error = %v", err)
			}
		})
	}
}

func TestAuditCommandsDefaultToRuntimeDependencies(t *testing.T) {
	for _, command := range []*cobra.Command{ServiceCmd, WorkspaceCmd} {
		flag := command.Flags().Lookup("include-dev")
		if flag == nil {
			t.Fatalf("%s has no --include-dev flag", command.CommandPath())
		}
		if flag.DefValue != "false" {
			t.Fatalf("%s --include-dev default = %q, want false", command.CommandPath(), flag.DefValue)
		}
	}
}
