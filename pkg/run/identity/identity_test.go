package identity

import (
	"errors"
	"os"
	"testing"
	"time"

	"github.com/codefly-dev/core/workloadidentity/native"
	"github.com/stretchr/testify/require"
)

func projectTestCredential(t *testing.T, launcher *Launcher, service string) Credential {
	t.Helper()
	workload := launcher.Workload("example", service)
	launcher.Declare(workload)
	credential, err := launcher.Project(workload.Unique(), workload)
	require.NoError(t, err)
	return credential
}

func readCredential(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	return data
}

func TestRefreshUsesWallClockAfterSleep(t *testing.T) {
	launcher, err := Start("example.test", "host", t.TempDir())
	require.NoError(t, err)
	credential := projectTestCredential(t, launcher, "scorer")
	original := string(readCredential(t, credential.File))

	// A short poll must not rotate an otherwise fresh credential.
	require.NoError(t, launcher.Refresh(time.Now().Add(5*time.Second)))
	require.True(t, original == string(readCredential(t, credential.File)), "fresh credential was rewritten")

	// Only wall time advances during this simulated sleep: no intervening
	// ticks or elapsed monotonic timer are needed to notice the expiration.
	wake := time.Unix(time.Now().Unix()+int64((15*time.Minute)/time.Second), 0)
	require.NoError(t, launcher.Refresh(wake))
	require.True(t, original != string(readCredential(t, credential.File)), "expired credential was not renewed on wake")
}

func TestRefreshThreshold(t *testing.T) {
	launcher, err := Start("example.test", "host", t.TempDir())
	require.NoError(t, err)
	credential := projectTestCredential(t, launcher, "scorer")
	original := string(readCredential(t, credential.File))
	due := credential.expiresAt.Add(-(native.TokenLifetime - native.RefreshInterval))
	require.NoError(t, launcher.Refresh(due.Add(-time.Second)))
	require.True(t, original == string(readCredential(t, credential.File)), "rotated before threshold")
	require.NoError(t, launcher.Refresh(due))
	require.True(t, original != string(readCredential(t, credential.File)), "did not rotate at threshold")
}

func TestRefreshReportsFailureContinuesAndRetries(t *testing.T) {
	launcher, err := Start("example.test", "host", t.TempDir())
	require.NoError(t, err)
	broken := projectTestCredential(t, launcher, "broken")
	healthy := projectTestCredential(t, launcher, "healthy")
	original := string(readCredential(t, healthy.File))
	require.NoError(t, os.Remove(broken.File))
	require.NoError(t, os.Mkdir(broken.File, 0o700)) // Atomic replacement cannot replace a directory.
	wake := time.Now().Add(15 * time.Minute)
	err = launcher.Refresh(wake)
	require.Error(t, err)
	require.ErrorContains(t, err, broken.Workload.Unique())
	require.ErrorContains(t, err, broken.File)
	var filesystemError *os.LinkError
	require.True(t, errors.As(err, &filesystemError), "filesystem reason was lost")
	require.ErrorContains(t, err, filesystemError.Err.Error())
	require.True(t, original != string(readCredential(t, healthy.File)), "one failure prevented another renewal")
	afterFailure, ok := launcher.Projected(broken.Workload.Unique())
	require.True(t, ok)
	require.Equal(t, broken.expiresAt, afterFailure.expiresAt, "failed write advanced expiration")
	require.NoError(t, os.Remove(broken.File))
	require.NoError(t, launcher.Refresh(wake.Add(5*time.Second)))
	require.NotEmpty(t, readCredential(t, broken.File))
}
