package ci

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

func TestDisposableCIRefusesPrerequisitesBeforeAnyScheduledAction(t *testing.T) {
	root, workspace := loadSchedulerFixture(t)
	path := filepath.Join(root, "modules/web/services/frontend/service.codefly.yaml")
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, []byte(strings.Replace(string(raw), "- name: gateway", "- name: organization\n      module: management\n      kind: build", 1)), 0600))
	plan := &Plan{Services: []PlannedService{{Service: "management/organization"}, {Service: "web/frontend"}}}
	for _, phase := range []string{"lint", "compile", "test", "build"} {
		t.Run(phase, func(t *testing.T) {
			calls := 0
			err := WithPlanOptions(t.Context(), workspace, plan, func(context.Context, *resources.Workspace, *resources.Module, *resources.Service) error {
				calls++
				return nil
			}, ScheduleOptions{Jobs: 2, Phase: phase, DisposableImages: true})
			require.ErrorContains(t, err, "disposable image input rebinding is unsupported")
			require.Zero(t, calls, "even the producer must not run before rejection")
		})
	}
}

func TestDisposableCISchedulesIndependentServiceWithoutUnrelatedRefusal(t *testing.T) {
	_, workspace := loadSchedulerFixture(t)
	plan := &Plan{Services: []PlannedService{{Service: "management/worker"}}}
	calls := 0
	err := WithPlanOptions(t.Context(), workspace, plan, func(context.Context, *resources.Workspace, *resources.Module, *resources.Service) error {
		calls++
		return nil
	}, ScheduleOptions{Phase: "build", DisposableImages: true})
	require.NoError(t, err)
	require.Equal(t, 1, calls)
}

// This fixture has no exported endpoints, so the withdrawal is independently
// valid. Its raw scheduler graph and selected Flow graph deliberately differ.
func disposableAdmissionWorkspace(t *testing.T, edge string, excluded bool) (string, *resources.Workspace) {
	t.Helper()
	root := t.TempDir()
	workspace := "name: disposable-admission\nlayout: modules\nmodules:\n  - name: app\nenvironments:\n  - name: local\n"
	if excluded {
		workspace += "    exclude-services:\n      - service: app/intermediate\n        withdrawn-from: [app/origin]\n"
	}
	files := map[string]string{
		"workspace.codefly.yaml":          workspace,
		"modules/app/module.codefly.yaml": "name: app\nservices:\n  - name: independent\n  - name: origin\n  - name: intermediate\n  - name: producer\n",
	}
	for _, name := range []string{"independent", "origin", "intermediate", "producer"} {
		declaration := "name: " + name + "\nversion: 0.0.0\nagent:\n  kind: codefly:service\n  publisher: example.test\n  name: missing-admission-peer\n  version: 0.0.1\n"
		if name == "origin" {
			declaration += "service-dependencies:\n  - name: intermediate\n    kind: " + edge + "\n"
		}
		if name == "intermediate" {
			declaration += "service-dependencies:\n  - name: producer\n    kind: build\n"
		}
		files["modules/app/services/"+name+"/service.codefly.yaml"] = declaration
	}
	for name, value := range files {
		path := filepath.Join(root, name)
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0700))
		require.NoError(t, os.WriteFile(path, []byte(value), 0600))
	}
	ws, err := resources.LoadWorkspaceFromDir(t.Context(), root)
	require.NoError(t, err)
	return root, ws
}

func validDisposableReplay(t *testing.T, root string, ws *resources.Workspace, phase string, changed []string) *Plan {
	t.Helper()
	runCacheTestGit(t, root, "init")
	runCacheTestGit(t, root, "add", ".")
	runCacheTestGit(t, root, "-c", "user.name=CI Test", "-c", "user.email=ci@example.com", "commit", "-m", "fixture")
	options := PlanOptions{ChangedFiles: changed}
	plan, err := BuildPlan(t.Context(), ws, &options)
	require.NoError(t, err)
	invocation := ReplayInvocation{Phases: []string{phase}, Suites: []string{"unit"}, RuntimeContext: "free"}
	saved, err := buildReplayPlan(t.Context(), ws, plan, invocation)
	require.NoError(t, err)
	data, err := json.Marshal(saved)
	require.NoError(t, err)
	path := filepath.Join(t.TempDir(), "plan.json")
	require.NoError(t, os.WriteFile(path, data, 0600))
	// Use the same selection entrypoint as ci test --plan, not a forged replay.
	flags := SelectionFlags{changedFiles: changed, planFile: path}
	actual, err := flags.BuildPlan(t.Context(), ws, invocation)
	require.NoError(t, err)
	require.NotNil(t, actual.replay)
	return actual
}

func TestDisposableReplayAdmissionBeforeAnyAction(t *testing.T) {
	root, ws := disposableAdmissionWorkspace(t, "build", false)
	plan := validDisposableReplay(t, root, ws, "test", []string{"modules/app/services/independent/code/input", "modules/app/services/origin/code/input"})
	for _, disposable := range []bool{false, true} {
		calls := 0
		err := WithPlanOptions(t.Context(), ws, plan, func(context.Context, *resources.Workspace, *resources.Module, *resources.Service) error {
			calls++
			return nil
		}, ScheduleOptions{Phase: "test", Suite: "unit", Jobs: 1, DisposableImages: disposable})
		if disposable {
			require.ErrorContains(t, err, "disposable image input rebinding is unsupported")
			require.Zero(t, calls)
		} else {
			require.NoError(t, err)
			require.GreaterOrEqual(t, calls, 2, "ordinary replay remains supported")
		}
	}
}

func TestDisposableAdmissionIncludesScheduledExcludedBridge(t *testing.T) {
	for _, replay := range []bool{false, true} {
		root, ws := disposableAdmissionWorkspace(t, "build", true)
		plan := &Plan{Services: []PlannedService{{Service: "app/origin"}}}
		if replay {
			plan = validDisposableReplay(t, root, ws, "build", []string{"modules/app/services/origin/code/input"})
		}
		// The Flow-only check admits this valid withdrawal; the build scheduler
		// still dispatches producer/intermediate, so whole-request refusal wins.
		require.NoError(t, checkDisposablePlan(t.Context(), ws, plan, "test"))
		require.ErrorContains(t, checkDisposablePlan(t.Context(), ws, plan, "lint", "build"), "build prerequisite")
		calls := 0
		err := WithPlanOptions(t.Context(), ws, plan, func(context.Context, *resources.Workspace, *resources.Module, *resources.Service) error {
			calls++
			return nil
		}, ScheduleOptions{Phase: "build", Jobs: 2, DisposableImages: true})
		require.ErrorContains(t, err, "build prerequisite")
		require.Zero(t, calls)
	}
}

func TestDisposableAdmissionDoesNotTraverseUndispatchedRuntimeBranch(t *testing.T) {
	_, ws := disposableAdmissionWorkspace(t, "runtime", true)
	for _, selected := range []string{"app/origin", "app/independent"} {
		plan := &Plan{Services: []PlannedService{{Service: selected}}}
		calls := 0
		err := WithPlanOptions(t.Context(), ws, plan, func(context.Context, *resources.Workspace, *resources.Module, *resources.Service) error {
			calls++
			return nil
		}, ScheduleOptions{Phase: "build", DisposableImages: true})
		require.NoError(t, err)
		require.Equal(t, 1, calls, "no scheduler or Flow dispatches the withdrawn runtime branch")
	}
}
