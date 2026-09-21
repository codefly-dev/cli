//go:build unix

package composition

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestCompositionCommandInputsRejectFIFOWithoutWaitingForPeer(t *testing.T) {
	path := filepath.Join(t.TempDir(), "input")
	require.NoError(t, syscall.Mkfifo(path, 0o600))
	for name, read := range map[string]func() error{
		"json":          func() error { var value any; return readJSON(path, &value) },
		"identity key":  func() error { _, err := readCommandFile(path); return err },
		"approval key":  func() error { _, err := readApprovalKey(path); return err },
		"build request": func() error { _, err := readBuildConfiguration(path); return err },
	} {
		t.Run(name, func(t *testing.T) {
			done := make(chan error, 1)
			go func() { done <- read() }()
			select {
			case err := <-done:
				require.Error(t, err)
			case <-time.After(time.Second):
				// Drain a regressed blocking open, so failure cannot strand a worker.
				peer, err := os.OpenFile(path, os.O_RDWR|syscall.O_NONBLOCK, 0)
				require.NoError(t, err)
				require.NoError(t, peer.Close())
				select {
				case <-done:
				case <-time.After(time.Second):
					t.Fatal("reader did not finish after releasing FIFO open")
				}
				t.Fatal("composition input waited for a FIFO peer")
			}
		})
	}
}

func TestCompositionCommandInputsAreBoundedRegularFiles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "input")
	file, err := os.Create(path)
	require.NoError(t, err)
	require.NoError(t, file.Truncate(maxCommandInputBytes+1))
	require.NoError(t, file.Close())
	_, err = readCommandFile(path)
	require.ErrorContains(t, err, "16 MiB")
	require.NoError(t, os.WriteFile(path, []byte(`{"value":"preserved"}`), 0o600))
	var value map[string]string
	require.NoError(t, readJSON(path, &value))
	require.Equal(t, "preserved", value["value"])
	_, err = readCommandFile(filepath.Dir(path))
	require.Error(t, err)
}
