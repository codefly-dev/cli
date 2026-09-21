package composition

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestApprovalUseNoRefundAfterLinkedFinalizationFailure(t *testing.T) {
	for _, failure := range []string{"cancellation", "cleanup-denied", "combined"} {
		t.Run(failure, func(t *testing.T) {
			if failure != "cancellation" && os.Geteuid() == 0 {
				t.Skip("root bypasses directory write permission; exercise this test as an unprivileged user")
			}
			parent, err := filepath.EvalSymlinks(t.TempDir())
			require.NoError(t, err)
			path := filepath.Join(parent, "uses", "use.json")
			directory, err := openAuthorityRegistry(path, true)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, directory.Close()) })
			t.Cleanup(func() { require.NoError(t, os.Chmod(filepath.Dir(path), 0o700)) })
			data := bytes.Repeat([]byte("retained-evidence"), 4096)
			file, err := directory.OpenFile("temporary", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
			require.NoError(t, err)
			_, err = file.Write(data)
			require.NoError(t, err)
			require.NoError(t, file.Sync())
			require.NoError(t, file.Close())
			require.NoError(t, directory.Link("temporary", "use.json"))
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if failure != "cleanup-denied" {
				cancel()
			}
			if failure != "cancellation" {
				require.NoError(t, os.Chmod(filepath.Dir(path), 0o500))
			}
			err = finalizeApprovalUse(ctx, directory, "temporary", true, nil)
			require.ErrorIs(t, err, ErrApprovalUseUncertain)
			if failure != "cleanup-denied" {
				require.ErrorIs(t, err, context.Canceled)
			}
			if failure != "cancellation" {
				require.ErrorIs(t, err, os.ErrPermission, "actual directory permissions must deny cleanup")
			}
			require.NoError(t, os.Chmod(filepath.Dir(path), 0o700))
			retained, err := directory.ReadFile("use.json")
			require.NoError(t, err)
			require.Equal(t, data, retained)
			require.ErrorIs(t, publishApprovalUse(t.Context(), directory, "use.json", []byte("retry")), ErrApprovalAlreadyUsed)
			retained, err = directory.ReadFile("use.json")
			require.NoError(t, err)
			require.Equal(t, data, retained)
		})
	}
}

func TestApprovalUseCancellationBeforeLinkDoesNotPublish(t *testing.T) {
	parent, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	directory, err := openAuthorityRegistry(filepath.Join(parent, "uses", "use.json"), true)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, directory.Close()) })
	require.NoError(t, directory.WriteFile("temporary", []byte("unpublished"), 0o600))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	err = finalizeApprovalUse(ctx, directory, "temporary", false, context.Canceled)
	require.ErrorIs(t, err, context.Canceled)
	require.NotErrorIs(t, err, ErrApprovalUseUncertain)
	_, err = directory.Lstat("temporary")
	require.ErrorIs(t, err, os.ErrNotExist)
	_, err = directory.Lstat("use.json")
	require.ErrorIs(t, err, os.ErrNotExist)
	require.NoError(t, publishApprovalUse(t.Context(), directory, "use.json", []byte("first use")))
}
