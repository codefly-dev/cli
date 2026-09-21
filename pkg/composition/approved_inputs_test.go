package composition

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/codefly-dev/cli/pkg/internal/selectionguard"
	"github.com/stretchr/testify/require"
)

func TestApprovedInputsRetainExactBytesAndConsumeWithoutReopeningSources(t *testing.T) {
	session, files, approval, checked, now := approvalUseFixture(t)
	runtimeInput := files.Runtime[0]
	runtimeBytes, err := os.ReadFile(runtimeInput.Path)
	require.NoError(t, err)
	execution := approval.Admission.Record.Executions[0]
	output := execution.Outputs[0]
	outputBytes, err := os.ReadFile(filepath.Join(files.Executions[0].Directory, output.Path))
	require.NoError(t, err)
	prepared, err := session.PrepareApprovedInputs(t.Context(), files, approval, checked.AuthorityDigest, now)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, prepared.Close()) })
	path := prepared.directory.Name()
	require.True(t, filepath.IsAbs(path))
	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o700), info.Mode().Perm())
	for _, input := range files.Runtime {
		require.NoError(t, os.Remove(input.Path))
	}
	for _, input := range files.Executions {
		require.NoError(t, os.RemoveAll(input.Directory))
	}
	// Caller-owned evidence and paths must not alias the retained snapshot.
	files.Bindings["target"] = "changed"
	files.Qualifications[0].Statement[0] ^= 1
	files.Executions[0].Receipt[0] ^= 1
	files.Runtime[0].Path = "missing"
	approval.Authorization = "changed"
	approval.Admission.Record.Executions[0].Outputs[0].Path = "missing"
	for _, input := range []struct {
		open func() (*os.File, error)
		want []byte
	}{
		{func() (*os.File, error) { return prepared.OpenRuntime(runtimeInput.Target, runtimeInput.Name) }, runtimeBytes},
		{func() (*os.File, error) {
			return prepared.OpenRenderOutput(execution.Target, execution.Service, output.Name)
		}, outputBytes},
	} {
		file, openErr := input.open()
		require.NoError(t, openErr)
		data, readErr := io.ReadAll(file)
		require.NoError(t, readErr)
		require.Equal(t, input.want, data)
		_, writeErr := file.Write([]byte("cannot write"))
		require.Error(t, writeErr)
		require.NoError(t, file.Close())
	}
	_, err = prepared.OpenRuntime("unselected", runtimeInput.Name)
	require.Error(t, err)
	_, err = prepared.OpenRenderOutput(execution.Target, execution.Service, "../undeclared")
	require.Error(t, err)
	used, err := prepared.Reserve(t.Context(), time.Now())
	require.NoError(t, err)
	require.Equal(t, checked.AdmissionIdentity, used.Approval.AdmissionIdentity)
	require.ErrorIs(t, selectionguard.RejectUnboundExecution(session.Root), selectionguard.ErrUnboundExecution)
	require.NoError(t, prepared.Close())
	_, err = os.Stat(path)
	require.ErrorIs(t, err, os.ErrNotExist)
	_, err = prepared.OpenRuntime(runtimeInput.Target, runtimeInput.Name)
	require.ErrorIs(t, err, os.ErrClosed)
	_, err = prepared.Reserve(t.Context(), time.Now())
	require.ErrorIs(t, err, os.ErrClosed)
	retained, err := session.InspectApprovalUse(t.Context(), checked.UseIdentity)
	require.NoError(t, err)
	require.Equal(t, used, retained, "snapshot cleanup never refunds consumption")
}

func TestApprovedInputsRejectDriftBeforePreparationAndBeforeConsumption(t *testing.T) {
	session, files, approval, checked, now := approvalUseFixture(t)
	original, err := os.ReadFile(files.Runtime[0].Path)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(files.Runtime[0].Path, []byte("private patch"), 0o600))
	prepared, err := session.PrepareApprovedInputs(t.Context(), files, approval, checked.AuthorityDigest, now)
	require.Error(t, err)
	require.Nil(t, prepared)
	require.NoError(t, os.WriteFile(files.Runtime[0].Path, original, 0o600))
	prepared, err = session.PrepareApprovedInputs(t.Context(), files, approval, checked.AuthorityDigest, now)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, prepared.Close()) })
	for _, path := range []string{prepared.files.Runtime[0].Path,
		filepath.Join(prepared.files.Executions[0].Directory, approval.Admission.Record.Executions[0].Outputs[0].Path)} {
		before, readErr := os.ReadFile(path)
		require.NoError(t, readErr)
		require.NoError(t, os.WriteFile(path, []byte("private snapshot drift"), 0o600))
		_, err = prepared.Reserve(t.Context(), now)
		require.Error(t, err)
		_, err = session.InspectApprovalUse(t.Context(), checked.UseIdentity)
		require.ErrorIs(t, err, os.ErrNotExist)
		require.NoError(t, os.WriteFile(path, before, 0o600))
	}
	_, err = prepared.Reserve(t.Context(), now)
	require.NoError(t, err)
}

