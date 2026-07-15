package audit

import "testing"

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
