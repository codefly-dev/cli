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
// manager using creack/pty. Terminals are a generic, local operation (no plugin)
// — the plane runs a shell in the target directory, like the web/gateway
// terminal servers, minus the gRPC coupling.
//
// Lifecycle mirrors those siblings: each session has a reaper goroutine that
// cmd.Wait()s the shell (so a self-exited shell is reaped, not left a zombie)
// and removes the session from the map. Attach honors its context so an
// abandoned reader unwinds instead of leaking.

type terminalManager struct {
	mu       sync.Mutex
	sessions map[string]*termSession
}

type termSession struct {
	ptyFile  *os.File
	cmd      *exec.Cmd
	done     chan struct{} // closed by the reaper when the shell exits
	attached bool          // guards against a second competing reader
}

func newTerminalManager() *terminalManager {
	return &terminalManager{sessions: make(map[string]*termSession)}
}

func (m *terminalManager) get(id TerminalID) (*termSession, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[string(id)]
	return s, ok
}

func (m *terminalManager) remove(id string) {
	m.mu.Lock()
	delete(m.sessions, id)
	m.mu.Unlock()
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

// OpenTerminal starts a shell PTY scoped to the target and returns its id. A
// reaper goroutine waits on the shell and self-removes the session when it
// exits, so sessions do not accumulate and the child is never left a zombie.
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
	sess := &termSession{ptyFile: f, cmd: cmd, done: make(chan struct{})}
	p.terminals.mu.Lock()
	p.terminals.sessions[string(id)] = sess
	p.terminals.mu.Unlock()

	// Reaper: wait the shell, close its PTY, drop the session.
	go func() {
		_ = cmd.Wait()
		_ = f.Close()
		close(sess.done)
		p.terminals.remove(string(id))
	}()
	return id, nil
}

// AttachTerminal streams the session's output to onOutput until ctx is done, the
// session exits, or onOutput errors. Only one reader may attach at a time.
func (p *planeImpl) AttachTerminal(ctx context.Context, id TerminalID, onOutput func([]byte) error) (TerminalInput, error) {
	p.terminals.mu.Lock()
	sess, ok := p.terminals.sessions[string(id)]
	if ok && sess.attached {
		p.terminals.mu.Unlock()
		return nil, fmt.Errorf("terminal %s already has a reader attached", id)
	}
	if ok {
		sess.attached = true
	}
	p.terminals.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("unknown terminal %s", id)
	}

	// A watcher closes the PTY when ctx is cancelled, which unblocks the read
	// loop below (pty.Read returns an error) — so an abandoned attach unwinds
	// instead of leaking. It also exits when the session's reaper fires.
	go func() {
		select {
		case <-ctx.Done():
			_ = sess.ptyFile.Close()
		case <-sess.done:
		}
	}()

	go func() {
		defer func() {
			p.terminals.mu.Lock()
			sess.attached = false
			p.terminals.mu.Unlock()
		}()
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
	return pty.Setsize(sess.ptyFile, &pty.Winsize{Rows: clampWindow(rows), Cols: clampWindow(cols)})
}

// clampWindow keeps a terminal dimension in the uint16 range pty.Winsize uses,
// so a negative or oversized value can't silently wrap.
func clampWindow(v int) uint16 {
	if v < 1 {
		return 1
	}
	if v > 65535 {
		return 65535
	}
	return uint16(v)
}

func (p *planeImpl) CloseTerminal(ctx context.Context, id TerminalID) error {
	sess, ok := p.terminals.get(id)
	if !ok {
		return fmt.Errorf("unknown terminal %s", id)
	}
	// Killing the shell makes the reaper (from OpenTerminal) fire, which closes
	// the PTY and removes the session — so cleanup happens in exactly one place.
	if sess.cmd.Process != nil {
		_ = sess.cmd.Process.Kill()
	} else {
		_ = sess.ptyFile.Close()
	}
	// Block until the reaper has finished teardown so the session is gone (and
	// its child reaped) by the time Close returns.
	select {
	case <-sess.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (p *planeImpl) ListTerminals(ctx context.Context) ([]TerminalID, error) {
	p.terminals.mu.Lock()
	defer p.terminals.mu.Unlock()
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
