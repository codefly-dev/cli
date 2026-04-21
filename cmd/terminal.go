package cmd

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/codefly-dev/cli/pkg/cli"
	cliv0 "github.com/codefly-dev/core/generated/go/codefly/cli/v0"
	"github.com/spf13/cobra"
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
	Short: "Open an interactive terminal session",
	Long: `Opens a terminal session scoped to a module/service directory.
The session runs inside the codefly daemon and persists across disconnections.

Examples:
  codefly terminal
  codefly terminal --module app --service api
  codefly terminal --shell /bin/zsh`,
	Run: func(cmd *cobra.Command, args []string) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// Connect to the daemon gRPC server
		conn, err := grpc.NewClient(termServer, grpc.WithTransportCredentials(insecure.NewCredentials()))
		if err != nil {
			cli.Error("Cannot connect to codefly server at %s: %v", termServer, err)
			cli.ExitError()
		}
		defer conn.Close()

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
			cli.Error("Cannot open terminal: %v", err)
			cli.ExitError()
		}

		sessionID := openResp.SessionId
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
			cli.Error("Cannot set raw mode: %v", err)
			cli.ExitError()
		}
		defer term.Restore(int(os.Stdin.Fd()), oldState)

		// Attach to the session
		stream, err := client.Attach(ctx)
		if err != nil {
			term.Restore(int(os.Stdin.Fd()), oldState)
			cli.Error("Cannot attach: %v", err)
			cli.ExitError()
		}

		// Send initial message with session ID
		if err := stream.Send(&cliv0.TerminalInput{SessionId: sessionID}); err != nil {
			term.Restore(int(os.Stdin.Fd()), oldState)
			cli.Error("Cannot send session ID: %v", err)
			cli.ExitError()
		}

		// Handle SIGWINCH (terminal resize)
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGWINCH)
		go func() {
			for range sigCh {
				w, h, err := term.GetSize(int(os.Stdin.Fd()))
				if err == nil {
					_, _ = client.Resize(ctx, &cliv0.ResizeTerminalRequest{
						SessionId: sessionID,
						Rows:      uint32(h),
						Cols:      uint32(w),
					})
				}
			}
		}()

		done := make(chan struct{})

		// Read from server, write to stdout
		go func() {
			defer close(done)
			for {
				msg, err := stream.Recv()
				if err != nil {
					return
				}
				if len(msg.Data) > 0 {
					os.Stdout.Write(msg.Data)
				}
				if msg.Done {
					return
				}
			}
		}()

		// Read from stdin, send to server
		go func() {
			buf := make([]byte, 1024)
			for {
				n, err := os.Stdin.Read(buf)
				if n > 0 {
					if sendErr := stream.Send(&cliv0.TerminalInput{
						SessionId: sessionID,
						Data:      buf[:n],
					}); sendErr != nil {
						return
					}
				}
				if err != nil {
					return
				}
			}
		}()

		<-done
	},
}

func init() {
	TerminalCmd.Flags().StringVar(&termModule, "module", "", "Module name (optional)")
	TerminalCmd.Flags().StringVar(&termService, "service", "", "Service name (optional)")
	TerminalCmd.Flags().StringVar(&termShell, "shell", "", "Shell override (default: $SHELL)")
	TerminalCmd.Flags().StringVar(&termServer, "server", "localhost:10000", "codefly gRPC server address")
}
