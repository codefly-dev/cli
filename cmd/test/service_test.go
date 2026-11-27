package test

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/cli/pkg/orchestration"
	"github.com/codefly-dev/core/runners/runstate"
	"github.com/stretchr/testify/require"
)

func TestServiceCommandReturnsErrors(t *testing.T) {
	if ServiceCmd.RunE == nil || ServiceCmd.Run != nil {
		t.Fatal("test service must return errors through RunE")
	}
	if err := serviceArgs(ServiceCmd, []string{"one", "two"}); err == nil {
		t.Fatal("test service accepted two service selectors")
	}
}

func TestServiceCommandMissingWorkspaceReturnsError(t *testing.T) {
	previous, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(previous) })
	if err := ServiceCmd.RunE(ServiceCmd, nil); err == nil {
		t.Fatal("test service unexpectedly succeeded outside a workspace")
	}
}

func TestBuildTestRequestCopiesSlices(t *testing.T) {
	previousFilters := testFilters
	testFilters = []string{"Auth"}
	t.Cleanup(func() { testFilters = previousFilters })
	extra := []string{"--shard", "1/2"}
	request := buildTestRequest(extra)
	testFilters[0] = "changed"
	extra[0] = "changed"
	if request.Filters[0] != "Auth" || request.ExtraArgs[0] != "--shard" {
		t.Fatalf("request retained mutable flag slices: %+v", request)
	}
}

// `test solution` delegates to this exact function, so it must stay the shared
// entry point rather than logic trapped inside ServiceCmd's closure. Asserted by
// identity rather than by invoking it: testServiceCommand does no argument
// validation of its own (serviceArgs is cobra's Args hook), so calling it here
// proved nothing about arguments and, without a workspace-free working
// directory, could reach agent spawning from a unit test.
func TestServiceCommandIsTheSharedTestPath(t *testing.T) {
	if ServiceCmd.RunE == nil {
		t.Fatal("test service has no RunE")
	}
	if reflect.ValueOf(ServiceCmd.RunE).Pointer() != reflect.ValueOf(testServiceCommand).Pointer() {
		t.Fatal("test service no longer runs the shared path test solution delegates to")
	}
}

func TestStopNilFlowIsSafe(t *testing.T) {
	if err := stopService(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
}

func TestSelectedTestFixtureUsesEnvironmentAndAllowsOverride(t *testing.T) {
	environment := &environments.Environment{Fixture: "dev-admin"}
	if got := orchestration.SelectedFixture(environment, ""); got != "dev-admin" {
		t.Fatalf("selected fixture = %q, want environment fixture", got)
	}
	if got := orchestration.SelectedFixture(environment, "custom"); got != "custom" {
		t.Fatalf("selected fixture = %q, want explicit override", got)
	}
	if got := orchestration.SelectedFixture(nil, ""); got != "" {
		t.Fatalf("selected fixture without environment = %q, want empty", got)
	}
}

func TestFinishTestInvocationReclaimsEachIsolatedDatabase(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, "cache"))
	t.Setenv("CODEFLY_HOME", filepath.Join(home, "codefly"))
	prepared, err := prepareSourceWorkspace(t.Context(), t.TempDir(), "codefly.dev/go:9.9.9")
	require.NoError(t, err)
	defer prepared.Close()
	root, err := runstate.Root()
	require.NoError(t, err)
	state := func(name, scope string, disposable bool) string {
		dir := filepath.Join(root, "postgres", "run", name)
		require.NoError(t, os.MkdirAll(dir, 0o700))
		holder, err := runstate.Claim(dir, runstate.Run{Workspace: prepared.Workspace.Dir(), NamingScope: scope, Disposable: disposable})
		require.NoError(t, err)
		require.NoError(t, holder.Release())
		return dir
	}
	persistent := state("developer", "dev", false)
	for range 2 {
		started := time.Now()
		env := &environments.Environment{Name: orchestration.LocalEnvironmentName}
		flow, err := orchestration.NewFlow(t.Context(), prepared.Workspace, prepared.Module, prepared.Service, env, orchestration.TestMode)
		require.NoError(t, err)
		flow.WithRuntimeContext("local")
		scope := flow.WithIsolatedInvocation()
		require.NotEmpty(t, scope)
		db := state(scope, scope, true)
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		require.NoError(t, finishTestInvocation(ctx, flow, started))
		require.NoDirExists(t, db)
		require.DirExists(t, persistent)
	}
}
