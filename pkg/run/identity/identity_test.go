package identity

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/codefly-dev/core/workloadidentity/native"
	"github.com/stretchr/testify/require"
)

func TestHostProjectionCannotBePresentedToSecondAudience(t *testing.T) {
	launcher, err := Start("example.test", "host", t.TempDir())
	require.NoError(t, err)
	workload := launcher.Workload("example", "api")
	launcher.Declare(workload)
	credential, err := launcher.Project("example/api", workload)
	require.NoError(t, err)
	token, err := os.ReadFile(credential.File)
	require.NoError(t, err)
	attester, err := native.NewAttester(launcher.Anchor())
	require.NoError(t, err)
	_, err = attester.Attest(t.Context(), string(token), "host")
	require.NoError(t, err)
	_, err = attester.Attest(t.Context(), string(token), "second/audience")
	require.Error(t, err, "the existing projection issues only the host audience")
}

func TestAudienceProjectionsRefreshAndForget(t *testing.T) {
	launcher, err := Start("example.test", "host", t.TempDir())
	require.NoError(t, err)
	workload := launcher.Workload("example", "api")
	launcher.Declare(workload)
	host, err := launcher.Project("example/api", workload)
	require.NoError(t, err)
	second, err := launcher.ProjectForAudience("example/api", workload, "https://verifier.example/audience")
	require.NoError(t, err)
	require.NotEqual(t, host.File, second.File)
	require.Equal(t, filepath.Join(launcher.stateDir, "identities", "example", "api", "credential"), host.File)
	require.Equal(t, host.Workload, second.Workload)
	same, err := launcher.ProjectForAudience("example/api", workload, "host")
	require.NoError(t, err)
	require.Equal(t, host, same)
	anchor := filepath.Join(t.TempDir(), "anchor.json")
	require.NoError(t, launcher.WriteAnchor(anchor))
	t.Setenv(native.AnchorEnvironment, anchor)
	attester, err := native.LoadAttester()
	require.NoError(t, err)
	previous := map[string]string{}
	verify := func(refreshed bool) {
		for audience, credential := range map[string]Credential{"host": host, "https://verifier.example/audience": second} {
			token, err := os.ReadFile(credential.File)
			require.NoError(t, err)
			info, err := os.Stat(credential.File)
			require.NoError(t, err)
			require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
			attested, err := attester.Attest(t.Context(), string(token), audience)
			require.NoError(t, err)
			require.Equal(t, credential.Workload, attested.Workload)
			other := "host"
			if audience == "host" {
				other = "https://verifier.example/audience"
			}
			_, err = attester.Attest(t.Context(), string(token), other)
			require.Error(t, err)
			if refreshed {
				require.NotEqual(t, previous[audience], string(token))
			}
			previous[audience] = string(token)
		}
	}
	verify(false)
	require.NoError(t, launcher.Refresh())
	verify(true)
	require.NoError(t, launcher.Forget("example/api"))
	require.NoError(t, launcher.Refresh())
	for _, credential := range []Credential{host, second} {
		require.NoFileExists(t, credential.File)
	}
	_, exists := launcher.Projected("example/api")
	require.False(t, exists)
	// Redeclaration/reprojection cannot retain the previous credential bytes.
	launcher.Declare(workload)
	fresh, err := launcher.Project("example/api", workload)
	require.NoError(t, err)
	token, err := os.ReadFile(fresh.File)
	require.NoError(t, err)
	require.NotEqual(t, previous["host"], string(token))
	require.NoFileExists(t, second.File)
}

func TestProjectionRefusesUndeclaredAndConflictingWorkloads(t *testing.T) {
	launcher, err := Start("example.test", "host", t.TempDir())
	require.NoError(t, err)
	workload := launcher.Workload("example", "api")
	_, err = launcher.ProjectForAudience("example/api", workload, "second")
	require.ErrorContains(t, err, "did not declare")
	launcher.Declare(workload)
	_, err = launcher.ProjectForAudience("../escape", workload, "second")
	require.Error(t, err)
	_, err = launcher.ProjectForAudience("example/api", workload, "")
	require.Error(t, err)
	_, err = launcher.Project("example/api", workload)
	require.NoError(t, err)
	workload.TrustDomain = "other.test"
	_, err = launcher.Project("example/api", workload)
	require.Error(t, err)
}

func TestFailedCleanupRetiresRefreshAndRetriesEveryAudience(t *testing.T) {
	launcher, err := Start("example.test", "host", t.TempDir())
	require.NoError(t, err)
	workload := launcher.Workload("example", "api")
	launcher.Declare(workload)
	first, err := launcher.Project("example/api", workload)
	require.NoError(t, err)
	second, err := launcher.ProjectForAudience("example/api", workload, "second")
	require.NoError(t, err)
	require.NoError(t, os.Remove(first.File))
	require.NoError(t, os.Mkdir(first.File, 0o700))
	blocker := filepath.Join(first.File, "busy")
	require.NoError(t, os.WriteFile(blocker, []byte("fixture"), 0o600))
	require.Error(t, launcher.Forget("example/api"))
	require.NoFileExists(t, second.File)
	require.NoError(t, launcher.Refresh(), "retired paths are not refreshed even when removal fails")
	require.NoFileExists(t, second.File)
	require.NoError(t, os.Remove(blocker))
	require.NoError(t, launcher.RetryCleanup())
	_, err = os.Stat(first.File)
	require.ErrorIs(t, err, os.ErrNotExist)
	require.NoError(t, launcher.RetryCleanup())
	fresh, err := launcher.Project("example/api", workload)
	require.NoError(t, err)
	require.FileExists(t, fresh.File)
	require.NoError(t, launcher.Close())
	require.NoError(t, launcher.Refresh())
	require.NoFileExists(t, fresh.File)
	_, err = launcher.Project("example/api", workload)
	require.ErrorContains(t, err, "closed")
}
