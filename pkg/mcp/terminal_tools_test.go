package mcp

import (
	"fmt"
	"testing"

	"github.com/codefly-dev/core/network"
)

func TestTerminalAddressUsesWorkspacePort(t *testing.T) {
	workspace := "payments"
	want := fmt.Sprintf("127.0.0.1:%d", network.CLIServerPort(workspace))
	if got := terminalAddress(workspace); got != want {
		t.Fatalf("terminalAddress(%q) = %q, want %q", workspace, got, want)
	}
}
