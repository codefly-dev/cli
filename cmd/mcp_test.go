package cmd

import (
	"strings"
	"testing"
)

func TestMCPCmdHelpUsesCorrectClaudeDesktopPath(t *testing.T) {
	if strings.Contains(MCPCmd.Long, "~/.claude/claude_desktop_config.json") {
		t.Error("MCPCmd help still points at the stale ~/.claude path; Claude Desktop reads the Application Support location, so that file is never loaded")
	}
	if !strings.Contains(MCPCmd.Long, "~/Library/Application Support/Claude/claude_desktop_config.json") {
		t.Error("MCPCmd help is missing the correct macOS Claude Desktop config path")
	}
}
