package cmd

import (
	"github.com/codefly-dev/cli/cmd/companion"
	"github.com/spf13/cobra"
)

// CompanionCmd is the parent of `codefly companion build/push/list`,
// the host CLI's companion-management surface. Replaces the bash
// scripts under core/companions/scripts/ + core/companions/<name>/
// scripts/ with a typed Go implementation.
//
// Companions are codefly-owned sidecar images (proto compilation, Go
// toolchain, Python toolchain, etc.) that codefly invokes during
// generate/build/run flows. They differ from agents (which run as
// long-lived plugin processes) and from toolboxes (narrow MCP-shape
// permissions surfaces) — companions are short-lived, single-purpose
// docker invocations.
var CompanionCmd = &cobra.Command{
	Use:   "companion",
	Short: "Build and publish companion images used by Codefly toolchains",
}

func init() {
	CompanionCmd.AddCommand(companion.BuildCmd)
	CompanionCmd.AddCommand(companion.PushCmd)
	CompanionCmd.AddCommand(companion.ListCmd)
}
