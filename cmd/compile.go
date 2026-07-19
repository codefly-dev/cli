package cmd

import (
	"github.com/codefly-dev/cli/cmd/validation"
	"github.com/spf13/cobra"
)

// CompileCmd exposes plugin-owned native compilation/typechecking outside CI.
var CompileCmd = &cobra.Command{
	Use:   "compile",
	Short: "Run plugin-owned native compilation or typechecking",
}

func init() {
	CompileCmd.AddCommand(validation.CompileServiceCmd)
}
