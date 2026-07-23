package cmd

import (
	"github.com/codefly-dev/cli/cmd/audit"
	"github.com/spf13/cobra"
)

// AuditCmd is the parent for `codefly audit service` and
// `codefly audit workspace`.
var AuditCmd = &cobra.Command{
	Use:   "audit",
	Short: "Find vulnerable and outdated dependencies in services or workspaces",
}

func init() {
	AuditCmd.AddCommand(audit.ServiceCmd)
	AuditCmd.AddCommand(audit.WorkspaceCmd)
	AuditCmd.AddCommand(audit.GoCmd)
}
