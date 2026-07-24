package control

import (
	"context"
	"strings"
	"testing"
)

func TestRunChecksRunsCommandInServiceDir(t *testing.T) {
	t.Chdir(writeWorkspace(t))
	res, err := New().RunChecks(context.Background(), CheckRequest{
		Service: "backend/api",
		Command: "echo checks_ran",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Passed {
		t.Errorf("expected pass, got %+v", res)
	}
	if !strings.Contains(res.Output, "checks_ran") {
		t.Errorf("output = %q, want it to contain checks_ran", res.Output)
	}
}

func TestRunChecksReportsFailureAsResult(t *testing.T) {
	t.Chdir(writeWorkspace(t))
	// `false` exits non-zero; that's a failing check, not a call error.
	res, err := New().RunChecks(context.Background(), CheckRequest{Service: "backend/api", Command: "false"})
	if err != nil {
		t.Fatalf("unexpected call error: %v", err)
	}
	if res.Passed {
		t.Error("expected a failing check for `false`")
	}
}

func TestAddressesErrorsWhenNothingRunning(t *testing.T) {
	t.Chdir(writeWorkspace(t))
	// No flow registered → addresses are unavailable.
	if _, err := New().Addresses(context.Background(), "backend/api"); err == nil {
		t.Error("expected an error when no flow is running")
	}
}
