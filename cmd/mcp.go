package cmd

import (
	"context"

	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/cli/pkg/mcp"
	"github.com/codefly-dev/core/wool"
	"github.com/spf13/cobra"
)

// MCPCmd represents the mcp command group
var MCPCmd = &cobra.Command{
	Use:   "mcp",
	Short: "MCP server for AI integration",
	Long: `Model Context Protocol (MCP) server for codefly.

This allows AI assistants like Claude to interact with your codefly workspace,
query services, and perform development operations.

Usage with Claude Desktop:
  Add to ~/.claude/claude_desktop_config.json:
  {
    "mcpServers": {
      "codefly": {
        "command": "codefly",
        "args": ["mcp", "serve"]
      }
    }
  }`,
}

// MCPServeCmd starts the MCP server
var MCPServeCmd = &cobra.Command{
	Use:   "serve",
	Short: "Start MCP server in stdio mode",
	Long: `Start the MCP server in stdio mode for integration with AI assistants.

The server communicates via JSON-RPC 2.0 over stdin/stdout, following the
Model Context Protocol specification.`,
	Run: func(cmd *cobra.Command, args []string) {
		ctx := context.Background()
		w := wool.Get(ctx).In("mcp.serve")

		// Get version from CLI
		version, err := cli.GetCurrentVersion()
		if err != nil {
			version = "unknown"
		}

		server, err := mcp.NewServer(ctx, version)
		if err != nil {
			w.Error("failed to create MCP server", wool.ErrField(err))
			cli.ExitOnError(err, "failed to create MCP server")
			return
		}

		w.Info("Starting codefly MCP server", wool.Field("version", version))

		err = server.Serve(ctx)
		if err != nil {
			w.Error("MCP server error", wool.ErrField(err))
			cli.ExitOnError(err, "MCP server error")
		}
	},
}

// MCPToolsCmd lists available MCP tools
var MCPToolsCmd = &cobra.Command{
	Use:   "tools",
	Short: "List available MCP tools",
	Run: func(cmd *cobra.Command, args []string) {
		ctx := context.Background()

		version, _ := cli.GetCurrentVersion()

		server, err := mcp.NewServer(ctx, version)
		if err != nil {
			cli.ExitOnError(err, "failed to create MCP server")
			return
		}

		tools := server.ListTools()
		for _, tool := range tools {
			cli.Info("%s - %s", tool.Name, tool.Description)
		}
	},
}

func init() {
	MCPCmd.AddCommand(MCPServeCmd)
	MCPCmd.AddCommand(MCPToolsCmd)
}
