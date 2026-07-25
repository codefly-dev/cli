package control

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestTerminalOpenAttachWriteClose(t *testing.T) {
	t.Chdir(writeWorkspace(t))
	ctx := context.Background()
	p := New()
	t.Cleanup(func() { _ = p.Close() })

	id, err := p.OpenTerminal(ctx, OpenTerminalRequest{Shell: "/bin/sh"})
	if err != nil {
		t.Fatal(err)
	}
	// Ensure the shell + reader goroutine are torn down even if an assertion
	// below fails mid-test.
	t.Cleanup(func() { _ = p.CloseTerminal(context.Background(), id) })
	if ids, _ := p.ListTerminals(ctx); len(ids) != 1 {
		t.Fatalf("ListTerminals = %v, want 1 session", ids)
	}

	out := make(chan []byte, 256)
	input, err := p.AttachTerminal(ctx, id, func(b []byte) error {
		out <- append([]byte(nil), b...)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := input.Write([]byte("echo control_plane_ok\n")); err != nil {
		t.Fatal(err)
	}

	var buf strings.Builder
	deadline := time.After(5 * time.Second)
	for !strings.Contains(buf.String(), "control_plane_ok") {
		select {
		case b := <-out:
			buf.Write(b)
		case <-deadline:
			t.Fatalf("did not see terminal echo output; got: %q", buf.String())
		}
	}

	if err := p.CloseTerminal(ctx, id); err != nil {
		t.Fatal(err)
	}
	if ids, _ := p.ListTerminals(ctx); len(ids) != 0 {
		t.Errorf("ListTerminals after close = %v, want empty", ids)
	}
}

func TestTerminalUnknownID(t *testing.T) {
	ctx := context.Background()
	p := New()
	t.Cleanup(func() { _ = p.Close() })
	if err := p.CloseTerminal(ctx, "does-not-exist"); err == nil {
		t.Error("closing an unknown terminal should error")
	}
	if err := p.ResizeTerminal(ctx, "does-not-exist", 80, 24); err == nil {
		t.Error("resizing an unknown terminal should error")
	}
}

func TestPlaneCloseReapsTerminalsAndRejectsNewSessions(t *testing.T) {
	t.Chdir(writeWorkspace(t))
	p := New()
	id, err := p.OpenTerminal(context.Background(), OpenTerminalRequest{Shell: "/bin/sh"})
	if err != nil {
		t.Fatal(err)
	}
	impl := p.(*planeImpl)
	session, ok := impl.terminals.get(id)
	if !ok {
		t.Fatal("opened terminal was not registered")
	}

	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-session.done:
	case <-time.After(5 * time.Second):
		t.Fatal("terminal process was not reaped by plane close")
	}
	if err := p.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if _, err := p.OpenTerminal(context.Background(), OpenTerminalRequest{Shell: "/bin/sh"}); err == nil {
		t.Fatal("terminal opened after plane close")
	}
}
