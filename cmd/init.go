package cmd

import (
	"github.com/spf13/cobra"
)

// InitCmd represents the install command
var InitCmd = &cobra.Command{
	Use:   "init",
	Short: "Initialize codefly",
}
