package cmd

import (
	"errors"
	"fmt"
	"os/exec"
	"testing"

	hostprovider "github.com/codefly-dev/cli/pkg/provider"
)

// A wrapped subprocess failure must map to the generic exit code 1, never leak
// the child process's own exit code. *exec.ExitError promotes ExitCode() from
// os.ProcessState, so a bare interface{ ExitCode() int } match in ExitCode
// would have returned the subprocess code (here 3) for any command that wraps a
// failed subprocess — a silent change to the CLI's exit-code contract.
func TestExitCodeIgnoresSubprocessExitError(t *testing.T) {
	runErr := exec.Command("sh", "-c", "exit 3").Run()
	var exitErr *exec.ExitError
	if !errors.As(runErr, &exitErr) {
		t.Fatalf("expected an *exec.ExitError, got %T", runErr)
	}
	if exitErr.ExitCode() != 3 {
		t.Fatalf("subprocess exit code = %d, want 3 (test precondition)", exitErr.ExitCode())
	}

	wrapped := fmt.Errorf("check whether git ignores foo: %w", runErr)
	if got := ExitCode(wrapped); got != 1 {
		t.Fatalf("ExitCode(wrapped *exec.ExitError) = %d, want 1", got)
	}
}

// The `codefly provider` commands carry their stable 0-7 exit code through the
// process boundary's ExitCode mapper, and mark their JSON emission as
// machine-readable so main does not append a human error chain.
func TestProviderResultExitCodeWiring(t *testing.T) {
	cases := []struct {
		status hostprovider.Status
		want   int
	}{
		{hostprovider.StatusPolicyDenied, 3},
		{hostprovider.StatusApprovalRequired, 4},
		{hostprovider.StatusStale, 7},
		{hostprovider.StatusInvalid, 1},
	}
	for _, tc := range cases {
		t.Run(string(tc.status), func(t *testing.T) {
			result := hostprovider.NewResult("setup", "local")
			result.Status = tc.status

			_, err := captureStdout(t, func() error { return result.Emit(true) })

			if err == nil {
				t.Fatalf("a %s outcome must return an error", tc.status)
			}
			if got := ExitCode(err); got != tc.want {
				t.Fatalf("ExitCode(%s) = %d, want %d", tc.status, got, tc.want)
			}
			if !IsMachineReadableError(err) {
				t.Fatalf("a JSON emission must be machine-readable so main does not double-render")
			}
		})
	}
}
