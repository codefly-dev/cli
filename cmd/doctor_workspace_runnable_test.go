package cmd

import (
	"context"
	"strings"
	"testing"

	"github.com/codefly-dev/cli/cmd/deploy"
	"github.com/codefly-dev/cli/cmd/generate"
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

// slottedOperationFixture is derivedOperationsFixture reduced to its one
// operation with a required scope slot, so an environment can decline every
// binding the workspace derives.
func slottedOperationFixture() map[string]string {
	fixture := derivedOperationsFixture()
	fixture[derivedRunnablesDir+"index.json"] = `{"schema":"codefly/runnable-operations/v1","workspace":"demo","module":"backend","operations":[
  {"name":"api-connect-invoke-source","service":"api","endpoint":"connect","method":"/backend.v1.Api/InvokeSource","path":"contracts/runnables/api/connect/InvokeSource"}]}`
	return fixture
}

// An environment that selects no runnable binding — none derived, or every
// one declined — prepares none, and an absent or empty runnable-bindings
// group is its answer. An environment that selects one needs the group to
// hold it, and any other group that defines no values still fails.
func TestDoctorWorkspaceJudgesTheRunnableBindingsGroupByWhatTheEnvironmentSelects(t *testing.T) {
	deps := []string{generate.RunnableBindingsGroup, "auth0"}
	bindings := "configurations/local/" + generate.RunnableBindingsGroup + ".env"
	const header = "# Generated by `codefly generate runnable-bindings`. DO NOT EDIT: regenerate.\n"
	withFiles := func(fixture map[string]string, files map[string]string) map[string]string {
		for name, content := range files {
			fixture[name] = content
		}
		return fixture
	}
	auth0 := map[string]string{"configurations/local/auth0.env": "CLIENT_ID=public-id\n"}
	requireReady := func(t *testing.T, report *workspaceReadinessReport) {
		t.Helper()
		if report.Status != readinessStatusReady {
			t.Fatalf("status = %q, want ready: %s", report.Status, reportJSON(t, report))
		}
		requireNoCode(t, report, codeConfigurationMissing)
		requireNoCode(t, report, codeConfigurationDirMissing)
		check := findCheck(report, "workspace configuration "+generate.RunnableBindingsGroup)
		if check == nil || check.Status != "ok" || !strings.Contains(check.Message, "prepares no runnable binding") || !strings.Contains(check.Message, "backend/api") {
			t.Fatalf("want an ok check naming the group and its consumer: %s", reportJSON(t, report))
		}
	}

	t.Run("absent, none derived", func(t *testing.T) {
		requireReady(t, runReadiness(t, workspaceReadinessOptions{dir: singleServiceWorkspace(t, testWorkspaceYAML, deps, auth0)}))
	})
	t.Run("absent, no configuration directory", func(t *testing.T) {
		report := runReadiness(t, workspaceReadinessOptions{dir: singleServiceWorkspace(t, testWorkspaceYAML, []string{generate.RunnableBindingsGroup}, nil)})
		requireReady(t, report)
	})
	t.Run("empty, none derived", func(t *testing.T) {
		dir := singleServiceWorkspace(t, testWorkspaceYAML, deps, withFiles(map[string]string{bindings: header}, auth0))
		requireReady(t, runReadiness(t, workspaceReadinessOptions{dir: dir}))
	})
	t.Run("absent, every binding declined", func(t *testing.T) {
		dir := singleServiceWorkspace(t, decliningWorkspace(slottedBindingKey), deps, withFiles(slottedOperationFixture(), auth0))
		report := runReadiness(t, workspaceReadinessOptions{dir: dir})
		requireReady(t, report)
		requireCode(t, report, codeRunnableBindingDeclined, "warn")
	})
	t.Run("absent, a binding selected", func(t *testing.T) {
		dir := singleServiceWorkspace(t, testWorkspaceYAMLLocalDeclared, deps, withFiles(derivedOperationsFixture(), auth0))
		diagnostic := requireCode(t, runReadiness(t, workspaceReadinessOptions{dir: dir}), codeConfigurationMissing, checkStatusFail)
		for _, want := range []string{generate.RunnableBindingsGroup, slottedBindingKey, slotlessBindingKey, "backend/api"} {
			if !strings.Contains(diagnostic.Message, want) {
				t.Fatalf("message %q does not mention %q", diagnostic.Message, want)
			}
		}
		if diagnostic.Remediation != "run `codefly generate runnable-bindings --env local`" {
			t.Fatalf("remediation %q does not name the generation", diagnostic.Remediation)
		}
	})
	t.Run("present, lacking a selected binding", func(t *testing.T) {
		dir := singleServiceWorkspace(t, decliningWorkspace(slottedBindingKey), deps, withFiles(derivedOperationsFixture(), map[string]string{
			bindings:                         header + "# " + slottedBindingKey + ": declined (runnable-scope-selections: []); no binding, no authority\n",
			"configurations/local/auth0.env": "CLIENT_ID=public-id\n",
		}))
		diagnostic := requireCode(t, runReadiness(t, workspaceReadinessOptions{dir: dir}), codeConfigurationMissing, checkStatusFail)
		if !strings.Contains(diagnostic.Message, "lacks 1 runnable binding(s)") || !strings.Contains(diagnostic.Message, slotlessBindingKey) ||
			!strings.Contains(diagnostic.Remediation, "codefly generate runnable-bindings --env local") {
			t.Fatalf("want the missing selected binding and the generation: %+v", diagnostic)
		}
	})
	t.Run("another empty group", func(t *testing.T) {
		dir := singleServiceWorkspace(t, testWorkspaceYAML, deps, map[string]string{
			bindings:                         header,
			"configurations/local/auth0.env": "# no values\n",
		})
		diagnostics := findDiagnostics(runReadiness(t, workspaceReadinessOptions{dir: dir}), codeConfigurationMissing)
		if len(diagnostics) != 1 || diagnostics[0].Status != checkStatusFail ||
			!strings.Contains(diagnostics[0].Message, `"auth0"`) || !strings.Contains(diagnostics[0].Message, "defines no values") {
			t.Fatalf("want one failure, for the empty auth0 group: %+v", diagnostics)
		}
	})
}

// The render's readiness gate is the doctor scoped to the module: a module
// whose service declares runnable-bindings renders in an environment that
// prepares none, with no runnable-bindings file anywhere.
func TestRenderReadinessAcceptsAnAbsentRunnableBindingsGroupWhenNoneIsSelected(t *testing.T) {
	dir := singleServiceWorkspace(t, testWorkspaceYAML, []string{generate.RunnableBindingsGroup}, nil)
	t.Chdir(dir)
	if err := deploy.WorkspaceReadiness(context.Background(), "local", "backend"); err != nil {
		t.Fatalf("the render's readiness gate refused: %v", err)
	}
	out, err := runGitOpsVerb(t, dir, "render", "backend", "--env", "local")
	if err != nil && strings.Contains(err.Error(), "workspace is not ready") {
		t.Fatalf("render was refused by readiness: %v\noutput: %s", err, out)
	}
}
