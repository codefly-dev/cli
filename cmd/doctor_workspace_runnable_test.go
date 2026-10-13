package cmd

import (
	"strings"
	"testing"
)

// Binding keys of the two operations derivedOperationsFixture writes for
// module backend: one declares a required scope slot, one does not.
const (
	slottedBindingKey   = "BACKEND__API_CONNECT_INVOKE_SOURCE"
	slotlessBindingKey  = "BACKEND__API_CONNECT_PRUNE_RECEIPTS"
	unknownBindingKey   = "BACKEND__API_CONNECT_NO_SUCH_OPERATION"
	derivedRunnablesDir = "modules/backend/contracts/runnables/"
)

// derivedOperationsFixture is the derived-operation tree `codefly generate
// runnables` leaves under module backend, reduced to what judging a decline
// reads: the index, each package's agent and each operation's scope slots.
func derivedOperationsFixture() map[string]string {
	pkg := `{"agent":{"kind":"SERVICE","name":"go-grpc","publisher":"codefly.dev","version":"0.1.53"}}`
	return map[string]string{
		derivedRunnablesDir + "index.json": `{"schema":"codefly/runnable-operations/v1","workspace":"demo","module":"backend","operations":[
  {"name":"api-connect-invoke-source","service":"api","endpoint":"connect","method":"/backend.v1.Api/InvokeSource","path":"contracts/runnables/api/connect/InvokeSource"},
  {"name":"api-connect-prune-receipts","service":"api","endpoint":"connect","method":"/backend.v1.Api/PruneReceipts","path":"contracts/runnables/api/connect/PruneReceipts"}]}`,
		derivedRunnablesDir + "api/connect/InvokeSource/runnable-package.json":  pkg,
		derivedRunnablesDir + "api/connect/InvokeSource/operation.json":         `{"required_scope_slots":[{"name":"source","required_actions":["invoke"]}]}`,
		derivedRunnablesDir + "api/connect/PruneReceipts/runnable-package.json": pkg,
		derivedRunnablesDir + "api/connect/PruneReceipts/operation.json":        `{}`,
	}
}

func decliningWorkspace(keys ...string) string {
	yaml := testWorkspaceYAML + "environments:\n    - name: local\n      runnable-scope-selections:\n"
	for _, key := range keys {
		yaml += "          " + key + ": []\n"
	}
	return yaml
}

func TestDoctorWorkspaceNamesADeclinedRunnableBinding(t *testing.T) {
	clean := singleServiceWorkspace(t, testWorkspaceYAMLLocalDeclared, nil, derivedOperationsFixture())
	requireNoCode(t, runReadiness(t, workspaceReadinessOptions{dir: clean}), codeRunnableBindingDeclined)

	declining := decliningWorkspace(slottedBindingKey)
	report := runReadiness(t, workspaceReadinessOptions{dir: singleServiceWorkspace(t, declining, nil, derivedOperationsFixture())})
	diagnostic := requireCode(t, report, codeRunnableBindingDeclined, "warn")
	for _, want := range []string{"local", slottedBindingKey, "runnable-bindings/" + slottedBindingKey, "no authority"} {
		if !strings.Contains(diagnostic.Message, want) {
			t.Fatalf("message %q does not mention %q", diagnostic.Message, want)
		}
	}
	if !strings.Contains(diagnostic.Remediation, "codefly generate runnable-bindings --env local") {
		t.Fatalf("remediation %q does not name the generation", diagnostic.Remediation)
	}
	requireNoCode(t, report, codeRunnableBindingDeclineInvalid)
	if report.Status != readinessStatusReady {
		t.Fatalf("a decline is a warning, not a failure: status %s", report.Status)
	}
}

// The doctor judges a decline with the rule generation applies, so it never
// reports as deliberate a decline `generate runnable-bindings` refuses: one
// naming no derived operation, or one whose operation has no required scope
// slot to decline.
func TestDoctorWorkspaceRefusesADeclineGenerationRefuses(t *testing.T) {
	for name, tc := range map[string]struct {
		key, want string
	}{
		"no derived operation": {unknownBindingKey, "names no derived operation"},
		"no required slot":     {slotlessBindingKey, "declares no required scope slot"},
	} {
		t.Run(name, func(t *testing.T) {
			dir := singleServiceWorkspace(t, decliningWorkspace(tc.key, slottedBindingKey), nil, derivedOperationsFixture())
			report := runReadiness(t, workspaceReadinessOptions{dir: dir})
			refused := requireCode(t, report, codeRunnableBindingDeclineInvalid, checkStatusFail)
			if !strings.Contains(refused.Message, tc.key) || !strings.Contains(refused.Message, tc.want) {
				t.Fatalf("message %q does not name %s and %q", refused.Message, tc.key, tc.want)
			}
			for _, check := range findDiagnostics(report, codeRunnableBindingDeclined) {
				if strings.Contains(check.Name, tc.key) {
					t.Fatalf("a refused decline is reported as deliberate: %+v", check)
				}
			}
			requireCode(t, report, codeRunnableBindingDeclined, "warn") // the valid one still is
			if report.Status != readinessStatusNotReady {
				t.Fatalf("a decline generation refuses fails readiness: status %s", report.Status)
			}
		})
	}
}

// A derived-operation tree that cannot be read is not a bad decline: no
// decline can be judged at all, and the doctor says that, under its own code.
func TestDoctorWorkspaceReportsUnreadableOperationsAsSuch(t *testing.T) {
	fixture := derivedOperationsFixture()
	fixture[derivedRunnablesDir+"index.json"] = `{"schema":"not-the-index"}`
	dir := singleServiceWorkspace(t, decliningWorkspace(slottedBindingKey), nil, fixture)
	report := runReadiness(t, workspaceReadinessOptions{dir: dir})
	unreadable := requireCode(t, report, codeRunnableOperationsUnreadable, checkStatusFail)
	if !strings.Contains(unreadable.Message, "cannot be read") {
		t.Fatalf("message %q does not say the operations cannot be read", unreadable.Message)
	}
	requireNoCode(t, report, codeRunnableBindingDeclineInvalid)
	requireNoCode(t, report, codeRunnableBindingDeclined)
}
