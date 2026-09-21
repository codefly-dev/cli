//go:build unix

package composition

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	core "github.com/codefly-dev/core/composition"
	"github.com/codefly-dev/core/resources"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

func TestSelectionMetadataRejectsFIFOAndReleasesLock(t *testing.T) {
	for _, name := range []string{core.DescriptorFileName, SelectionFile, LocalSelectionFile} {
		t.Run(name, func(t *testing.T) {
			r := newSelectionRegistry(t)
			release := r.publish(t, selectionManifest("team/root", "1.0.0"))
			session := r.session(t, &core.Descriptor{Kind: core.DescriptorKind, Name: "consumer", Base: core.Base{ID: release.ID, Version: release.Version}})
			_, err := session.Initialize(t.Context(), &SelectionInputs{Root: release})
			require.NoError(t, err)
			path := filepath.Join(session.Root, name)
			require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
			err = os.Remove(path)
			require.True(t, err == nil || os.IsNotExist(err))
			require.NoError(t, syscall.Mkfifo(path, 0o600))
			err = rejectMetadataFIFO(t, path, func(ctx context.Context) error {
				_, inspectErr := session.Inspect(ctx)
				return inspectErr
			})
			require.ErrorContains(t, err, "regular file")
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			require.NoError(t, session.locked(ctx, func() error { return nil }))
		})
	}
}

func TestSelectionMetadataRecheckRejectsFIFOReplacement(t *testing.T) {
	for _, name := range []string{core.DescriptorFileName, SelectionFile, LocalSelectionFile, resources.WorkspaceConfigurationName} {
		t.Run(name, func(t *testing.T) {
			r := newSelectionRegistry(t)
			release := r.publish(t, selectionManifest("team/root", "1.0.0"))
			session := r.session(t, &core.Descriptor{Kind: core.DescriptorKind, Name: "consumer", Base: core.Base{ID: release.ID, Version: release.Version}})
			_, err := session.Initialize(t.Context(), &SelectionInputs{Root: release})
			require.NoError(t, err)
			snapshot, err := session.snapshot(t.Context(), false)
			require.NoError(t, err)
			path := filepath.Join(session.Root, name)
			if name == resources.WorkspaceConfigurationName {
				session.trustPath, session.trustDocument = path, []byte("trusted")
			}
			require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
			err = os.Remove(path)
			require.True(t, err == nil || os.IsNotExist(err))
			require.NoError(t, syscall.Mkfifo(path, 0o600))
			err = rejectMetadataFIFO(t, path, func(ctx context.Context) error {
				return session.locked(ctx, func() error { return session.unchanged(ctx, snapshot) })
			})
			require.ErrorContains(t, err, "regular file")
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			require.NoError(t, session.locked(ctx, func() error { return nil }))
		})
	}
}

func TestSelectionTrustRejectsFIFO(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, resources.WorkspaceConfigurationName)
	require.NoError(t, syscall.Mkfifo(path, 0o600))
	for _, read := range []func(context.Context) error{
		func(context.Context) error { _, _, err := LoadModuleTrust(root); return err },
		func(context.Context) error { _, err := NewSelectionSession(root, root, ""); return err },
	} {
		require.ErrorContains(t, rejectMetadataFIFO(t, path, read), "regular file")
	}
}

func rejectMetadataFIFO(t *testing.T, path string, run func(context.Context) error) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- run(ctx) }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
	}
	// A regressed ReadFile needs both an opening peer and EOF. Drain it before
	// failing the test so no goroutine or product lock is abandoned.
	peer, err := os.OpenFile(path, os.O_WRONLY|syscall.O_NONBLOCK, 0)
	require.NoError(t, err)
	_, err = peer.Write([]byte("{}\n"))
	require.NoError(t, err)
	require.NoError(t, peer.Close())
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("metadata read did not drain")
	}
	t.Fatal("metadata FIFO read outlived cancellation")
	return nil
}

