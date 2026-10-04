package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/codefly-dev/cli/cmd/common"
	cliv0 "github.com/codefly-dev/core/generated/go/codefly/cli/v0"
	"github.com/codefly-dev/core/network"
	"github.com/codefly-dev/core/resources"
	"github.com/spf13/cobra"
	"golang.org/x/sys/unix"
	"golang.org/x/term"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

var (
	termModule  string
	termService string
	termShell   string
	termServer  string
)

// TerminalCmd opens an interactive terminal session via the codefly daemon.
var TerminalCmd = &cobra.Command{
	Use:   "terminal",
	Short: "Open an interactive shell scoped to a workspace resource",
	Long: `Opens a terminal session scoped to a module/service directory.
The session runs inside the codefly daemon and persists across disconnections.

Examples:
  codefly terminal
  codefly terminal --module app --service api
  codefly terminal --shell /bin/zsh`,
	Args: cobra.NoArgs,
	RunE: terminalCommand,
}

func terminalCommand(_ *cobra.Command, _ []string) (returnErr error) {
	ctx, done := common.NewContext()
	defer done()
	ctx, stop := common.SignalContext(ctx)
	defer stop()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return fmt.Errorf("terminal requires an interactive stdin TTY")
	}

	serverAddress, err := resolveTerminalServer(ctx)
	if err != nil {
		return err
	}

	// Connect to the daemon gRPC server
	conn, err := grpc.NewClient(serverAddress, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return fmt.Errorf("cannot connect to codefly server at %s: %w", serverAddress, err)
	}
	defer func() {
		if err = conn.Close(); err != nil {
			returnErr = errors.Join(returnErr, fmt.Errorf("close terminal connection: %w", err))
		}
	}()

	client := cliv0.NewTerminalServiceClient(conn)

	// Open a new session
	openResp, err := client.Open(ctx, &cliv0.OpenTerminalRequest{
		Module:  termModule,
		Service: termService,
		Shell:   termShell,
		Rows:    24,
		Cols:    80,
	})
	if err != nil {
		return fmt.Errorf("cannot open terminal: %w", err)
	}
	if openResp == nil || openResp.SessionId == "" {
		return fmt.Errorf("terminal server returned an empty session")
	}

	sessionID := openResp.SessionId
	keepSession := false
	defer func() {
		if keepSession {
			return
		}
		cleanupCtx, cleanupCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cleanupCancel()
		if _, err = client.Close(cleanupCtx, &cliv0.CloseTerminalRequest{SessionId: sessionID}); err != nil {
			returnErr = errors.Join(returnErr, fmt.Errorf("close incomplete terminal session: %w", err))
		}
	}()
	fmt.Printf("Terminal session %s (%s) in %s\n", sessionID, openResp.Shell, openResp.WorkingDir)

	// Get current terminal size
	width, height, err := term.GetSize(int(os.Stdin.Fd()))
	if err == nil {
		_, _ = client.Resize(ctx, &cliv0.ResizeTerminalRequest{
			SessionId: sessionID,
			Rows:      terminalCells(height),
			Cols:      terminalCells(width),
		})
	}

	// Put terminal into raw mode
	oldState, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		return fmt.Errorf("cannot set raw mode: %w", err)
	}
	defer func() {
		if err = term.Restore(int(os.Stdin.Fd()), oldState); err != nil {
			returnErr = errors.Join(returnErr, fmt.Errorf("restore terminal mode: %w", err))
		}
	}()

	// Attach to the session
	stream, err := client.Attach(ctx)
	if err != nil {
		return fmt.Errorf("cannot attach: %w", err)
	}

	// Send initial message with session ID
	if err := stream.Send(&cliv0.TerminalInput{SessionId: sessionID}); err != nil {
		return fmt.Errorf("cannot send session ID: %w", err)
	}
	keepSession = true

	stopResizing := followTerminalResize(ctx, client, sessionID)
	defer stopResizing()

	receiveDone := make(chan error, 1)
	go receiveTerminalOutput(stream, receiveDone)

	// If the stdin reader exits (stdin error or send failure), cancel the
	// context so stream.Recv() in the stdout reader unblocks and that
	// goroutine can close done — otherwise the main goroutine would hang
	// forever on <-done.
	sendDone := make(chan error, 1)
	go sendTerminalInput(ctx, stream, sessionID, sendDone)

	select {
	case err := <-receiveDone:
		cancel()
		_ = stream.CloseSend()
		<-sendDone
		return err
	case err := <-sendDone:
		cancel()
		_ = stream.CloseSend()
		receiveErr := <-receiveDone
		return errors.Join(err, receiveErr)
	case <-ctx.Done():
		cancel()
		_ = stream.CloseSend()
		<-receiveDone
		<-sendDone
		return ctx.Err()
	}
}

