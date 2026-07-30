package test

import (
	"context"
	"os"
	"testing"

	"github.com/codefly-dev/cli/pkg/orchestration"
	"github.com/codefly-dev/core/resources"
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

func TestStopNilFlowIsSafe(t *testing.T) {
	if err := stopService(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
}

func TestSelectedTestFixtureUsesEnvironmentAndAllowsOverride(t *testing.T) {
	environment := &resources.Environment{Fixture: "dev-admin"}
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
