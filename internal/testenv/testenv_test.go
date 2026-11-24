package testenv_test

import (
	"os"
	"os/exec"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/codefly-dev/cli/internal/testenv"
	"github.com/codefly-dev/cli/pkg/processgroup"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/runners/recoveryscope"
)

// surroundingRun is what a shell opened from a running service carries.
var surroundingRun = []string{
	"CODEFLY_AGENT_TOKEN=bearer",
	"CODEFLY_CONTAINER_RECOVERY_SCOPE=18671:v2:scope:namespace",
	"CODEFLY_DISPOSABLE_STORAGE_SCOPE=18671:v2:scope:namespace",
	"CODEFLY_PROCESS_GROUP_AUTH=secret",
	"CODEFLY_RUN_WORKSPACE=/somewhere/else",
	"CODEFLY__RUNTIME_CONTEXT=native",
	"CODEFLY__ENDPOINT__MODULE__SERVICE__GRPC__GRPC=localhost:1",
	"CODEFLY__SERVICE=runner",
}

// whatATestRunIsConfiguredWith must survive: CI and a contributor configure the
// suite through these.
var whatATestRunIsConfiguredWith = []string{
	"CODEFLY_CONFORMANCE_REQUIRED=1",
	"CODEFLY_HOME=/tmp/home",
	"CODEFLY_TEST_REGISTRY_CACHE=/tmp/cache",
}

const reportEnvironment = "TESTENV_REPORT_ENVIRONMENT"

// TestMain lets the binary re-execute itself as the process whose environment
// is under test: by the time it runs, the package has initialized.
func TestMain(m *testing.M) {
	if os.Getenv(reportEnvironment) != "" {
		for _, entry := range os.Environ() {
			if strings.HasPrefix(entry, "CODEFLY") {
				_, _ = os.Stdout.WriteString(entry + "\n")
			}
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func codeflyEnvironmentOfAChildStartedWith(t *testing.T, environ ...string) []string {
	t.Helper()
	cmd := exec.Command(os.Args[0])
	cmd.Env = append([]string{reportEnvironment + "=1", "PATH=" + os.Getenv("PATH")}, environ...)
	out, err := cmd.Output()
	require.NoError(t, err)
	got := strings.Fields(string(out))
	sort.Strings(got)
	return got
}

func TestATestBinaryStartedUnderARunDoesNotInheritTheRun(t *testing.T) {
	got := codeflyEnvironmentOfAChildStartedWith(t, append(append([]string{}, surroundingRun...), whatATestRunIsConfiguredWith...)...)
	want := append([]string{testenv.Cleared + "=1"}, whatATestRunIsConfiguredWith...)
	sort.Strings(want)
	require.Equal(t, want, got)
}

// A test that sets a run variable on purpose and re-executes its own binary as
// a helper process must find it there.
func TestWhatATestSetsReachesTheProcessesItStarts(t *testing.T) {
	environ := append([]string{testenv.Cleared + "=1"}, surroundingRun...)
	got := codeflyEnvironmentOfAChildStartedWith(t, environ...)
	sort.Strings(environ)
	require.Equal(t, environ, got)
}

func TestTheNamesAreTheOwnersNames(t *testing.T) {
	require.Contains(t, testenv.RunMarkers, recoveryscope.EnvironmentVariable)
	require.Contains(t, testenv.RunMarkers, processgroup.LaunchWorkspaceEnv)
	for _, injected := range []string{
		resources.WorkspacePrefix, resources.ModulePrefix, resources.ServicePrefix, resources.VersionPrefix,
		resources.EnvironmentPrefix, resources.NamingScopePrefix, resources.RuntimeContextPrefix,
		resources.EndpointPrefix, resources.SelfEndpointPrefix, resources.RunningPrefix,
		resources.WorkspaceConfigurationPrefix, resources.ServiceConfigurationPrefix,
	} {
		require.True(t, strings.HasPrefix(injected, testenv.InjectedPrefix), injected)
	}
}
