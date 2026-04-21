package go_grpc

import (
	"context"
	"os"
	"testing"
	"time"

	cliv0 "github.com/codefly-dev/core/generated/go/codefly/cli/v0"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTerminalServer_OpenAndList(t *testing.T) {
	srv := NewTerminalServer(os.TempDir())

	resp, err := srv.Open(context.Background(), &cliv0.OpenTerminalRequest{
		Rows: 24,
		Cols: 80,
	})
	require.NoError(t, err)
	assert.NotEmpty(t, resp.SessionId)
	assert.NotEmpty(t, resp.Shell)
	assert.NotEmpty(t, resp.WorkingDir)

	// List should return the session
	listResp, err := srv.List(context.Background(), &cliv0.ListTerminalsRequest{})
	require.NoError(t, err)
	require.Len(t, listResp.Terminals, 1)
	assert.Equal(t, resp.SessionId, listResp.Terminals[0].SessionId)

	// Cleanup
	_, err = srv.Close(context.Background(), &cliv0.CloseTerminalRequest{SessionId: resp.SessionId})
	require.NoError(t, err)
}

func TestTerminalServer_Close(t *testing.T) {
	srv := NewTerminalServer(os.TempDir())

	resp, err := srv.Open(context.Background(), &cliv0.OpenTerminalRequest{})
	require.NoError(t, err)

	_, err = srv.Close(context.Background(), &cliv0.CloseTerminalRequest{SessionId: resp.SessionId})
	require.NoError(t, err)

	// List should be empty
	listResp, err := srv.List(context.Background(), &cliv0.ListTerminalsRequest{})
	require.NoError(t, err)
	assert.Empty(t, listResp.Terminals)

	// Close again should error
	_, err = srv.Close(context.Background(), &cliv0.CloseTerminalRequest{SessionId: resp.SessionId})
	assert.Error(t, err)
}

func TestTerminalServer_CloseNotFound(t *testing.T) {
	srv := NewTerminalServer(os.TempDir())
	_, err := srv.Close(context.Background(), &cliv0.CloseTerminalRequest{SessionId: "nonexistent"})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "session not found")
}

func TestTerminalServer_Resize(t *testing.T) {
	srv := NewTerminalServer(os.TempDir())

	resp, err := srv.Open(context.Background(), &cliv0.OpenTerminalRequest{
		Rows: 24,
		Cols: 80,
	})
	require.NoError(t, err)

	_, err = srv.Resize(context.Background(), &cliv0.ResizeTerminalRequest{
		SessionId: resp.SessionId,
		Rows:      40,
		Cols:      120,
	})
	require.NoError(t, err)

	// Cleanup
	_, err = srv.Close(context.Background(), &cliv0.CloseTerminalRequest{SessionId: resp.SessionId})
	require.NoError(t, err)
}

func TestTerminalServer_ResizeNotFound(t *testing.T) {
	srv := NewTerminalServer(os.TempDir())
	_, err := srv.Resize(context.Background(), &cliv0.ResizeTerminalRequest{
		SessionId: "nonexistent",
		Rows:      40,
		Cols:      120,
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "session not found")
}

func TestTerminalServer_OpenBadDir(t *testing.T) {
	srv := NewTerminalServer("/nonexistent/path/that/should/not/exist")

	_, err := srv.Open(context.Background(), &cliv0.OpenTerminalRequest{
		Module: "bad",
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "working directory not found")
}

func TestTerminalServer_MultipleSessions(t *testing.T) {
	srv := NewTerminalServer(os.TempDir())

	resp1, err := srv.Open(context.Background(), &cliv0.OpenTerminalRequest{})
	require.NoError(t, err)

	resp2, err := srv.Open(context.Background(), &cliv0.OpenTerminalRequest{})
	require.NoError(t, err)

	assert.NotEqual(t, resp1.SessionId, resp2.SessionId)

	listResp, err := srv.List(context.Background(), &cliv0.ListTerminalsRequest{})
	require.NoError(t, err)
	assert.Len(t, listResp.Terminals, 2)

	// Close both
	srv.Shutdown()

	listResp, err = srv.List(context.Background(), &cliv0.ListTerminalsRequest{})
	require.NoError(t, err)
	assert.Empty(t, listResp.Terminals)
}

func TestTerminalServer_Cleanup(t *testing.T) {
	srv := NewTerminalServer(os.TempDir())

	resp, err := srv.Open(context.Background(), &cliv0.OpenTerminalRequest{
		Shell: "/bin/sh",
	})
	require.NoError(t, err)

	// Kill the process so it exits
	srv.mu.RLock()
	sess := srv.sessions[resp.SessionId]
	srv.mu.RUnlock()
	require.NotNil(t, sess)
	_ = sess.Cmd.Process.Kill()

	// Wait for process to exit
	time.Sleep(100 * time.Millisecond)

	// Cleanup should remove the dead session
	srv.Cleanup()

	listResp, err := srv.List(context.Background(), &cliv0.ListTerminalsRequest{})
	require.NoError(t, err)
	assert.Empty(t, listResp.Terminals)
}

func TestTerminalServer_DefaultShell(t *testing.T) {
	srv := NewTerminalServer(os.TempDir())

	resp, err := srv.Open(context.Background(), &cliv0.OpenTerminalRequest{})
	require.NoError(t, err)

	// Should use $SHELL or fallback to /bin/sh
	assert.NotEmpty(t, resp.Shell)

	srv.Shutdown()
}

func TestTerminalServer_CustomShell(t *testing.T) {
	srv := NewTerminalServer(os.TempDir())

	resp, err := srv.Open(context.Background(), &cliv0.OpenTerminalRequest{
		Shell: "/bin/sh",
	})
	require.NoError(t, err)
	assert.Equal(t, "/bin/sh", resp.Shell)

	srv.Shutdown()
}
