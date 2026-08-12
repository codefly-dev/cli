package gateway

// terminal.go — PTY-backed interactive terminals hosted IN the gateway.
//
// The gateway runs inside the execution box (the per-session pod / local
// workspace), so it's the right place for the shell: it has the worktree and
// the ability to exec. Mind never runs in the box; it PROXIES these RPCs to
// give the user an interactive terminal in the session's working directory.
//
// Reuses the proven creack/pty approach from cli/pkg/web/go-grpc/terminal.go.
// The gateway owns the live PTY; Mind owns the scrollback/session state and the
// browser-safe (server-stream + unary) shape its clients consume.

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

	gatewayv1 "github.com/codefly-dev/core/generated/go/mind/gateway/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type termSession struct {
	id         string
	ptmx       *os.File
	cmd        *exec.Cmd
	shell      string
	workingDir string
	createdAt  time.Time
	done       chan struct{}
	exitCode   int
}

type terminalManager struct {
	mu       sync.RWMutex
	sessions map[string]*termSession
	closed   bool
}

func newTerminalManager() *terminalManager {
	return &terminalManager{sessions: make(map[string]*termSession)}
}

func (m *terminalManager) get(id string) (*termSession, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s, ok := m.sessions[id]
	return s, ok
}

func (m *terminalManager) add(session *termSession) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return fmt.Errorf("terminal manager is closed")
	}
	m.sessions[session.id] = session
	return nil
}

func (m *terminalManager) take(id string) (*termSession, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	session, ok := m.sessions[id]
	if ok {
		delete(m.sessions, id)
	}
	return session, ok
}

func (m *terminalManager) remove(id string) {
	m.mu.Lock()
	delete(m.sessions, id)
	m.mu.Unlock()
}

func (m *terminalManager) close() {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	m.closed = true
	sessions := make([]*termSession, 0, len(m.sessions))
	for _, session := range m.sessions {
		sessions = append(sessions, session)
	}
	m.sessions = make(map[string]*termSession)
	m.mu.Unlock()

	for _, session := range sessions {
		if session.cmd.Process != nil {
			_ = session.cmd.Process.Kill()
		}
		_ = session.ptmx.Close()
	}
	for _, session := range sessions {
		<-session.done
	}
}

// OpenTerminal spawns a PTY-backed shell in the gateway's working directory.
func (s *Server) OpenTerminal(_ context.Context, req *gatewayv1.OpenTerminalRequest) (*gatewayv1.OpenTerminalResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "terminal request is required")
	}
	if err := ValidateUnstructuredUse(req.GetUnstructuredUse()); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	shell := req.Shell
	if shell == "" {
		shell = os.Getenv("SHELL")
		if shell == "" {
			shell = "/bin/sh"
		}
	}
	workDir := s.serviceRoot()
	if req.WorkingDir != "" {
		var err error
		workDir, err = s.resolveCommandDir(req.WorkingDir)
		if err != nil {
			return nil, fmt.Errorf("resolve terminal working directory: %w", err)
		}
	}

	cmd := exec.Command(shell)
	cmd.Dir = workDir
	cmd.Env = append(os.Environ(), "TERM=xterm-256color")

	ptmx, err := pty.Start(cmd)
	if err != nil {
		return nil, fmt.Errorf("start pty: %w", err)
	}
	if req.Rows > 0 && req.Cols > 0 {
		_ = pty.Setsize(ptmx, &pty.Winsize{Rows: uint16(req.Rows), Cols: uint16(req.Cols)})
	}

	id := uuid.New().String()[:8]
	sess := &termSession{
		id:         id,
		ptmx:       ptmx,
		cmd:        cmd,
		shell:      shell,
		workingDir: workDir,
		createdAt:  time.Now(),
		done:       make(chan struct{}),
	}
	if err := s.terminals.add(sess); err != nil {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		_ = ptmx.Close()
		_ = cmd.Wait()
		close(sess.done)
		return nil, err
	}
	go func() {
		_ = cmd.Wait()
		sess.exitCode = cmd.ProcessState.ExitCode()
		_ = ptmx.Close()
		close(sess.done)
		s.terminals.remove(id)
	}()

	return &gatewayv1.OpenTerminalResponse{TerminalId: id, Shell: shell, WorkingDir: workDir}, nil
}

