package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
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

func terminalCommand(cmd *cobra.Command, args []string) (returnErr error) {
	ctx, done := common.NewContext()
	defer done()
	ctx, stop := common.SignalContext(ctx)
	defer stop()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return fmt.Errorf("terminal requires an interactive stdin TTY")
	}

	// Resolve the server address. By default it is derived from the
	// workspace name (the CLI server binds a deterministic per-workspace
	// port in [20000,29900]); the legacy fixed `localhost:10000` is no
	// longer listened on, so a hard-coded default always failed. --server
	// still overrides for unusual setups.
	serverAddress := termServer
	if serverAddress == "" {
		ws, wsErr := resources.FindWorkspaceUp(ctx)
		if wsErr != nil {
			return fmt.Errorf("cannot find workspace to locate the codefly server (pass --server host:port): %w", wsErr)
		}
		if ws == nil {
			return fmt.Errorf("cannot find workspace to locate the codefly server (pass --server host:port)")
		}
		serverAddress = fmt.Sprintf("127.0.0.1:%d", network.CLIServerPort(ws.Name))
	}

	// Connect to the daemon gRPC server
	conn, err := grpc.NewClient(serverAddress, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return fmt.Errorf("cannot connect to codefly server at %s: %w", serverAddress, err)
	}
	defer func() {
		if err := conn.Close(); err != nil {
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
		if _, err := client.Close(cleanupCtx, &cliv0.CloseTerminalRequest{SessionId: sessionID}); err != nil {
			returnErr = errors.Join(returnErr, fmt.Errorf("close incomplete terminal session: %w", err))
		}
	}()
	fmt.Printf("Terminal session %s (%s) in %s\n", sessionID, openResp.Shell, openResp.WorkingDir)

	// Get current terminal size
	width, height, err := term.GetSize(int(os.Stdin.Fd()))
	if err == nil {
		_, _ = client.Resize(ctx, &cliv0.ResizeTerminalRequest{
			SessionId: sessionID,
			Rows:      uint32(height),
			Cols:      uint32(width),
		})
	}

	// Put terminal into raw mode
	oldState, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		return fmt.Errorf("cannot set raw mode: %w", err)
	}
	defer func() {
		if err := term.Restore(int(os.Stdin.Fd()), oldState); err != nil {
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

	// Handle SIGWINCH (terminal resize). Stop + close on exit so both
	// the signal registration and the listener goroutine are released.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGWINCH)
	resizeCtx, resizeCancel := context.WithCancel(ctx)
	var resizeWG sync.WaitGroup
	resizeWG.Add(1)
	defer func() {
		signal.Stop(sigCh)
		resizeCancel()
		resizeWG.Wait()
	}()
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
						Rows:      uint32(h),
						Cols:      uint32(w),
					})
				}
			}
		}
	}()

	receiveDone := make(chan error, 1)

	// Read from server, write to stdout
	go func() {
		for {
			msg, err := stream.Recv()
			if err != nil {
				if errors.Is(err, io.EOF) || errors.Is(err, context.Canceled) {
					receiveDone <- nil
				} else {
					receiveDone <- fmt.Errorf("receive terminal output: %w", err)
				}
				return
			}
			if len(msg.Data) > 0 {
				n, writeErr := os.Stdout.Write(msg.Data)
				if writeErr != nil {
					receiveDone <- fmt.Errorf("write terminal output: %w", writeErr)
					return
				}
				if n != len(msg.Data) {
					receiveDone <- io.ErrShortWrite
					return
				}
			}
			if msg.Done {
				receiveDone <- nil
				return
			}
		}
	}()

	// Read from stdin, send to server. If this reader exits (stdin error or
	// send failure), cancel the context so stream.Recv() in the stdout
	// reader unblocks and that goroutine can close done — otherwise the
	// main goroutine would hang forever on <-done.
	sendDone := make(chan error, 1)
	go func() {
		buf := make([]byte, 1024)
		for {
			n, err := readTerminalInput(ctx, int(os.Stdin.Fd()), buf)
			if n > 0 {
				if sendErr := stream.Send(&cliv0.TerminalInput{
					SessionId: sessionID,
					Data:      buf[:n],
				}); sendErr != nil {
					sendDone <- fmt.Errorf("send terminal input: %w", sendErr)
					return
				}
			}
			if err != nil {
				if errors.Is(err, io.EOF) || errors.Is(err, context.Canceled) {
					sendDone <- nil
				} else {
					sendDone <- fmt.Errorf("read terminal input: %w", err)
				}
				return
			}
		}
	}()

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

func readTerminalInput(ctx context.Context, fd int, buffer []byte) (int, error) {
	for {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
		_, err := unix.Poll(fds, 250)
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
