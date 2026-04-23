package cmd

import (
	"github.com/codefly-dev/cli/cmd/audit"
	"github.com/spf13/cobra"
)

// AuditCmd is the parent for `codefly audit service` and
// `codefly audit workspace`.
var AuditCmd = &cobra.Command{
	Use:   "audit",
	Short: "Audit dependencies for CVEs and outdated releases",
}

func init() {
	AuditCmd.AddCommand(audit.ServiceCmd)
	AuditCmd.AddCommand(audit.WorkspaceCmd)
}
