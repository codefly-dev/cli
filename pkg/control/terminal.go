package control

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"sync"

	"github.com/creack/pty"
	"github.com/google/uuid"
)

// This file implements the TerminalController group with a plane-owned local PTY
// manager. Terminals are a generic, local operation (no plugin) — the plane runs
// a shell in the target directory via creack/pty, exactly like the web/gateway
// terminal servers, minus the gRPC coupling. Sessions live for the lifetime of
// the plane instance.

type terminalManager struct {
	mu       sync.RWMutex
	sessions map[string]*termSession
}

type termSession struct {
	ptyFile *os.File
	cmd     *exec.Cmd
}

func newTerminalManager() *terminalManager {
	return &terminalManager{sessions: make(map[string]*termSession)}
}

func (m *terminalManager) get(id TerminalID) (*termSession, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s, ok := m.sessions[string(id)]
	return s, ok
}

// terminalDir resolves where a session's shell starts: the service directory
// when a service is named, otherwise the workspace root.
func (p *planeImpl) terminalDir(ctx context.Context, req OpenTerminalRequest) (string, error) {
	if req.Service != "" {
		name := req.Service
		if req.Module != "" {
			name = req.Module + "/" + req.Service
		}
		_, _, service, err := p.loadTarget(ctx, name)
		if err != nil {
			return "", err
		}
		return service.Dir(), nil
	}
	ws, err := p.workspace(ctx)
	if err != nil {
		return "", err
	}
	return ws.Dir(), nil
}

// OpenTerminal starts a shell PTY scoped to the target and returns its id.
func (p *planeImpl) OpenTerminal(ctx context.Context, req OpenTerminalRequest) (TerminalID, error) {
	shell := req.Shell
	if shell == "" {
		if shell = os.Getenv("SHELL"); shell == "" {
			shell = "/bin/bash"
		}
	}
	dir, err := p.terminalDir(ctx, req)
	if err != nil {
		return "", err
	}
	cmd := exec.Command(shell)
	cmd.Dir = dir
	f, err := pty.Start(cmd)
	if err != nil {
		return "", fmt.Errorf("start terminal: %w", err)
	}
	id := TerminalID(uuid.New().String())
	p.terminals.mu.Lock()
	p.terminals.sessions[string(id)] = &termSession{ptyFile: f, cmd: cmd}
	p.terminals.mu.Unlock()
	return id, nil
}

// AttachTerminal streams the session's output to onOutput (in a goroutine that
// ends when the PTY closes or onOutput errors) and returns a writer for input.
func (p *planeImpl) AttachTerminal(ctx context.Context, id TerminalID, onOutput func([]byte) error) (TerminalInput, error) {
	sess, ok := p.terminals.get(id)
	if !ok {
		return nil, fmt.Errorf("unknown terminal %s", id)
	}
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := sess.ptyFile.Read(buf)
			if n > 0 {
				if emitErr := onOutput(append([]byte(nil), buf[:n]...)); emitErr != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()
	return &terminalInput{pty: sess.ptyFile}, nil
}

func (p *planeImpl) ResizeTerminal(ctx context.Context, id TerminalID, cols, rows int) error {
	sess, ok := p.terminals.get(id)
	if !ok {
		return fmt.Errorf("unknown terminal %s", id)
	}
	return pty.Setsize(sess.ptyFile, &pty.Winsize{Rows: uint16(rows), Cols: uint16(cols)})
}

func (p *planeImpl) CloseTerminal(ctx context.Context, id TerminalID) error {
	p.terminals.mu.Lock()
	sess, ok := p.terminals.sessions[string(id)]
	if ok {
		delete(p.terminals.sessions, string(id))
	}
	p.terminals.mu.Unlock()
	if !ok {
		return fmt.Errorf("unknown terminal %s", id)
	}
	_ = sess.ptyFile.Close()
	if sess.cmd.Process != nil {
		_ = sess.cmd.Process.Kill()
	}
	return nil
}

func (p *planeImpl) ListTerminals(ctx context.Context) ([]TerminalID, error) {
	p.terminals.mu.RLock()
	defer p.terminals.mu.RUnlock()
	ids := make([]TerminalID, 0, len(p.terminals.sessions))
	for id := range p.terminals.sessions {
		ids = append(ids, TerminalID(id))
	}
	return ids, nil
}

// terminalInput writes bytes to a session's PTY. Close is a no-op — the session
// is torn down via CloseTerminal, not by closing the input handle.
type terminalInput struct {
	pty *os.File
}

func (t *terminalInput) Write(p []byte) (int, error) { return t.pty.Write(p) }
func (t *terminalInput) Close() error                { return nil }