// resolveTerminalServer resolves the server address. By default it is
// derived from the workspace name (the CLI server binds a deterministic
// per-workspace port in [20000,29900]); the legacy fixed `localhost:10000` is
// no longer listened on, so a hard-coded default always failed. --server
// still overrides for unusual setups.
func resolveTerminalServer(ctx context.Context) (string, error) {
	if termServer != "" {
		return termServer, nil
	}
	ws, err := resources.FindWorkspaceUp(ctx)
	if err != nil {
		return "", fmt.Errorf("cannot find workspace to locate the codefly server (pass --server host:port): %w", err)
	}
	if ws == nil {
		return "", fmt.Errorf("cannot find workspace to locate the codefly server (pass --server host:port)")
	}
	return fmt.Sprintf("127.0.0.1:%d", network.CLIServerPort(ws.Name)), nil
}

// followTerminalResize forwards SIGWINCH (terminal resize) to the session
// until the returned stop is called, which releases both the signal
// registration and the listener goroutine.
func followTerminalResize(ctx context.Context, client cliv0.TerminalServiceClient, sessionID string) func() {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGWINCH)
	resizeCtx, resizeCancel := context.WithCancel(ctx)
	var resizeWG sync.WaitGroup
	resizeWG.Add(1)
	go func() {
		defer resizeWG.Done()
		for {
			select {
			case <-resizeCtx.Done():
				return
			case <-sigCh:
				w, h, err := term.GetSize(int(os.Stdin.Fd()))
				if err == nil {
					_, _ = client.Resize(resizeCtx, &cliv0.ResizeTerminalRequest{
						SessionId: sessionID,
						Rows:      terminalCells(h),
						Cols:      terminalCells(w),
					})
				}
			}
		}
	}()
	return func() {
		signal.Stop(sigCh)
		resizeCancel()
		resizeWG.Wait()
	}
}

// receiveTerminalOutput writes what the server sends to stdout and reports
// on done once the stream ends.
func receiveTerminalOutput(stream cliv0.TerminalService_AttachClient, done chan<- error) {
	for {
		msg, err := stream.Recv()
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, context.Canceled) {
				done <- nil
			} else {
				done <- fmt.Errorf("receive terminal output: %w", err)
			}
			return
		}
		if len(msg.Data) > 0 {
			n, writeErr := os.Stdout.Write(msg.Data)
			if writeErr != nil {
				done <- fmt.Errorf("write terminal output: %w", writeErr)
				return
			}
			if n != len(msg.Data) {
				done <- io.ErrShortWrite
				return
			}
		}
		if msg.Done {
			done <- nil
			return
		}
	}
}

// sendTerminalInput reads stdin and sends it to the session, reporting on
// done when stdin ends or a send fails.
func sendTerminalInput(ctx context.Context, stream cliv0.TerminalService_AttachClient, sessionID string, done chan<- error) {
	buf := make([]byte, 1024)
	for {
		n, err := readTerminalInput(ctx, int(os.Stdin.Fd()), buf)
		if n > 0 {
			if sendErr := stream.Send(&cliv0.TerminalInput{
				SessionId: sessionID,
				Data:      buf[:n],
			}); sendErr != nil {
				done <- fmt.Errorf("send terminal input: %w", sendErr)
				return
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, context.Canceled) {
				done <- nil
			} else {
				done <- fmt.Errorf("read terminal input: %w", err)
			}
			return
		}
	}
}

// terminalCells fits a terminal dimension into the resize request; the
// terminal reports no negative size and none is that wide.
func terminalCells(size int) uint32 {
	switch {
	case size < 0:
		return 0
	case size > math.MaxUint32:
		return math.MaxUint32
	}
	return uint32(size)
}

// pollDescriptor fits the descriptor into poll's int32; a negative one is no
// descriptor at all.
func pollDescriptor(fd int) (int32, error) {
	if fd < 0 || fd > math.MaxInt32 {
		return 0, fmt.Errorf("terminal descriptor %d is outside poll's range", fd)
	}
	return int32(fd), nil
}

func readTerminalInput(ctx context.Context, fd int, buffer []byte) (int, error) {
	for {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		descriptor, err := pollDescriptor(fd)
		if err != nil {
			return 0, err
		}
		fds := []unix.PollFd{{Fd: descriptor, Events: unix.POLLIN}}
		_, err = unix.Poll(fds, 250)
		if err != nil {
			if errors.Is(err, unix.EINTR) {
				continue
			}
			return 0, err
		}
		if fds[0].Revents&(unix.POLLIN|unix.POLLHUP|unix.POLLERR) != 0 {
			return unix.Read(fd, buffer)
		}
	}
}

func init() {
	TerminalCmd.Flags().StringVar(&termModule, "module", "", "Module name (optional)")
	TerminalCmd.Flags().StringVar(&termService, "service", "", "Service name (optional)")
	TerminalCmd.Flags().StringVar(&termShell, "shell", "", "Shell override (default: $SHELL)")
	TerminalCmd.Flags().StringVar(&termServer, "server", "", "codefly gRPC server address (default: derived from workspace)")
}
