package cmd

import (
	"testing"

	hostprovider "github.com/codefly-dev/cli/pkg/provider"
)

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
