package go_grpc

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/creack/pty"
	"github.com/google/uuid"

	cliv0 "github.com/codefly-dev/core/generated/go/codefly/cli/v0"
)

// TerminalServer implements TerminalServiceServer.
type TerminalServer struct {
	cliv0.UnimplementedTerminalServiceServer

	mu       sync.RWMutex
	sessions map[string]*termSession

	// workspaceDir is the root workspace directory for resolving module/service paths
	workspaceDir string
}

type termSession struct {
	ID         string
	PTY        *os.File
	Cmd        *exec.Cmd
	Shell      string
	WorkingDir string
	Module     string
	Service    string
	CreatedAt  time.Time
	done       chan struct{}
}

// NewTerminalServer creates a new terminal server.
func NewTerminalServer(workspaceDir string) *TerminalServer {
	return &TerminalServer{
		sessions:     make(map[string]*termSession),
		workspaceDir: workspaceDir,
	}
}

func (s *TerminalServer) Open(_ context.Context, req *cliv0.OpenTerminalRequest) (*cliv0.OpenTerminalResponse, error) {
	shell := req.Shell
	if shell == "" {
		shell = os.Getenv("SHELL")
		if shell == "" {
			shell = "/bin/sh"
		}
	}

	// Resolve working directory
	workDir := s.workspaceDir
	if req.Module != "" {
		workDir = fmt.Sprintf("%s/modules/%s", s.workspaceDir, req.Module)
		if req.Service != "" {
			workDir = fmt.Sprintf("%s/services/%s", workDir, req.Service)
		}
	}

	// Verify directory exists
	if _, err := os.Stat(workDir); err != nil {
		return nil, fmt.Errorf("working directory not found: %s", workDir)
	}

	// Create command
	cmd := exec.Command(shell)
	cmd.Dir = workDir
	cmd.Env = os.Environ()

	// Start with PTY
	ptmx, err := pty.Start(cmd)
	if err != nil {
		return nil, fmt.Errorf("cannot start PTY: %w", err)
	}

	// Set initial size
	if req.Rows > 0 && req.Cols > 0 {
		_ = pty.Setsize(ptmx, &pty.Winsize{
			Rows: uint16(req.Rows),
			Cols: uint16(req.Cols),
		})
	}

	sessionID := uuid.New().String()[:8]
	sess := &termSession{
		ID:         sessionID,
		PTY:        ptmx,
		Cmd:        cmd,
		Shell:      shell,
		WorkingDir: workDir,
		Module:     req.Module,
		Service:    req.Service,
		CreatedAt:  time.Now(),
		done:       make(chan struct{}),
	}

	// Monitor process exit
	go func() {
		_ = cmd.Wait()
		close(sess.done)
	}()

	s.mu.Lock()
	s.sessions[sessionID] = sess
	s.mu.Unlock()

	return &cliv0.OpenTerminalResponse{
		SessionId:  sessionID,
		Shell:      shell,
		WorkingDir: workDir,
	}, nil
}

func (s *TerminalServer) Attach(stream cliv0.TerminalService_AttachServer) error {
	// Read first message to get session_id
	firstMsg, err := stream.Recv()
	if err != nil {
		return fmt.Errorf("cannot read first message: %w", err)
	}

	s.mu.RLock()
	sess, ok := s.sessions[firstMsg.SessionId]
	s.mu.RUnlock()
	if !ok {
		return fmt.Errorf("session not found: %s", firstMsg.SessionId)
	}

	// Send first input if any
	if len(firstMsg.Data) > 0 {
		_, _ = sess.PTY.Write(firstMsg.Data)
	}

	ctx := stream.Context()
	errCh := make(chan error, 2)

	// PTY → client (read from PTY, send to stream)
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := sess.PTY.Read(buf)
			if n > 0 {
				if sendErr := stream.Send(&cliv0.TerminalOutput{
					SessionId: sess.ID,
					Data:      buf[:n],
				}); sendErr != nil {
					errCh <- sendErr
					return
				}
			}
			if err != nil {
				// PTY closed — send done signal
				_ = stream.Send(&cliv0.TerminalOutput{
					SessionId: sess.ID,
					Done:      true,
					ExitCode:  int32(exitCode(sess.Cmd)),
				})
				errCh <- err
				return
			}
		}
	}()

	// Client → PTY (read from stream, write to PTY)
	go func() {
		for {
			msg, err := stream.Recv()
			if err != nil {
				errCh <- err
				return
			}
			if len(msg.Data) > 0 {
				_, _ = sess.PTY.Write(msg.Data)
			}
		}
	}()

	// Wait for either direction to finish or context to cancel
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-sess.done:
		return nil
	case err := <-errCh:
		if err == io.EOF {
			return nil
		}
		return err
	}
}

func (s *TerminalServer) Resize(_ context.Context, req *cliv0.ResizeTerminalRequest) (*cliv0.ResizeTerminalResponse, error) {
	s.mu.RLock()
	sess, ok := s.sessions[req.SessionId]
	s.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("session not found: %s", req.SessionId)
	}

	if err := pty.Setsize(sess.PTY, &pty.Winsize{
		Rows: uint16(req.Rows),
		Cols: uint16(req.Cols),
	}); err != nil {
		return nil, fmt.Errorf("cannot resize: %w", err)
	}

	return &cliv0.ResizeTerminalResponse{}, nil
}

func (s *TerminalServer) Close(_ context.Context, req *cliv0.CloseTerminalRequest) (*cliv0.CloseTerminalResponse, error) {
	s.mu.Lock()
	sess, ok := s.sessions[req.SessionId]
	if ok {
		delete(s.sessions, req.SessionId)
	}
	s.mu.Unlock()

	if !ok {
		return nil, fmt.Errorf("session not found: %s", req.SessionId)
	}

	// Signal the process to exit
	if sess.Cmd.Process != nil {
		_ = sess.Cmd.Process.Signal(os.Interrupt)
		time.AfterFunc(3*time.Second, func() {
			if sess.Cmd.ProcessState == nil || !sess.Cmd.ProcessState.Exited() {
				_ = sess.Cmd.Process.Kill()
			}
		})
	}
	_ = sess.PTY.Close()

	return &cliv0.CloseTerminalResponse{}, nil
}

func (s *TerminalServer) List(_ context.Context, _ *cliv0.ListTerminalsRequest) (*cliv0.ListTerminalsResponse, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var terminals []*cliv0.TerminalInfo
	for _, sess := range s.sessions {
		terminals = append(terminals, &cliv0.TerminalInfo{
			SessionId:  sess.ID,
			Shell:      sess.Shell,
			WorkingDir: sess.WorkingDir,
			Module:     sess.Module,
			Service:    sess.Service,
		})
	}
	return &cliv0.ListTerminalsResponse{Terminals: terminals}, nil
}

// Cleanup removes all sessions where the process has exited.
func (s *TerminalServer) Cleanup() {
	s.mu.Lock()
	defer s.mu.Unlock()

	for id, sess := range s.sessions {
		select {
		case <-sess.done:
			_ = sess.PTY.Close()
			delete(s.sessions, id)
		default:
		}
	}
}

// Shutdown closes all sessions.
func (s *TerminalServer) Shutdown() {
	s.mu.Lock()
	defer s.mu.Unlock()

	for id, sess := range s.sessions {
		if sess.Cmd.Process != nil {
			_ = sess.Cmd.Process.Kill()
		}
		_ = sess.PTY.Close()
		delete(s.sessions, id)
	}
}

func exitCode(cmd *exec.Cmd) int {
	if cmd.ProcessState != nil {
		return cmd.ProcessState.ExitCode()
	}
	return -1
}
