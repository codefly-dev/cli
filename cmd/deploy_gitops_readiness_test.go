package cmd

import (
	"context"
	"strings"
	"testing"

	"github.com/codefly-dev/cli/cmd/deploy"
	"github.com/codefly-dev/core/resources"
)

// runGitOpsVerb drives the real command tree the way an operator does, from
// inside a workspace directory, and returns what the command printed.
func runGitOpsVerb(t *testing.T, dir string, args ...string) (string, error) {
	t.Helper()
	t.Setenv(resources.CodeflyHomeEnv, t.TempDir())
	t.Chdir(dir)
	RootCmd.SetArgs(append([]string{"deploy", "gitops"}, args...))
	defer RootCmd.SetArgs(nil)
	return captureStdout(t, func() error {
		return RootCmd.ExecuteContext(context.Background())
	})
}

// A GitOps delivery verb must evaluate the `doctor workspace` verdict for the
// target environment before it does any expensive work. The regression it
// guards: `deploy gitops render` used to resolve modules, drive the builder
// agents and push container images for minutes, and only then die deep inside
// the builder with "no configuration found for <group>" — rediscovering, from a
// failed render, exactly what the readiness check names in seconds.
func TestGitOpsRenderRefusesANotReadyWorkspace(t *testing.T) {
	// The directory exists and holds an unrelated group: the failure is the
	// missing group itself, the shape an operator actually meets.
	dir := singleServiceWorkspace(t, testWorkspaceYAML, []string{"openrouter"}, map[string]string{
		"configurations/local/unrelated.env": "X=1\n",
	})

	out, err := runGitOpsVerb(t, dir, "render", "backend", "--env", "local")
	if err == nil {
		t.Fatalf("render of a not-ready workspace exited without error\noutput: %s", out)
	}
	if !strings.Contains(out, "NOT ready") {
		t.Fatalf("render did not print the readiness diagnostics\noutput: %s", out)
	}
	if !strings.Contains(out, "openrouter") {
		t.Fatalf("render did not name the missing configuration\noutput: %s", out)
	}
	// The doctor's own message and fix hint travel in the error too, so a CI
	// log that keeps only the last line still says what to do.
	message := err.Error()
	for _, want := range []string{"openrouter", "configuration_missing", "fix:", "--" + deploy.SkipWorkspaceReadinessFlag} {
		if !strings.Contains(message, want) {
			t.Fatalf("refusal %q does not carry %q", message, want)
		}
	}
}

// The gate is a precondition of the delivery verbs, not of the whole GitOps
// surface: the same refusal must come from `snapshot`, `plan` and `publish`.
func TestGitOpsDeliveryVerbsRefuseANotReadyWorkspace(t *testing.T) {
	for _, verb := range []string{"snapshot", "plan", "publish"} {
		t.Run(verb, func(t *testing.T) {
			dir := singleServiceWorkspace(t, testWorkspaceYAML, []string{"openrouter"}, nil)
			out, err := runGitOpsVerb(t, dir, verb, "backend", "--env", "local")
			if err == nil {
				t.Fatalf("%s of a not-ready workspace exited without error\noutput: %s", verb, out)
			}
			if !strings.Contains(out, "NOT ready") || !strings.Contains(out, "openrouter") {
				t.Fatalf("%s did not refuse on the readiness verdict\noutput: %s\nerror: %v", verb, out, err)
			}
		})
	}
}

// A ready workspace is not refused: the verb proceeds past the gate and fails,
// if at all, on its own work. Without this the gate could pass its other tests
// by refusing everything.
func TestGitOpsRenderDoesNotRefuseAReadyWorkspace(t *testing.T) {
	dir := singleServiceWorkspace(t, testWorkspaceYAML, []string{"openrouter"}, map[string]string{
		"configurations/local/openrouter.env": "API_KEY=abc\n",
	})

	out, err := runGitOpsVerb(t, dir, "render", "backend", "--env", "local")
	if err != nil && strings.Contains(err.Error(), "workspace is not ready") {
		t.Fatalf("a ready workspace was refused: %v\noutput: %s", err, out)
	}
	if strings.Contains(out, "NOT ready") {
		t.Fatalf("a ready workspace printed a not-ready verdict\noutput: %s", out)
	}
}

// Scoping: rendering one module must not be blocked by a configuration only a
// sibling module requires. The gate asks the readiness engine about the module
// named on the command line, and nothing else.
func TestGitOpsRenderIsNotBlockedByASiblingModulesConfiguration(t *testing.T) {
	dir := writeTestWorkspace(t, map[string]string{
		"workspace.codefly.yaml":                            "name: demo\nlayout: modules\nmodules:\n    - name: backend\n    - name: sibling\n",
		"modules/backend/module.codefly.yaml":               testModuleYAML("api"),
		"modules/backend/services/api/service.codefly.yaml": testServiceYAML("api"),
		"modules/sibling/module.codefly.yaml":               "kind: module\nname: sibling\nservices:\n    - name: worker\n",
		"modules/sibling/services/worker/service.codefly.yaml": strings.Replace(
			testServiceYAML("worker", "openrouter"), "module: backend", "module: sibling", 1),
	})

	// Unscoped, the workspace is not ready — the sibling's group is missing.
	whole := runReadiness(t, workspaceReadinessOptions{dir: dir})
	if whole.Status != readinessStatusNotReady {
		t.Fatalf("workspace-wide readiness = %q, want not_ready\n%s", whole.Status, reportJSON(t, whole))
	}
	requireCode(t, whole, codeConfigurationMissing, "fail")

	// Scoped to backend, which requires nothing, it is.
	scoped := runReadiness(t, workspaceReadinessOptions{dir: dir, module: "backend"})
	if scoped.Status != readinessStatusReady {
		t.Fatalf("module-scoped readiness = %q, want ready\n%s", scoped.Status, reportJSON(t, scoped))
	}
	if scoped.Module != "backend" {
		t.Fatalf("report module = %q, want backend", scoped.Module)
	}

	// And the verb refuses only for the module that actually requires it.
	out, err := runGitOpsVerb(t, dir, "render", "backend", "--env", "local")
	if err != nil && strings.Contains(err.Error(), "workspace is not ready") {
		t.Fatalf("rendering backend was blocked by the sibling's configuration: %v\noutput: %s", err, out)
	}
	out, err = runGitOpsVerb(t, dir, "render", "sibling", "--env", "local")
	if err == nil || !strings.Contains(out, "openrouter") {
		t.Fatalf("rendering sibling was not refused on its own missing configuration\noutput: %s\nerror: %v", out, err)
	}
}

// An unknown module is named as such rather than silently widening the scope
// back to the whole workspace.
func TestReadinessScopedToAnUnknownModule(t *testing.T) {
	dir := singleServiceWorkspace(t, testWorkspaceYAML, nil, nil)
	report := runReadiness(t, workspaceReadinessOptions{dir: dir, module: "ghost"})
	if report.Status != readinessStatusNotReady {
		t.Fatalf("status = %q, want not_ready", report.Status)
	}
	requireCode(t, report, codeModuleNotFound, "fail")
}