func TestAcquisitionReplacesFIFOWithoutBlockingSelection(t *testing.T) {
	r := newSelectionRegistry(t)
	artifact := r.artifact("runtime", core.ArtifactRuntime, []byte("selected bytes"))
	session := &SelectionSession{Root: t.TempDir(), Engine: &core.Engine{}}
	directory := filepath.Join(session.Root, ".codefly", "selection-artifacts")
	require.NoError(t, os.MkdirAll(directory, 0o700))
	path := filepath.Join(directory, strings.TrimPrefix(artifact.Digest, "sha256:"))
	require.NoError(t, syscall.Mkfifo(path, 0o600))
	err := rejectFIFOWithoutWaiting(t, path, func(ctx context.Context) error {
		return session.locked(ctx, func() error {
			_, acquireErr := session.acquireArtifact(ctx, r.artifacts.Client(), artifact)
			return acquireErr
		})
	})
	require.NoError(t, err)
	require.True(t, artifactMatches(t.Context(), path, artifact.Digest))
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	require.NoError(t, session.locked(ctx, func() error { return nil }))
}

func TestContractArtifactRejectsFIFOAfterAcquisition(t *testing.T) {
	r := newSelectionRegistry(t)
	artifact := r.artifact("contracts", core.ArtifactContracts, []byte(`{"module":"team/module"}`))
	session := &SelectionSession{Root: t.TempDir(), Engine: &core.Engine{}}
	path, err := session.acquireArtifact(t.Context(), r.artifacts.Client(), artifact)
	require.NoError(t, err)
	data, err := readContractArtifact(t.Context(), path)
	require.NoError(t, err)
	require.Equal(t, artifact.Digest, contentDigest(data))
	require.NoError(t, os.Remove(path))
	require.NoError(t, syscall.Mkfifo(path, 0o600))
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- session.locked(ctx, func() error { _, readErr := readContractArtifact(ctx, path); return readErr })
	}()
	select {
	case err = <-done:
		require.ErrorContains(t, err, "regular file")
	case <-ctx.Done():
		// Drain a regressed blocking ReadFile, including its read-until-EOF,
		// before reporting the failure. No abandoned lock-holding goroutine.
		writer, openErr := os.OpenFile(path, os.O_WRONLY|syscall.O_NONBLOCK, 0)
		require.NoError(t, openErr)
		_, writeErr := writer.Write([]byte("drain"))
		require.NoError(t, writeErr)
		require.NoError(t, writer.Close())
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("FIFO writer did not drain blocked contract read")
		}
		t.Fatal("contract cache read outlived cancellation")
	}
	lockCtx, stop := context.WithTimeout(t.Context(), time.Second)
	defer stop()
	require.NoError(t, session.locked(lockCtx, func() error { return nil }))
}

// A regression must drain the blocked syscall before failing, not leave a
// goroutine holding the product lock or hide the hang by starting a FIFO peer.
func rejectFIFOWithoutWaiting(t *testing.T, path string, run func(context.Context) error) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- run(ctx) }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
	}
	select {
	case err := <-done:
		return err
	case <-time.After(300 * time.Millisecond):
	}
	peer, err := os.OpenFile(path, os.O_RDWR|syscall.O_NONBLOCK, 0)
	require.NoError(t, err)
	defer func() { require.NoError(t, peer.Close()) }()
	select {
	case err = <-done:
		t.Fatalf("input open outlived cancellation and required a FIFO peer: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("FIFO peer did not release input open")
	}
	return nil
}

func TestApprovedInputsRejectFIFOAndReleaseSelectionLock(t *testing.T) {
	session, files, approval, checked, _ := approvalUseFixture(t)
	path := filepath.Join(t.TempDir(), "runtime-fifo")
	require.NoError(t, syscall.Mkfifo(path, 0o600))
	files.Runtime[0].Path = path
	err := rejectFIFOWithoutWaiting(t, path, func(ctx context.Context) error {
		prepared, prepareErr := session.PrepareApprovedInputs(ctx, files, approval, checked.AuthorityDigest, time.Now())
		if prepared != nil {
			_ = prepared.Close()
		}
		return prepareErr
	})
	require.ErrorContains(t, err, "regular file")
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	require.NoError(t, session.locked(ctx, func() error { return nil }))
	_, err = session.InspectApprovalUse(ctx, checked.UseIdentity)
	require.ErrorIs(t, err, os.ErrNotExist)
	directory, _, err := session.approvalStorageDirectory("composition-approved-inputs")
	require.NoError(t, err)
	entries, err := os.ReadDir(directory)
	if !os.IsNotExist(err) {
		require.NoError(t, err)
		require.Empty(t, entries)
	}
}

