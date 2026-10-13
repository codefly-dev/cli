package cmd

import (
	"strings"
	"testing"
)

func TestDoctorWorkspaceNamesADeclinedRunnableBinding(t *testing.T) {
	clean := singleServiceWorkspace(t, testWorkspaceYAMLLocalDeclared, nil, nil)
	requireNoCode(t, runReadiness(t, workspaceReadinessOptions{dir: clean}), codeRunnableBindingDeclined)

	declining := testWorkspaceYAML + `environments:
    - name: local
      runnable-scope-selections:
          SAAS__ACCOUNTS_CONNECT_INVOKE_SOURCE_OPERATION: []
          SAAS__ACCOUNTS_CONNECT_PRUNE_SOURCE_OPERATION_RECEIPTS:
              - slot: source
                invoke:
                    - resource_kind: datasource.sources
                      actions: [invoke, read]
                      resource_ids: [source-a]
`
	report := runReadiness(t, workspaceReadinessOptions{dir: singleServiceWorkspace(t, declining, nil, nil)})
	diagnostic := requireCode(t, report, codeRunnableBindingDeclined, "warn")
	for _, want := range []string{"local", "SAAS__ACCOUNTS_CONNECT_INVOKE_SOURCE_OPERATION", "runnable-bindings/SAAS__ACCOUNTS_CONNECT_INVOKE_SOURCE_OPERATION", "no authority"} {
		if !strings.Contains(diagnostic.Message, want) {
			t.Fatalf("message %q does not mention %q", diagnostic.Message, want)
		}
	}
	if !strings.Contains(diagnostic.Remediation, "codefly generate runnable-bindings --env local") {
		t.Fatalf("remediation %q does not name the generation", diagnostic.Remediation)
	}
	for _, check := range report.Checks {
		if check.Code == codeRunnableBindingDeclined && strings.Contains(check.Message, "PRUNE") {
			t.Fatalf("a selected binding is not a decline: %q", check.Message)
		}
	}
	if report.Status != readinessStatusReady {
		t.Fatalf("a decline is a warning, not a failure: status %s", report.Status)
	}
}