// AttachTerminal bidirectionally streams PTY I/O: the first inbound message
// selects the terminal; thereafter input bytes go to the PTY and output bytes
// stream back until the shell exits or the client detaches.
func (s *Server) AttachTerminal(stream gatewayv1.Gateway_AttachTerminalServer) error {
	first, err := stream.Recv()
	if err != nil {
		return fmt.Errorf("read first message: %w", err)
	}
	sess, ok := s.terminals.get(first.TerminalId)
	if !ok {
		return fmt.Errorf("terminal not found: %s", first.TerminalId)
	}
	if len(first.Data) > 0 {
		_, _ = sess.ptmx.Write(first.Data)
	}

	ctx := stream.Context()
	errCh := make(chan error, 2)

	// PTY → client.
	go func() {
		buf := make([]byte, 4096)
		for {
			n, rerr := sess.ptmx.Read(buf)
			if n > 0 {
				if serr := stream.Send(&gatewayv1.TerminalOutput{TerminalId: sess.id, Data: buf[:n]}); serr != nil {
					errCh <- serr
					return
				}
			}
			if rerr != nil {
				_ = stream.Send(&gatewayv1.TerminalOutput{TerminalId: sess.id, Done: true, ExitCode: int32(sess.exitCode)})
				errCh <- rerr
				return
			}
		}
	}()

	// Client → PTY.
	go func() {
		for {
			msg, rerr := stream.Recv()
			if rerr != nil {
				errCh <- rerr
				return
			}
			if len(msg.Data) > 0 {
				_, _ = sess.ptmx.Write(msg.Data)
			}
		}
	}()

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

func (s *Server) ResizeTerminal(_ context.Context, req *gatewayv1.ResizeTerminalRequest) (*gatewayv1.ResizeTerminalResponse, error) {
	sess, ok := s.terminals.get(req.TerminalId)
	if !ok {
		return nil, fmt.Errorf("terminal not found: %s", req.TerminalId)
	}
	if err := pty.Setsize(sess.ptmx, &pty.Winsize{Rows: uint16(req.Rows), Cols: uint16(req.Cols)}); err != nil {
		return nil, fmt.Errorf("resize: %w", err)
	}
	return &gatewayv1.ResizeTerminalResponse{}, nil
}

func (s *Server) CloseTerminal(ctx context.Context, req *gatewayv1.CloseTerminalRequest) (*gatewayv1.CloseTerminalResponse, error) {
	sess, ok := s.terminals.take(req.TerminalId)
	if !ok {
		return nil, fmt.Errorf("terminal not found: %s", req.TerminalId)
	}
	if sess.cmd.Process != nil {
		_ = sess.cmd.Process.Kill()
	}
	_ = sess.ptmx.Close()
	select {
	case <-sess.done:
		return &gatewayv1.CloseTerminalResponse{}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (s *Server) ListTerminals(_ context.Context, _ *gatewayv1.ListTerminalsRequest) (*gatewayv1.ListTerminalsResponse, error) {
	s.terminals.mu.RLock()
	defer s.terminals.mu.RUnlock()
	resp := &gatewayv1.ListTerminalsResponse{}
	for _, sess := range s.terminals.sessions {
		status := "running"
		select {
		case <-sess.done:
			status = "exited"
		default:
		}
		resp.Terminals = append(resp.Terminals, &gatewayv1.TerminalInfo{
			TerminalId: sess.id,
			Shell:      sess.shell,
			WorkingDir: sess.workingDir,
			Status:     status,
		})
	}
	return resp, nil
}