func TestApprovedInputsRejectFIFOReplacementAfterAdmissionAndCleanCopies(t *testing.T) {
	session, files, approval, checked, now := approvalUseFixture(t)
	retained, err := session.PrepareApprovedInputs(t.Context(), files, approval, checked.AuthorityDigest, now)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, retained.Close()) })
	for _, path := range []string{files.Runtime[0].Path,
		filepath.Join(files.Executions[0].Directory, approval.Admission.Record.Executions[0].Outputs[0].Path)} {
		t.Run(filepath.Base(path), func(t *testing.T) {
			// Reproduce a replacement after authenticating the actual source bytes
			// but before the construction/copy phase used by PrepareApprovedInputs.
			_, checkErr := session.CheckApproval(t.Context(), files, approval, time.Now())
			require.NoError(t, checkErr)
			before, readErr := os.ReadFile(path)
			require.NoError(t, readErr)
			require.NoError(t, os.Remove(path))
			require.NoError(t, syscall.Mkfifo(path, 0o600))
			t.Cleanup(func() {
				require.NoError(t, os.Remove(path))
				require.NoError(t, os.WriteFile(path, before, 0o600))
			})
			err = rejectFIFOWithoutWaiting(t, path, func(ctx context.Context) error {
				return session.locked(ctx, func() error {
					prepared, prepareErr := session.newApprovedInputs(ctx, files, approval, checked.AuthorityDigest, &approval.Admission.Record)
					if prepared != nil {
						_ = prepared.Close()
					}
					return prepareErr
				})
			})
			require.ErrorContains(t, err, "regular file")
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
			defer cancel()
			require.NoError(t, session.locked(ctx, func() error { return nil }))
			_, inspectErr := session.InspectApprovalUse(ctx, checked.UseIdentity)
			require.ErrorIs(t, inspectErr, os.ErrNotExist)
			entries, readErr := os.ReadDir(retained.parent.Name())
			require.NoError(t, readErr)
			require.Len(t, entries, 1, "failed construction must remove its partial runtime/render copies only")
			require.Equal(t, retained.name, entries[0].Name())
		})
	}
	_, err = retained.Reserve(t.Context(), time.Now())
	require.NoError(t, err, "failed copying must not spend approval or damage the retained snapshot")
}

func TestInputOpenValidatesNonblockingDescriptorAndRetainsContainment(t *testing.T) {
	directory, err := os.OpenRoot(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, directory.Close()) })
	require.NoError(t, directory.WriteFile("regular", []byte("approved"), 0o600))
	for _, root := range []*os.Root{nil, directory} {
		path := "regular"
		if root == nil {
			path = filepath.Join(directory.Name(), path)
		}
		file, openErr := openInputFile(t.Context(), root, path)
		require.NoError(t, openErr)
		raw, rawErr := file.SyscallConn()
		require.NoError(t, rawErr)
		var flags int
		var flagErr error
		require.NoError(t, raw.Control(func(fd uintptr) { flags, flagErr = unix.FcntlInt(fd, unix.F_GETFL, 0) }))
		require.NoError(t, flagErr)
		require.NotZero(t, flags&unix.O_NONBLOCK, "both absolute and root-relative opens must be nonblocking")
		require.NoError(t, file.Close())
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		file, openErr = openInputFile(ctx, root, path)
		require.ErrorIs(t, openErr, context.Canceled)
		require.Nil(t, file)
	}
	external := filepath.Join(t.TempDir(), "external")
	require.NoError(t, os.WriteFile(external, []byte("not selected"), 0o600))
	require.NoError(t, directory.Symlink(external, "escape"))
	for _, path := range []string{"escape", "../external", "."} {
		file, openErr := openInputFile(t.Context(), directory, path)
		require.Error(t, openErr)
		require.Nil(t, file)
	}
}
