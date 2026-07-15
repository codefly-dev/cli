package cmd

import "testing"

func TestServerAndMCPCommandsReturnErrors(t *testing.T) {
	commands := map[string]struct {
		run  bool
		runE bool
		args func([]string) error
	}{
		"server":    {run: ServerCmd.Run != nil, runE: ServerCmd.RunE != nil, args: func(args []string) error { return ServerCmd.Args(ServerCmd, args) }},
		"mcp serve": {run: MCPServeCmd.Run != nil, runE: MCPServeCmd.RunE != nil, args: func(args []string) error { return MCPServeCmd.Args(MCPServeCmd, args) }},
		"mcp tools": {run: MCPToolsCmd.Run != nil, runE: MCPToolsCmd.RunE != nil, args: func(args []string) error { return MCPToolsCmd.Args(MCPToolsCmd, args) }},
	}
	for name, command := range commands {
		if command.run || !command.runE {
			t.Fatalf("%s must return errors through RunE", name)
		}
		if err := command.args([]string{"extra"}); err == nil {
			t.Fatalf("%s accepted positional arguments", name)
		}
	}
}