func TestApprovedInputsRejectMissingExtraAndSymlinkRenderFiles(t *testing.T) {
	session, files, approval, checked, now := approvalUseFixture(t)
	directory := files.Executions[0].Directory
	path := filepath.Join(directory, approval.Admission.Record.Executions[0].Outputs[0].Path)
	before, err := os.ReadFile(path)
	require.NoError(t, err)
	for _, state := range []string{"missing", "extra", "symlink"} {
		t.Run(state, func(t *testing.T) {
			switch state {
			case "missing":
				require.NoError(t, os.Remove(path))
				t.Cleanup(func() { require.NoError(t, os.WriteFile(path, before, 0o600)) })
			case "extra":
				extra := filepath.Join(directory, "undeclared-output")
				require.NoError(t, os.WriteFile(extra, []byte("extra"), 0o600))
				t.Cleanup(func() { require.NoError(t, os.Remove(extra)) })
			case "symlink":
				external := filepath.Join(t.TempDir(), "output")
				require.NoError(t, os.WriteFile(external, before, 0o600))
				require.NoError(t, os.Remove(path))
				require.NoError(t, os.Symlink(external, path))
				t.Cleanup(func() {
					require.NoError(t, os.Remove(path))
					require.NoError(t, os.WriteFile(path, before, 0o600))
				})
			}
			prepared, prepareErr := session.PrepareApprovedInputs(t.Context(), files, approval, checked.AuthorityDigest, now)
			require.Error(t, prepareErr)
			require.Nil(t, prepared)
			_, inspectErr := session.InspectApprovalUse(t.Context(), checked.UseIdentity)
			require.ErrorIs(t, inspectErr, os.ErrNotExist)
		})
	}
}

func TestApprovedInputsRecheckAuthoritySelectionExpiryAndCancellation(t *testing.T) {
	session, files, approval, checked, now := approvalUseFixture(t)
	prepared, err := session.PrepareApprovedInputs(t.Context(), files, approval, checked.AuthorityDigest, now)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, prepared.Close()) })
	_, err = prepared.Reserve(t.Context(), checked.ExpiresAt)
	require.ErrorContains(t, err, "expired")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = prepared.Reserve(ctx, now)
	require.ErrorIs(t, err, context.Canceled)
	_, err = session.PrepareApprovedInputs(ctx, files, approval, checked.AuthorityDigest, now)
	require.ErrorIs(t, err, context.Canceled)
	configuration := session.ConfigurationIdentity
	session.ConfigurationIdentity = contentDigest([]byte("new configuration"))
	_, err = prepared.Reserve(t.Context(), now)
	require.Error(t, err)
	session.ConfigurationIdentity = configuration
	authority, err := session.loadApprovalAuthority()
	require.NoError(t, err)
	authority.config.Audience = "different host policy"
	_, err = session.ConfigureApprovalAuthority(t.Context(), &authority.config, checked.AuthorityDigest)
	require.NoError(t, err)
	_, err = prepared.Reserve(t.Context(), now)
	require.ErrorContains(t, err, "independently reviewed authority digest")
	_, err = session.InspectApprovalUse(t.Context(), checked.UseIdentity)
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestApprovedInputsConcurrentCopiesAndCleanupAreIndependent(t *testing.T) {
	session, files, approval, checked, now := approvalUseFixture(t)
	var snapshots [2]*ApprovedInputs
	var errs [2]error
	var wg sync.WaitGroup
	for i := range snapshots {
		wg.Go(func() {
			snapshots[i], errs[i] = session.PrepareApprovedInputs(t.Context(), files, approval, checked.AuthorityDigest, now)
		})
	}
	wg.Wait()
	for i := range snapshots {
		require.NoError(t, errs[i])
		t.Cleanup(func() { require.NoError(t, snapshots[i].Close()) })
	}
	require.NotEqual(t, snapshots[0].directory.Name(), snapshots[1].directory.Name())
	require.NoError(t, snapshots[0].Close())
	_, err := snapshots[1].Reserve(t.Context(), now)
	require.NoError(t, err)
	_, err = session.ReserveApproval(t.Context(), files, approval, checked.AuthorityDigest, now)
	require.ErrorIs(t, err, ErrApprovalAlreadyUsed)
}

func TestApprovedInputsCleanupFailureCanRetry(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permission denial")
	}
	session, files, approval, checked, now := approvalUseFixture(t)
	prepared, err := session.PrepareApprovedInputs(t.Context(), files, approval, checked.AuthorityDigest, now)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, prepared.Close()) })
	parent := prepared.parent.Name()
	t.Cleanup(func() { require.NoError(t, os.Chmod(parent, 0o700)) })
	require.NoError(t, os.Chmod(parent, 0o500))
	require.ErrorIs(t, prepared.Close(), os.ErrPermission)
	require.False(t, prepared.closed)
	require.NoError(t, os.Chmod(parent, 0o700))
	require.NoError(t, prepared.Close())
}

func TestCopyApprovedInputRejectsInvalidFilesAndCancellation(t *testing.T) {
	directory, err := os.OpenRoot(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, directory.Close()) })
	for _, name := range []string{"oversized", "directory", "canceled"} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "input")
			if name == "directory" {
				require.NoError(t, os.Mkdir(path, 0o700))
			} else {
				require.NoError(t, os.WriteFile(path, []byte("input"), 0o600))
			}
			if name == "oversized" {
				require.NoError(t, os.Truncate(path, maxSelectionArtifactBytes+1))
			}
			source, openErr := os.Open(path)
			require.NoError(t, openErr)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if name == "canceled" {
				cancel()
			}
			require.Error(t, copyApprovedInput(ctx, source, directory, name))
			_, statErr := source.Stat()
			require.ErrorIs(t, statErr, os.ErrClosed)
		})
	}
}
